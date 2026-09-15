package cmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/provider"
	"zenget/internal/state"
)

type selfUpgradeTestProvider struct {
	release      *provider.Release
	body         []byte
	latestCalls  int
	downloads    int
	downloadErr  error
	latestErr    error
	downloadSeen provider.Asset
}

func (p *selfUpgradeTestProvider) LatestRelease(context.Context, string, string) (*provider.Release, error) {
	p.latestCalls++
	if p.latestErr != nil {
		return nil, p.latestErr
	}
	return p.release, nil
}

func (*selfUpgradeTestProvider) ReleaseByTag(context.Context, string, string, string) (*provider.Release, error) {
	return nil, fmt.Errorf("ReleaseByTag should not be called")
}

func (*selfUpgradeTestProvider) ListReleases(context.Context, string, string) ([]provider.Release, error) {
	return nil, fmt.Errorf("ListReleases should not be called")
}

func (p *selfUpgradeTestProvider) DownloadAsset(_ context.Context, asset provider.Asset, destination string, _ ...int64) error {
	p.downloads++
	p.downloadSeen = asset
	if p.downloadErr != nil {
		return p.downloadErr
	}
	return os.WriteFile(destination, p.body, 0600)
}

func TestRunSelfUpgradeReplacesResolvedExecutableAndVerifiesChecksum(t *testing.T) {
	realPath, executablePath := newSelfUpgradeExecutable(t, []byte("old zenget"))
	body := []byte("new zenget")
	digest := sha256.Sum256(body)
	providerClient := &selfUpgradeTestProvider{
		release: &provider.Release{
			TagName: "v0.0.2",
			Assets: []provider.Asset{{
				Name:   "zenget-linux-amd64",
				Digest: fmt.Sprintf("sha256:%x", digest),
			}},
		},
		body: body,
	}
	var output strings.Builder

	err := runSelfUpgradeWithOptions(context.Background(), providerClient, selfUpgradeOptions{
		repository:   "tyutyutyu/zenget",
		goos:         "linux",
		goarch:       "amd64",
		executable:   func() (string, error) { return executablePath, nil },
		evalSymlinks: filepath.EvalSymlinks,
	}, &output)
	if err != nil {
		t.Fatalf("runSelfUpgradeWithOptions() error = %v", err)
	}
	if got, err := os.ReadFile(realPath); err != nil {
		t.Fatalf("read replaced executable: %v", err)
	} else if string(got) != string(body) {
		t.Fatalf("replaced executable = %q, want %q", got, body)
	}
	if providerClient.latestCalls != 1 || providerClient.downloads != 1 {
		t.Fatalf("provider calls = latest %d/download %d, want 1/1", providerClient.latestCalls, providerClient.downloads)
	}
	if !strings.Contains(output.String(), "Verified SHA-256 checksum") || !strings.Contains(output.String(), "Upgraded zenget") {
		t.Fatalf("output = %q, want verification and upgrade messages", output.String())
	}
	if info, err := os.Stat(realPath); err != nil {
		t.Fatalf("stat replaced executable: %v", err)
	} else if info.Mode().Perm() != 0755 {
		t.Fatalf("replaced executable permissions = %o, want 755", info.Mode().Perm())
	}
	if resolved, err := filepath.EvalSymlinks(executablePath); err != nil {
		t.Fatalf("resolve executable symlink: %v", err)
	} else if resolved != realPath {
		t.Fatalf("resolved executable = %q, want %q", resolved, realPath)
	}
}

func TestRunSelfUpgradeCurrentVersionIsReadOnly(t *testing.T) {
	realPath, executablePath := newSelfUpgradeExecutable(t, []byte("old zenget"))
	providerClient := &selfUpgradeTestProvider{release: &provider.Release{TagName: "0.0.1"}}
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	statePath := filepath.Join(configHome, "zenget", "state.json")
	var output strings.Builder

	err := runSelfUpgradeWithOptions(context.Background(), providerClient, selfUpgradeOptions{
		repository:   "tyutyutyu/zenget",
		goos:         "linux",
		goarch:       "amd64",
		executable:   func() (string, error) { return executablePath, nil },
		evalSymlinks: filepath.EvalSymlinks,
	}, &output)
	if err != nil {
		t.Fatalf("runSelfUpgradeWithOptions() error = %v", err)
	}
	if providerClient.downloads != 0 {
		t.Fatalf("downloads = %d, want 0", providerClient.downloads)
	}
	if got, err := os.ReadFile(realPath); err != nil {
		t.Fatalf("read executable: %v", err)
	} else if string(got) != "old zenget" {
		t.Fatalf("executable = %q, want old content", got)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state path error = %v, want state file to remain absent", err)
	}
	if !strings.Contains(output.String(), "already up to date") {
		t.Fatalf("output = %q, want up-to-date message", output.String())
	}
}

func TestRunSelfUpgradeDoesNotDowngrade(t *testing.T) {
	realPath, executablePath := newSelfUpgradeExecutable(t, []byte("old zenget"))
	providerClient := &selfUpgradeTestProvider{release: &provider.Release{TagName: "v0.0.0"}}
	var output strings.Builder

	err := runSelfUpgradeWithOptions(context.Background(), providerClient, selfUpgradeOptions{
		repository:   "tyutyutyu/zenget",
		goos:         "linux",
		goarch:       "amd64",
		executable:   func() (string, error) { return executablePath, nil },
		evalSymlinks: filepath.EvalSymlinks,
	}, &output)
	if err != nil {
		t.Fatalf("runSelfUpgradeWithOptions() error = %v", err)
	}
	if providerClient.downloads != 0 {
		t.Fatalf("downloads = %d, want 0", providerClient.downloads)
	}
	if got, err := os.ReadFile(realPath); err != nil {
		t.Fatalf("read executable: %v", err)
	} else if string(got) != "old zenget" {
		t.Fatalf("executable = %q, want old content", got)
	}
	if !strings.Contains(output.String(), "no downgrade performed") {
		t.Fatalf("output = %q, want no-downgrade message", output.String())
	}
}

func TestRunSelfUpgradeKeepsOldBinaryAfterChecksumMismatch(t *testing.T) {
	realPath, executablePath := newSelfUpgradeExecutable(t, []byte("old zenget"))
	providerClient := &selfUpgradeTestProvider{
		release: &provider.Release{
			TagName: "v0.0.2",
			Assets:  []provider.Asset{{Name: "zenget-linux-amd64", Digest: "sha256:" + strings.Repeat("0", 64)}},
		},
		body: []byte("new zenget"),
	}

	err := runSelfUpgradeWithOptions(context.Background(), providerClient, selfUpgradeOptions{
		repository:   "tyutyutyu/zenget",
		goos:         "linux",
		goarch:       "amd64",
		executable:   func() (string, error) { return executablePath, nil },
		evalSymlinks: filepath.EvalSymlinks,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
	if providerClient.downloads != 1 {
		t.Fatalf("downloads = %d, want 1", providerClient.downloads)
	}
	if got, readErr := os.ReadFile(realPath); readErr != nil {
		t.Fatalf("read executable: %v", readErr)
	} else if string(got) != "old zenget" {
		t.Fatalf("executable = %q after checksum failure, want old content", got)
	}
}

func TestRunSelfUpgradeReportsTargetWriteErrorWithoutReplacingOldBinary(t *testing.T) {
	directory := t.TempDir()
	oldPath := filepath.Join(directory, "zenget")
	if err := os.WriteFile(oldPath, []byte("old zenget"), 0755); err != nil {
		t.Fatalf("write old executable: %v", err)
	}
	blockedDirectory := filepath.Join(directory, "blocked")
	if err := os.Mkdir(blockedDirectory, 0755); err != nil {
		t.Fatalf("create blocked directory: %v", err)
	}
	target := filepath.Join(blockedDirectory, "zenget")
	if err := os.WriteFile(target, []byte("old zenget"), 0755); err != nil {
		t.Fatalf("write target executable: %v", err)
	}
	if err := os.Chmod(blockedDirectory, 0500); err != nil {
		t.Fatalf("remove write permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedDirectory, 0755) })
	providerClient := &selfUpgradeTestProvider{
		release: &provider.Release{TagName: "v0.0.2", Assets: []provider.Asset{{Name: "zenget-linux-amd64"}}},
		body:    []byte("new zenget"),
	}

	err := runSelfUpgradeWithOptions(context.Background(), providerClient, selfUpgradeOptions{
		repository:   "tyutyutyu/zenget",
		goos:         "linux",
		goarch:       "amd64",
		executable:   func() (string, error) { return target, nil },
		evalSymlinks: filepath.EvalSymlinks,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "temporary install file") {
		t.Fatalf("error = %v, want temporary install file error", err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil {
		t.Fatalf("read executable: %v", readErr)
	} else if string(got) != "old zenget" {
		t.Fatalf("executable = %q after write failure, want old content", got)
	}
}

func TestRunSelfUpgradeRejectsManagedArtifactWithoutNetwork(t *testing.T) {
	realPath, executablePath := newSelfUpgradeExecutable(t, []byte("old zenget"))
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	managed := &state.State{Apps: map[string]state.App{
		"tyutyutyu/zenget": {RealPath: realPath, InstallPath: filepath.Join(t.TempDir(), "zenget")},
	}}
	if err := managed.Save(); err != nil {
		t.Fatalf("save managed state: %v", err)
	}
	providerClient := &selfUpgradeTestProvider{release: &provider.Release{TagName: "v0.0.2"}}

	err := runSelfUpgradeWithOptions(context.Background(), providerClient, selfUpgradeOptions{
		repository:   "tyutyutyu/zenget",
		goos:         "linux",
		goarch:       "amd64",
		executable:   func() (string, error) { return executablePath, nil },
		evalSymlinks: filepath.EvalSymlinks,
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "zenget-managed path") {
		t.Fatalf("error = %v, want managed-path error", err)
	}
	if providerClient.latestCalls != 0 {
		t.Fatalf("latest calls = %d, want 0", providerClient.latestCalls)
	}
	if got, readErr := os.ReadFile(realPath); readErr != nil {
		t.Fatalf("read executable: %v", readErr)
	} else if string(got) != "old zenget" {
		t.Fatalf("executable = %q, want old content", got)
	}
}

func TestRunSelfUpgradeValidatesRepositoryAndWindowsBeforeExecutableResolution(t *testing.T) {
	providerClient := &selfUpgradeTestProvider{}
	for _, test := range []struct {
		name       string
		repository string
		goos       string
		want       string
	}{
		{name: "empty repository", repository: "", goos: "linux", want: "not configured"},
		{name: "placeholder repository", repository: "org/repo", goos: "linux", want: "not configured"},
		{name: "malformed repository", repository: "not-a-repository", goos: "linux", want: "expected org/repo"},
		{name: "windows", repository: "tyutyutyu/zenget", goos: "windows", want: "not supported on Windows"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := runSelfUpgradeWithOptions(context.Background(), providerClient, selfUpgradeOptions{
				repository: test.repository,
				goos:       test.goos,
				executable: func() (string, error) {
					t.Fatal("executable resolver should not be called")
					return "", nil
				},
				evalSymlinks: filepath.EvalSymlinks,
			}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompareSelfVersionsSupportsPrefixesPrereleasesAndBuildMetadata(t *testing.T) {
	for _, test := range []struct {
		current string
		latest  string
		want    int
	}{
		{current: "0.0.1", latest: "v0.0.2", want: -1},
		{current: "v1.2.3-rc.1", latest: "1.2.3", want: -1},
		{current: "1.2.3", latest: "v1.2.3-rc.1", want: 1},
		{current: "1.2.3-alpha", latest: "1.2.3-alpha.1", want: -1},
		{current: "1.2.3-rc.2", latest: "1.2.3-rc.1", want: 1},
		{current: "1.2.3+build.1", latest: "1.2.3+build.2", want: 0},
	} {
		got, err := compareSelfVersions(test.current, test.latest)
		if err != nil {
			t.Fatalf("compareSelfVersions(%q, %q) error = %v", test.current, test.latest, err)
		}
		if got != test.want {
			t.Errorf("compareSelfVersions(%q, %q) = %d, want %d", test.current, test.latest, got, test.want)
		}
	}
}

func TestCompareSelfVersionsRejectsInvalidVersions(t *testing.T) {
	for _, test := range []struct {
		current string
		latest  string
		want    string
	}{
		{current: "0.0", latest: "0.0.1", want: "invalid current zenget version"},
		{current: "0.0.1", latest: "release", want: "invalid latest zenget release tag"},
		{current: "0.0.1", latest: "v01.0.0", want: "leading zeroes"},
	} {
		_, err := compareSelfVersions(test.current, test.latest)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("compareSelfVersions(%q, %q) error = %v, want %q", test.current, test.latest, err, test.want)
		}
	}
}

func TestResolveSelfUpgradeTargetReportsResolverFailures(t *testing.T) {
	resolverError := fmt.Errorf("resolver failure")
	if _, err := resolveSelfUpgradeTarget(nil, filepath.EvalSymlinks); err == nil || !strings.Contains(err.Error(), "executable resolver is nil") {
		t.Errorf("nil executable resolver error = %v", err)
	}
	if _, err := resolveSelfUpgradeTarget(func() (string, error) { return "", nil }, nil); err == nil || !strings.Contains(err.Error(), "symlink resolver is nil") {
		t.Errorf("nil symlink resolver error = %v", err)
	}
	if _, err := resolveSelfUpgradeTarget(func() (string, error) { return "", resolverError }, filepath.EvalSymlinks); err == nil || !strings.Contains(err.Error(), "resolver failure") {
		t.Errorf("executable error = %v", err)
	}
	if _, err := resolveSelfUpgradeTarget(func() (string, error) { return "", nil }, filepath.EvalSymlinks); err == nil || !strings.Contains(err.Error(), "executable path is empty") {
		t.Errorf("empty executable path error = %v", err)
	}
	if _, err := resolveSelfUpgradeTarget(func() (string, error) { return "path", nil }, func(string) (string, error) { return "", resolverError }); err == nil || !strings.Contains(err.Error(), "resolver failure") {
		t.Errorf("symlink error = %v", err)
	}
	if _, err := resolveSelfUpgradeTarget(func() (string, error) { return "path", nil }, func(string) (string, error) { return "", nil }); err == nil || !strings.Contains(err.Error(), "resolved path is empty") {
		t.Errorf("empty resolved path error = %v", err)
	}
	if _, err := resolveSelfUpgradeTarget(func() (string, error) { return "missing", nil }, func(path string) (string, error) { return path, nil }); err == nil || !strings.Contains(err.Error(), "inspect running zenget binary") {
		t.Errorf("missing target error = %v", err)
	}
}

func newSelfUpgradeExecutable(t *testing.T, contents []byte) (realPath, executablePath string) {
	t.Helper()
	directory := t.TempDir()
	realPath = filepath.Join(directory, "bin", "zenget")
	if err := os.MkdirAll(filepath.Dir(realPath), 0755); err != nil {
		t.Fatalf("create executable directory: %v", err)
	}
	if err := os.WriteFile(realPath, contents, 0755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	executablePath = filepath.Join(directory, "current", "zenget")
	if err := os.MkdirAll(filepath.Dir(executablePath), 0755); err != nil {
		t.Fatalf("create symlink directory: %v", err)
	}
	if err := os.Symlink(realPath, executablePath); err != nil {
		t.Fatalf("create executable symlink: %v", err)
	}
	return realPath, executablePath
}

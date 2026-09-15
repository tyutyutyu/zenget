package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"zenget/internal/appcfg"
	"zenget/internal/artifactcache"
	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/state"
)

func TestResolveInstallChecksumPolicyPrecedence(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	cfg := &config.Config{}
	if err := cfg.SetChecksumPolicy(string(config.ChecksumPolicyRequired)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	if got, err := resolveInstallChecksumPolicy(installOptions{}); err != nil || got != config.ChecksumPolicyRequired {
		t.Fatalf("global policy = %q, error = %v; want required", got, err)
	}
	if got, err := resolveInstallChecksumPolicy(installOptions{
		checksumPolicy:    string(config.ChecksumPolicyIfPresent),
		checksumPolicySet: true,
	}); err != nil || got != config.ChecksumPolicyIfPresent {
		t.Fatalf("explicit policy = %q, error = %v; want if-present", got, err)
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got, err := resolveInstallChecksumPolicy(installOptions{}); err != nil || got != config.ChecksumPolicyIfPresent {
		t.Fatalf("missing config policy = %q, error = %v; want if-present", got, err)
	}
	if _, err := resolveInstallChecksumPolicy(installOptions{checksumPolicy: "off", checksumPolicySet: true}); err == nil {
		t.Fatal("invalid explicit policy error = nil")
	}
}

func TestRunInstallRequiredFailsBeforePersistentChanges(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	client, release, assetRequests := newChecksumPolicyInstallFixture(t, []byte("raw widget"))
	var output bytes.Buffer
	err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		release:           release,
		checksumPolicy:    string(config.ChecksumPolicyRequired),
		checksumPolicySet: true,
		goos:              runtime.GOOS,
		goarch:            runtime.GOARCH,
		input:             strings.NewReader(""),
	}, &output)
	if err == nil {
		t.Fatal("required install error = nil, want missing checksum error")
	}
	for _, want := range []string{
		"policy=required",
		"repository=acme/widget",
		"release=v1.0.0",
		`asset="widget"`,
		"no valid SHA-256 checksum",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("required install error = %q, want %q", err, want)
		}
	}
	if assetRequests.Load() != 1 {
		t.Fatalf("asset downloads = %d, want one download before verification", assetRequests.Load())
	}

	target := filepath.Join(home, ".local", "bin", "widget")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("wrapper exists after required checksum failure: %v", err)
	}
	statePath, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state exists after required checksum failure: %v", err)
	}
	appConfigPath, err := appcfg.Path("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(appConfigPath); !os.IsNotExist(err) {
		t.Fatalf("app config exists after required checksum failure: %v", err)
	}
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache.Root()); !os.IsNotExist(err) {
		t.Fatalf("artifact cache exists after required checksum failure: %v", err)
	}
}

func TestRunInstallExplicitChecksumPolicyOverridesGlobalPolicy(t *testing.T) {
	t.Run("if-present overrides required", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		cfg := &config.Config{}
		if err := cfg.SetChecksumPolicy(string(config.ChecksumPolicyRequired)); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}

		client, release, _ := newChecksumPolicyInstallFixture(t, []byte("raw widget"))
		err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
			release:           release,
			checksumPolicy:    string(config.ChecksumPolicyIfPresent),
			checksumPolicySet: true,
			goos:              runtime.GOOS,
			goarch:            runtime.GOARCH,
			input:             strings.NewReader(""),
		}, io.Discard)
		if err != nil {
			t.Fatalf("explicit if-present install: %v", err)
		}
	})

	t.Run("required overrides if-present", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		cfg := &config.Config{}
		if err := cfg.SetChecksumPolicy(string(config.ChecksumPolicyIfPresent)); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}

		client, release, _ := newChecksumPolicyInstallFixture(t, []byte("raw widget"))
		err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
			release:           release,
			checksumPolicy:    string(config.ChecksumPolicyRequired),
			checksumPolicySet: true,
			goos:              runtime.GOOS,
			goarch:            runtime.GOARCH,
			input:             strings.NewReader(""),
		}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "policy=required") {
			t.Fatalf("explicit required install error = %v, want required policy failure", err)
		}
	})
}

func TestUpgradeDryRunIncludesChecksumPolicyContext(t *testing.T) {
	setUpgradeEnvironment(t)
	cfg := &config.Config{}
	if err := cfg.SetChecksumPolicy(string(config.ChecksumPolicyRequired)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	saveUpgradeState(t, map[string]state.App{
		"acme/widget": seedUpgradeApp(t, "acme/widget", "v1.0.0", []byte("old"), false, "widget"),
	})
	server := newUpgradeTestServer(t, map[string]*upgradeFixture{
		"acme/widget": {tag: "v2.0.0", assetName: "widget", assetBody: []byte("new")},
	})
	var output bytes.Buffer
	if err := runUpgradeWithOptions(context.Background(), newUpgradeClient(server), nil, upgradeOptions{dryRun: true}, &output); err != nil {
		t.Fatalf("upgrade dry-run: %v", err)
	}
	for _, want := range []string{
		"policy=required",
		"repository=acme/widget",
		"release=v2.0.0",
		`asset="widget"`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("upgrade dry-run output = %q, want %q", output.String(), want)
		}
	}
}

func TestApplyDryRunIncludesChecksumPolicyContext(t *testing.T) {
	setupManifestEnvironment(t)
	cfg := &config.Config{}
	if err := cfg.SetChecksumPolicy(string(config.ChecksumPolicyRequired)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/tags/v1.0.0" {
			writeManifestRelease(w, server.URL, "v1.0.0", "widget")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveManifestFile(t, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/widget", Provider: "github", Tag: "v1.0.0", Asset: "widget"}},
	})
	previousDryRun := applyDryRun
	applyDryRun = true
	t.Cleanup(func() { applyDryRun = previousDryRun })
	command, output, _ := newBufferedCommand()
	if err := runApply(command, []string{manifestPath}); err != nil {
		t.Fatalf("apply dry-run: %v", err)
	}
	for _, want := range []string{
		"policy=required",
		"repository=acme/widget",
		"release=v1.0.0",
		`asset="widget"`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("apply dry-run output = %q, want %q", output.String(), want)
		}
	}
}

func TestUpgradeUsesGlobalChecksumPolicy(t *testing.T) {
	setUpgradeEnvironment(t)
	cfg := &config.Config{}
	if err := cfg.SetChecksumPolicy(string(config.ChecksumPolicyRequired)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	oldBinary := []byte("old")
	saveUpgradeState(t, map[string]state.App{
		"acme/widget": seedUpgradeApp(t, "acme/widget", "v1.0.0", oldBinary, false, "widget"),
	})
	server := newUpgradeTestServer(t, map[string]*upgradeFixture{
		"acme/widget": {tag: "v2.0.0", assetName: "widget", assetBody: []byte("new")},
	})
	var output bytes.Buffer
	err := runUpgradeWithOptions(context.Background(), newUpgradeClient(server), nil, upgradeOptions{yes: true}, &output)
	if err == nil {
		t.Fatal("upgrade error = nil, want checksum failure")
	}
	for _, want := range []string{"policy=required", "repository=acme/widget", "release=v2.0.0", `asset="widget"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("upgrade output = %q, want %q", output.String(), want)
		}
	}
	assertUpgradeBinary(t, "acme/widget", oldBinary)
}

func TestEnsureRestoreUsesGlobalChecksumPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg := &config.Config{}
	if err := cfg.SetChecksumPolicy(string(config.ChecksumPolicyRequired)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := (&state.State{Apps: map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstallPath: filepath.Join(home, ".local", "bin", "widget")},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	server := newEnsureTestServer(t, func(r *http.Request) (string, int) {
		if r.URL.Path == "/repos/acme/widget/releases/tags/v1.0.0" {
			return "v1.0.0", http.StatusOK
		}
		if r.URL.Path == "/assets/widget" {
			return "restored", http.StatusOK
		}
		return "", http.StatusNotFound
	})
	var output bytes.Buffer
	err := runEnsureWithClient(context.Background(), newEnsureTestClient(server), []string{"acme/widget"}, &output)
	if err == nil {
		t.Fatal("ensure error = nil, want checksum failure")
	}
	for _, want := range []string{"policy=required", "repository=acme/widget", "release=v1.0.0", `asset="widget"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("ensure output = %q, want %q", output.String(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); !os.IsNotExist(err) {
		t.Fatalf("restore wrapper exists after checksum failure: %v", err)
	}
}

func TestApplyUsesGlobalChecksumPolicy(t *testing.T) {
	setupManifestEnvironment(t)
	cfg := &config.Config{}
	if err := cfg.SetChecksumPolicy(string(config.ChecksumPolicyRequired)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/tags/v1.0.0" {
			writeManifestRelease(w, server.URL, "v1.0.0", "widget")
			return
		}
		if r.URL.Path == "/assets/widget" {
			_, _ = io.WriteString(w, "new widget")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveManifestFile(t, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/widget", Provider: "github", Tag: "v1.0.0", Asset: "widget"}},
	})
	command, output, _ := newBufferedCommand()
	if err := runApply(command, []string{manifestPath}); err == nil {
		t.Fatal("apply error = nil, want checksum failure")
	}
	for _, want := range []string{"policy=required", "repository=acme/widget", "release=v1.0.0", `asset="widget"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("apply output = %q, want %q", output.String(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".local", "bin", "widget")); !os.IsNotExist(err) {
		t.Fatalf("apply wrapper exists after checksum failure: %v", err)
	}
}

func newChecksumPolicyInstallFixture(t *testing.T, assetBody []byte) (*github.Client, *github.Release, *atomic.Int32) {
	t.Helper()
	var assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/assets/widget" {
			http.NotFound(w, r)
			return
		}
		assetRequests.Add(1)
		_, _ = w.Write(assetBody)
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{{
			ID:                 1,
			Name:               "widget",
			BrowserDownloadURL: server.URL + "/assets/widget",
		}},
	}
	return client, release, &assetRequests
}

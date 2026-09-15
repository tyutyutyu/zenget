package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zenget/internal/appcfg"
	"zenget/internal/github"
	"zenget/internal/state"
)

func setupManifestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ZENGET_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
}

func saveManifestState(t *testing.T, apps map[string]state.App) {
	t.Helper()
	if err := (&state.State{Apps: apps}).Save(); err != nil {
		t.Fatalf("save manifest state: %v", err)
	}
}

func saveManifestFile(t *testing.T, manifest state.Manifest) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	data, err := marshalManifest(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func useManifestClient(t *testing.T, client *github.Client) {
	t.Helper()
	original := manifestClientFactory
	manifestClientFactory = func(providerName string) (*github.Client, error) {
		if providerName != manifestProviderGitHub {
			t.Fatalf("provider factory received %q, want github", providerName)
		}
		return client, nil
	}
	t.Cleanup(func() { manifestClientFactory = original })
}

func TestRunExportIsDeterministicAndPortable(t *testing.T) {
	setupManifestEnvironment(t)
	if err := (&appcfg.AppConfig{
		Asset:         "acme-linux-amd64.tar.gz",
		ArchiveBinary: "bin/acme",
		TargetName:    "acme-tool",
	}).Save("zeta/acme"); err != nil {
		t.Fatal(err)
	}
	if err := (&appcfg.AppConfig{
		Asset:         "widget-linux-amd64.tar.gz",
		ArchiveBinary: "bin/widget",
		TargetName:    "widget-tool",
	}).Save("acme/widget"); err != nil {
		t.Fatal(err)
	}
	installedAt := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	saveManifestState(t, map[string]state.App{
		"zeta/acme": {
			Provider:     "github",
			Version:      "v2.0.0",
			InstalledAt:  installedAt,
			DownloadURL:  "https://secret.example/zeta-acme",
			InstallPath:  "/home/user/.local/bin/acme-tool",
			RealPath:     "/home/user/.local/share/zenget/acme",
			XXH3_64:      "machine-specific-hash",
			Pinned:       true,
			PlatformOS:   "linux",
			PlatformArch: "amd64",
		},
		"acme/widget": {
			Provider:     "github",
			Version:      "v1.2.3",
			InstalledAt:  installedAt,
			DownloadURL:  "https://secret.example/acme-widget",
			InstallPath:  "/home/user/.local/bin/widget-tool",
			RealPath:     "/home/user/.local/share/zenget/widget",
			XXH3_64:      "another-machine-specific-hash",
			PlatformOS:   "linux",
			PlatformArch: "amd64",
		},
	})

	previousForce := exportForce
	exportForce = false
	t.Cleanup(func() { exportForce = previousForce })
	firstCommand, firstOutput, _ := newBufferedCommand()
	if err := runExport(firstCommand, nil); err != nil {
		t.Fatalf("first export: %v", err)
	}
	secondCommand, secondOutput, _ := newBufferedCommand()
	if err := runExport(secondCommand, nil); err != nil {
		t.Fatalf("second export: %v", err)
	}
	if !bytes.Equal(firstOutput.Bytes(), secondOutput.Bytes()) {
		t.Fatalf("exports differ:\n%s\n---\n%s", firstOutput, secondOutput)
	}

	var manifest state.Manifest
	if err := json.Unmarshal(firstOutput.Bytes(), &manifest); err != nil {
		t.Fatalf("decode exported manifest: %v", err)
	}
	if manifest.SchemaVersion != state.ManifestSchemaVersion {
		t.Fatalf("schema version = %d, want %d", manifest.SchemaVersion, state.ManifestSchemaVersion)
	}
	if got := []string{manifest.Apps[0].Repository, manifest.Apps[1].Repository}; !bytes.Equal([]byte(strings.Join(got, ",")), []byte("acme/widget,zeta/acme")) {
		t.Fatalf("repository order = %v", got)
	}
	if got := manifest.Apps[0]; got.Asset != "widget-linux-amd64.tar.gz" || got.ArchiveBinary != "bin/widget" || got.TargetName != "widget-tool" || got.Tag != "v1.2.3" {
		t.Fatalf("exported choices = %#v", got)
	}

	text := firstOutput.String()
	for _, forbidden := range []string{
		"secret.example",
		"/home/user/.local",
		"machine-specific-hash",
		"installed_at",
		"download_url",
		"real_path",
		"xxh3_64",
		"pinned",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("export contains forbidden %q: %s", forbidden, text)
		}
	}
}

func TestWriteManifestFileIsAtomicAndNoClobberByDefault(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "manifest.json")
	first := []byte("first\n")
	second := []byte("second\n")
	if err := writeManifestFile(output, first, false); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	if got, err := os.ReadFile(output); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("initial output = %q, err = %v", got, err)
	}
	if err := writeManifestFile(output, second, false); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second write error = %v, want no-clobber error", err)
	}
	if got, err := os.ReadFile(output); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("output after refused write = %q, err = %v", got, err)
	}
	if err := writeManifestFile(output, second, true); err != nil {
		t.Fatalf("forced write: %v", err)
	}
	if got, err := os.ReadFile(output); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("output after forced write = %q, err = %v", got, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".zenget-manifest-") {
			t.Errorf("temporary manifest remains: %s", entry.Name())
		}
	}
}

func TestLoadManifestRejectsInvalidAndNonLocalInputs(t *testing.T) {
	setupManifestEnvironment(t)
	validEntry := `{"repository":"acme/widget","provider":"github","tag":"v1.0.0"}`
	tests := []struct {
		name string
		data string
	}{
		{name: "unknown schema", data: `{"schema_version":2,"apps":[]}`},
		{name: "missing schema", data: `{"apps":[]}`},
		{name: "missing apps", data: `{"schema_version":1}`},
		{name: "null apps", data: `{"schema_version":1,"apps":null}`},
		{name: "unknown field", data: `{"schema_version":1,"apps":[],"token":"secret"}`},
		{name: "duplicate repository", data: `{"schema_version":1,"apps":[` + validEntry + `,` + validEntry + `]}`},
		{name: "unsupported provider", data: `{"schema_version":1,"apps":[{"repository":"acme/widget","provider":"gitlab","tag":"v1.0.0"}]}`},
		{name: "invalid repository", data: `{"schema_version":1,"apps":[{"repository":"acme/widget/extra","provider":"github","tag":"v1.0.0"}]}`},
		{name: "empty tag", data: `{"schema_version":1,"apps":[{"repository":"acme/widget","provider":"github","tag":""}]}`},
		{name: "invalid platform", data: `{"schema_version":1,"apps":[{"repository":"acme/widget","provider":"github","tag":"v1.0.0","platform_os":"linux"}]}`},
		{name: "URL asset", data: `{"schema_version":1,"apps":[{"repository":"acme/widget","provider":"github","tag":"v1.0.0","asset":"https://evil.example/tool"}]}`},
		{name: "absolute archive member", data: `{"schema_version":1,"apps":[{"repository":"acme/widget","provider":"github","tag":"v1.0.0","archive_binary":"/tmp/tool"}]}`},
		{name: "traversal archive member", data: `{"schema_version":1,"apps":[{"repository":"acme/widget","provider":"github","tag":"v1.0.0","archive_binary":"../tool"}]}`},
		{name: "invalid target", data: `{"schema_version":1,"apps":[{"repository":"acme/widget","provider":"github","tag":"v1.0.0","target_name":"bin/tool"}]}`},
		{name: "multiple values", data: `{"schema_version":1,"apps":[]} {}`},
		{name: "non object", data: `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, []byte(tt.data), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadManifest(path); err == nil {
				t.Fatalf("loadManifest(%q) error = nil", tt.data)
			}
		})
	}

	if !isLocalManifestPath(filepath.Join(t.TempDir(), "manifest.json")) {
		t.Fatal("absolute filesystem path was rejected")
	}
	for _, input := range []string{"-", "https://example.com/manifest.json", "file:///tmp/manifest.json", "ssh://host/manifest.json"} {
		if isLocalManifestPath(input) {
			t.Errorf("non-local input accepted: %q", input)
		}
	}
	if _, err := loadManifest("https://example.com/manifest.json"); err == nil || !strings.Contains(err.Error(), "URLs and stdin") {
		t.Fatalf("URL load error = %v", err)
	}
}

func TestRunApplyDryRunUsesExactTagsInSortedReadOnlyPlan(t *testing.T) {
	setupManifestEnvironment(t)
	saveManifestState(t, map[string]state.App{
		"untouched/tool": {Provider: "github", Version: "v9.9.9", InstallPath: "/not-touched"},
	})
	statePath, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	beforeState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var releaseRequests, assetRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/alpha/releases/tags/v1.0.0":
			releaseRequests.Add(1)
			writeManifestRelease(w, server.URL, "v1.0.0", "alpha")
		case "/repos/zeta/tool/releases/tags/v2.0.0":
			releaseRequests.Add(1)
			writeManifestRelease(w, server.URL, "v2.0.0", "zeta")
		case "/assets/alpha", "/assets/zeta":
			assetRequests.Add(1)
			http.Error(w, "dry-run must not download", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveManifestFile(t, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps: []state.ManifestApp{
			{Repository: "zeta/tool", Provider: "github", Tag: "v2.0.0", Asset: "zeta"},
			{Repository: "acme/alpha", Provider: "github", Tag: "v1.0.0", Asset: "alpha"},
		},
	})

	previousDryRun := applyDryRun
	applyDryRun = true
	t.Cleanup(func() { applyDryRun = previousDryRun })
	cmd, output, _ := newBufferedCommand()
	if err := runApply(cmd, []string{manifestPath}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if got, want := output.String(), "install acme/alpha v1.0.0\ninstall zeta/tool v2.0.0\nApply summary: 2 install, 0 restore, 0 unchanged, 0 failed\nchecksum verification pending: policy=if-present repository=acme/alpha release=v1.0.0 asset=\"alpha\"\nchecksum verification pending: policy=if-present repository=zeta/tool release=v2.0.0 asset=\"zeta\"\n"; got != want {
		t.Fatalf("dry-run output = %q, want %q", got, want)
	}
	if got := releaseRequests.Load(); got != 2 {
		t.Fatalf("release requests = %d, want 2", got)
	}
	if got := assetRequests.Load(); got != 0 {
		t.Fatalf("asset requests = %d, want 0", got)
	}
	afterState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) {
		t.Fatalf("dry-run changed state:\n%s\n---\n%s", beforeState, afterState)
	}
	for _, repository := range []string{"acme/alpha", "zeta/tool"} {
		configPath, err := appcfg.Path(repository)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(configPath); !os.IsNotExist(err) {
			t.Errorf("dry-run changed app config %q: %v", configPath, err)
		}
	}
}

func TestApplyInstallsExactChoicesAndRestoresFromArtifactCache(t *testing.T) {
	setupManifestEnvironment(t)
	saveManifestState(t, map[string]state.App{
		"untouched/tool": {Provider: "github", Version: "v9.9.9", InstallPath: "/not-touched"},
	})
	binary := []byte("#!/bin/sh\necho manifest\n")
	archive := tarGzAsset(t, "bin/widget", binary)
	var exactRequests, latestRequests, assetRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.2.3":
			exactRequests.Add(1)
			writeManifestRelease(w, server.URL, "v1.2.3", "widget.tar.gz")
		case "/repos/acme/widget/releases/latest":
			latestRequests.Add(1)
			http.Error(w, "latest lookup is not allowed", http.StatusInternalServerError)
		case "/assets/widget.tar.gz":
			assetRequests.Add(1)
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveManifestFile(t, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps: []state.ManifestApp{{
			Repository:    "acme/widget",
			Provider:      "github",
			Tag:           "v1.2.3",
			Asset:         "widget.tar.gz",
			PlatformOS:    runtime.GOOS,
			PlatformArch:  runtime.GOARCH,
			ArchiveBinary: "bin/widget",
			TargetName:    "custom-widget",
		}},
	})

	previousDryRun := applyDryRun
	applyDryRun = false
	t.Cleanup(func() { applyDryRun = previousDryRun })
	firstCommand, firstOutput, _ := newBufferedCommand()
	if err := runApply(firstCommand, []string{manifestPath}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if !strings.Contains(firstOutput.String(), "install acme/widget v1.2.3") {
		t.Fatalf("first apply output = %q", firstOutput)
	}
	installedState, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, ok := installedState.Get("acme/widget")
	if !ok || app.Version != "v1.2.3" || app.Provider != "github" || app.PlatformOS != runtime.GOOS || app.PlatformArch != runtime.GOARCH {
		t.Fatalf("installed state app = %#v", app)
	}
	config, err := appcfg.Load("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if config.Asset != "widget.tar.gz" || config.ArchiveBinary != "bin/widget" || config.TargetName != "custom-widget" {
		t.Fatalf("saved app config = %#v", config)
	}
	if app.InstallPath == "" || filepath.Base(app.InstallPath) != "custom-widget" || app.RealPath == "" {
		t.Fatalf("installed paths missing: %#v", app)
	}
	if _, err := os.Stat(app.InstallPath); err != nil {
		t.Fatalf("wrapper missing: %v", err)
	}
	if got, err := os.ReadFile(app.RealPath); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("installed binary = %q, err = %v", got, err)
	}
	untouched, ok := installedState.Get("untouched/tool")
	if !ok || untouched.Version != "v9.9.9" || untouched.InstallPath != "/not-touched" {
		t.Fatalf("untouched app changed: %#v", untouched)
	}

	secondCommand, secondOutput, _ := newBufferedCommand()
	if err := runApply(secondCommand, []string{manifestPath}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got, want := secondOutput.String(), "unchanged acme/widget v1.2.3\nApply summary: 0 install, 0 restore, 1 unchanged, 0 failed\n"; got != want {
		t.Fatalf("second apply output = %q, want %q", got, want)
	}
	if got := exactRequests.Load(); got != 2 {
		t.Fatalf("exact release requests = %d, want 2", got)
	}
	if got := latestRequests.Load(); got != 0 {
		t.Fatalf("latest release requests = %d, want 0", got)
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests = %d, want 1", got)
	}
	if err := os.Remove(app.RealPath); err != nil {
		t.Fatalf("remove artifact before cache restore: %v", err)
	}
	thirdCommand, thirdOutput, _ := newBufferedCommand()
	if err := runApply(thirdCommand, []string{manifestPath}); err != nil {
		t.Fatalf("cache restore apply: %v", err)
	}
	if !strings.Contains(thirdOutput.String(), "restore acme/widget v1.2.3") {
		t.Fatalf("cache restore apply output = %q", thirdOutput)
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests after cache restore = %d, want 1", got)
	}
}

func TestRunApplyRestoresUnhealthySameVersionAndContinuesAfterFailure(t *testing.T) {
	setupManifestEnvironment(t)
	saveManifestState(t, map[string]state.App{
		"acme/restore": {Provider: "github", Version: "v1.0.0"},
	})
	binary := []byte("#!/bin/sh\necho restored\n")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/bad/releases/tags/v9.0.0":
			http.NotFound(w, r)
		case "/repos/acme/restore/releases/tags/v1.0.0":
			writeManifestRelease(w, server.URL, "v1.0.0", "restore")
		case "/assets/restore":
			_, _ = w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveManifestFile(t, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps: []state.ManifestApp{
			{Repository: "acme/bad", Provider: "github", Tag: "v9.0.0", Asset: "bad"},
			{Repository: "acme/restore", Provider: "github", Tag: "v1.0.0", Asset: "restore", TargetName: "restore"},
		},
	})
	previousDryRun := applyDryRun
	applyDryRun = false
	t.Cleanup(func() { applyDryRun = previousDryRun })
	cmd, output, _ := newBufferedCommand()
	err := runApply(cmd, []string{manifestPath})
	if err == nil || !strings.Contains(err.Error(), "apply failed for 1 application(s)") {
		t.Fatalf("apply error = %v, want one failed entry", err)
	}
	text := output.String()
	if !strings.Contains(text, "failed acme/bad") || !strings.Contains(text, "restore acme/restore v1.0.0") {
		t.Fatalf("partial apply output = %q", text)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, ok := st.Get("acme/restore")
	if !ok || app.Version != "v1.0.0" || app.RealPath == "" {
		t.Fatalf("restored state = %#v", app)
	}
	if _, err := os.Stat(app.RealPath); err != nil {
		t.Fatalf("restored binary missing: %v", err)
	}
}

func writeManifestRelease(w http.ResponseWriter, serverURL, tag, assetName string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tag_name": tag,
		"assets": []map[string]string{{
			"name":                 assetName,
			"browser_download_url": serverURL + "/assets/" + assetName,
		}},
	})
}

func TestManifestProviderAndChoiceHelpers(t *testing.T) {
	if got := manifestProvider(state.Manifest{}); got != manifestProviderGitHub {
		t.Fatalf("empty manifest provider = %q", got)
	}
	if _, err := newManifestClient("gitlab"); err == nil {
		t.Fatal("unsupported manifest provider was accepted")
	}
	if got := repositoryParts("acme/widget"); got != "acme" || repositoryName("acme/widget") != "widget" {
		t.Fatalf("repository split = %q/%q", got, repositoryName("acme/widget"))
	}
	if got := releaseTag(nil); got != "<nil>" {
		t.Fatalf("nil release tag = %q", got)
	}
	configuration := &appcfg.AppConfig{Asset: "asset", ArchiveBinary: "bin/tool", TargetName: "tool"}
	entry := state.ManifestApp{Provider: "github", Asset: "asset", ArchiveBinary: "bin/tool", TargetName: "tool"}
	app := state.App{Provider: "github", PlatformOS: "linux", PlatformArch: "amd64"}
	if !applyChoicesMatch(app, configuration, entry, "linux", "amd64") {
		t.Fatal("matching choices were rejected")
	}
	for _, mismatch := range []struct {
		name string
		app  state.App
		cfg  *appcfg.AppConfig
		want state.ManifestApp
	}{
		{name: "provider", app: state.App{Provider: "gitlab"}, cfg: configuration, want: entry},
		{name: "platform", app: state.App{Provider: "github", PlatformOS: "darwin", PlatformArch: "arm64"}, cfg: configuration, want: state.ManifestApp{Provider: "github", PlatformOS: "linux", PlatformArch: "amd64"}},
		{name: "asset", app: app, cfg: &appcfg.AppConfig{Asset: "other"}, want: entry},
		{name: "archive", app: app, cfg: &appcfg.AppConfig{ArchiveBinary: "other"}, want: entry},
		{name: "target", app: app, cfg: &appcfg.AppConfig{TargetName: "other"}, want: entry},
	} {
		t.Run(mismatch.name, func(t *testing.T) {
			if applyChoicesMatch(mismatch.app, mismatch.cfg, mismatch.want, "linux", "amd64") {
				t.Fatal("mismatched choices were accepted")
			}
		})
	}
}

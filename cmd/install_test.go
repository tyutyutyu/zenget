package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/artifactcache"
	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/hashx"
	"zenget/internal/install"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

func installedRealPath(t *testing.T, repository string) string {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get(repository)
	if !ok {
		t.Fatalf("state does not contain %s", repository)
	}
	return app.RealPath
}

func TestRunInstallWithClientInstallsAndSkipsSameVersionDownload(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho widget\n")
	asset := tarGzAsset(t, "widget", binary)
	var releaseRequests, assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			releaseRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
				}},
			})
		case "/assets/widget.tar.gz":
			assetRequests.Add(1)
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !strings.Contains(output.String(), "Installed widget v1.2.3 to ") {
		t.Fatalf("first output = %q", output.String())
	}

	target := filepath.Join(home, ".local", "bin", "widget")
	realBin := installedRealPath(t, "acme/widget")

	wrapper, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read installed wrapper: %v", err)
	}
	if !bytes.Contains(wrapper, []byte("zenget-managed wrapper")) {
		t.Fatalf("installed file is not a zenget wrapper: %q", wrapper)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat installed wrapper: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0755); got != want {
		t.Fatalf("installed mode = %o, want %o", got, want)
	}

	gotBinary, err := os.ReadFile(realBin)
	if err != nil {
		t.Fatalf("read installed real binary: %v", err)
	}
	if !bytes.Equal(gotBinary, binary) {
		t.Fatalf("installed real binary = %q, want %q", gotBinary, binary)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state does not contain acme/widget")
	}
	if app.Version != "v1.2.3" || app.InstallPath != target || app.RealPath != realBin || app.XXH3_64 != hashx.Bytes(binary) {
		t.Fatalf("state app = %#v", app)
	}
	if app.DownloadURL != server.URL+"/assets/widget.tar.gz" || app.InstalledAt.IsZero() {
		t.Fatalf("state metadata = %#v", app)
	}

	output.Reset()
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got, want := output.String(), "widget v1.2.3 is already installed\n"; got != want {
		t.Fatalf("second output = %q, want %q", got, want)
	}
	if got, want := assetRequests.Load(), int32(1); got != want {
		t.Fatalf("asset requests = %d, want %d", got, want)
	}
	if got, want := releaseRequests.Load(), int32(2); got != want {
		t.Fatalf("release requests = %d, want %d", got, want)
	}
}

func TestInstallReusesAndRepairsArtifactCache(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	binary := []byte("#!/bin/sh\necho cached\n")
	asset := tarGzAsset(t, "widget", binary)
	var assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]any{{
					"id": 7, "name": "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
				}},
			})
		case "/assets/widget.tar.gz":
			assetRequests.Add(1)
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("first install: %v", err)
	}
	release := &github.Release{
		TagName: "v1.0.0",
		Assets:  []github.Asset{{ID: 7, Name: "widget.tar.gz", BrowserDownloadURL: server.URL + "/assets/widget.tar.gz"}},
	}
	options := installOptions{
		release: release, tag: release.TagName, goos: runtime.GOOS, goarch: runtime.GOARCH,
		input: strings.NewReader(""), force: true, exactTag: true,
	}
	app, ok := func() (state.App, bool) {
		st, err := state.Load()
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		return st.Get("acme/widget")
	}()
	if !ok {
		t.Fatal("state does not contain acme/widget")
	}
	if err := os.Remove(app.RealPath); err != nil {
		t.Fatalf("remove active artifact: %v", err)
	}
	if err := runInstallWithOptions(context.Background(), client, "acme/widget", options, io.Discard); err != nil {
		t.Fatalf("cache-hit restore: %v", err)
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests after cache hit = %d, want 1", got)
	}

	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := cache.List(artifactcache.Protection{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache entries = (%+v, %v), want one entry", entries, err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root(), entries[0].Entry.RelativePath), []byte("corrupted"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := runInstallWithOptions(context.Background(), client, "acme/widget", options, io.Discard); err != nil {
		t.Fatalf("corrupt-cache repair: %v", err)
	}
	if got := assetRequests.Load(); got != 2 {
		t.Fatalf("asset requests after corrupt cache = %d, want 2", got)
	}
}

func TestRunInstallWithClientMigratesLegacyArtifactAndRetainsOnePrevious(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	var tag atomic.Value
	tag.Store("v2.0.0")
	var assetBody atomic.Value
	assetBody.Store([]byte("version two"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/tool/releases/latest" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag.Load().(string),
				"assets": []map[string]string{{
					"name":                 "tool",
					"browser_download_url": "http://" + r.Host + "/assets/tool",
				}},
			})
			return
		}
		if r.URL.Path == "/assets/tool" {
			_, _ = w.Write(assetBody.Load().([]byte))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	legacyBinary := []byte("legacy version")
	legacyWrapper, legacyReal, err := install.Install(legacyBinary, "acme/tool", false)
	if err != nil {
		t.Fatalf("legacy Install() error = %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Set("acme/tool", state.App{
		Version:     "v1.0.0",
		InstalledAt: time.Now().UTC(),
		DownloadURL: server.URL + "/assets/tool",
		InstallPath: legacyWrapper,
		RealPath:    legacyReal,
		XXH3_64:     hashx.Bytes(legacyBinary),
	})
	if err := st.Save(); err != nil {
		t.Fatalf("save legacy state: %v", err)
	}

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/tool", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("legacy upgrade: %v", err)
	}

	st, err = state.Load()
	if err != nil {
		t.Fatalf("load state after legacy upgrade: %v", err)
	}
	app, ok := st.Get("acme/tool")
	if !ok || app.ActiveArtifact == nil || app.PreviousArtifact == nil {
		t.Fatalf("state after legacy upgrade = %#v, want active and previous artifacts", app)
	}
	v2Path := app.ActiveArtifact.RealPath
	if app.PreviousArtifact.RealPath != legacyReal {
		t.Fatalf("legacy previous path = %q, want %q", app.PreviousArtifact.RealPath, legacyReal)
	}
	if _, err := os.Stat(legacyReal); err != nil {
		t.Fatalf("legacy artifact after first upgrade: %v", err)
	}

	tag.Store("v3.0.0")
	assetBody.Store([]byte("version three"))
	if err := runInstallWithClient(context.Background(), client, "acme/tool", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("second upgrade: %v", err)
	}
	st, err = state.Load()
	if err != nil {
		t.Fatalf("load state after second upgrade: %v", err)
	}
	app, ok = st.Get("acme/tool")
	if !ok || app.Version != "v3.0.0" || app.PreviousArtifact == nil || app.PreviousArtifact.RealPath != v2Path {
		t.Fatalf("state after second upgrade = %#v", app)
	}
	if _, err := os.Stat(legacyReal); !os.IsNotExist(err) {
		t.Fatalf("legacy artifact was not retired, stat err = %v", err)
	}
	if _, err := os.Stat(v2Path); err != nil {
		t.Fatalf("v2 artifact was not retained: %v", err)
	}
}

func TestRunInstallWithClientPrivateAssetAPIAndGitHubDigest(t *testing.T) {
	const token = "private-test-token"
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	binary := []byte("#!/bin/sh\necho private\n")
	asset := tarGzAsset(t, "widget", binary)
	digest := sha256.Sum256(asset)
	digestText := strings.ToLower(hex.EncodeToString(digest[:]))
	var assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q, want Bearer token", got)
		}
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"url":                  "http://" + r.Host + "/api/assets/1",
					"browser_download_url": "http://" + r.Host + "/browser/widget.tar.gz",
					"digest":               "sha256:" + digestText,
				}},
			})
		case "/api/assets/1":
			assetRequests.Add(1)
			if got := r.Header.Get("Accept"); got != "application/octet-stream" {
				t.Errorf("asset Accept header = %q, want application/octet-stream", got)
			}
			_, _ = w.Write(asset)
		case "/browser/widget.tar.gz":
			http.Error(w, "browser fallback should not be used", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	client.Token = token
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("private install: %v", err)
	}
	if got, want := assetRequests.Load(), int32(1); got != want {
		t.Fatalf("API asset requests = %d, want %d", got, want)
	}
	if !strings.Contains(output.String(), "Verified SHA-256 checksum for widget.tar.gz") {
		t.Fatalf("output missing GitHub digest verification: %q", output.String())
	}
	gotBinary, err := os.ReadFile(installedRealPath(t, "acme/widget"))
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if !bytes.Equal(gotBinary, binary) {
		t.Fatalf("installed binary = %q, want extracted binary", gotBinary)
	}
}

func TestRunInstallWithClientGitHubDigestMismatchLeavesInstallUntouched(t *testing.T) {
	const token = "private-test-token"
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	asset := tarGzAsset(t, "widget", []byte("#!/bin/sh\necho private\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q, want Bearer token", got)
		}
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"url":                  "http://" + r.Host + "/api/assets/1",
					"browser_download_url": "http://" + r.Host + "/browser/widget.tar.gz",
					"digest":               "sha256:" + strings.Repeat("0", 64),
				}},
			})
		case "/api/assets/1":
			if got := r.Header.Get("Accept"); got != "application/octet-stream" {
				t.Errorf("asset Accept header = %q, want application/octet-stream", got)
			}
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	client.Token = token
	err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard)
	if err == nil {
		t.Fatal("private install error = nil, want GitHub digest mismatch")
	}
	if !strings.Contains(err.Error(), "from GitHub digest") {
		t.Fatalf("private install error = %q, want GitHub digest source", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); !os.IsNotExist(err) {
		t.Fatalf("wrapper should not be installed after digest mismatch")
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, ok := st.Get("acme/widget"); ok {
		t.Fatal("state should not contain acme/widget after digest mismatch")
	}
}

func TestRunInstallWithClientUpgrade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var tag atomic.Value
	tag.Store("v1.0.0")
	var assetBody atomic.Value
	assetBody.Store([]byte("version one"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag.Load().(string),
				"assets": []map[string]string{{
					"name":                 "widget",
					"browser_download_url": "http://" + r.Host + "/assets/widget",
				}},
			})
			return
		}
		if r.URL.Path == "/assets/widget" {
			_, _ = w.Write(assetBody.Load().([]byte))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("first install: %v", err)
	}

	tag.Store("v2.0.0")
	assetBody.Store([]byte("version two"))
	output.Reset()
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if got, want := output.String(), "Upgraded widget v1.0.0 -> v2.0.0\n"; got != want {
		t.Fatalf("upgrade output = %q, want %q", got, want)
	}
	if got, err := os.ReadFile(installedRealPath(t, "acme/widget")); err != nil || string(got) != "version two" {
		t.Fatalf("upgraded real binary = %q, err = %v", got, err)
	}
}

func TestRunInstallWithClientRollsBackUpgradeWhenStateSaveFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var tag atomic.Value
	tag.Store("v1.0.0")
	var assetBody atomic.Value
	assetBody.Store([]byte("version one"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag.Load().(string),
				"assets": []map[string]string{{
					"name":                 "widget",
					"browser_download_url": "http://" + r.Host + "/assets/widget",
				}},
			})
			return
		}
		if r.URL.Path == "/assets/widget" {
			_, _ = w.Write(assetBody.Load().([]byte))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	target := filepath.Join(home, ".local", "bin", "widget")
	realBin := installedRealPath(t, "acme/widget")
	oldWrapper, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read old wrapper: %v", err)
	}
	oldBinary, err := os.ReadFile(realBin)
	if err != nil {
		t.Fatalf("read old binary: %v", err)
	}
	statePath, err := state.Path()
	if err != nil {
		t.Fatalf("state path: %v", err)
	}
	oldState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read old state: %v", err)
	}

	originalPersistState := persistState
	t.Cleanup(func() { persistState = originalPersistState })
	persistState = func(*state.State) error {
		return errors.New("injected state save failure")
	}
	tag.Store("v2.0.0")
	assetBody.Store([]byte("version two"))

	err = runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "save state") {
		t.Fatalf("upgrade error = %v, want state save failure", err)
	}
	if strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("upgrade rollback unexpectedly failed: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, oldWrapper) {
		t.Fatalf("wrapper after failed upgrade = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(realBin); err != nil || !bytes.Equal(got, oldBinary) {
		t.Fatalf("binary after failed upgrade = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(statePath); err != nil || !bytes.Equal(got, oldState) {
		t.Fatalf("state after failed upgrade = %q, err = %v", got, err)
	}
}

func TestRunInstallWithClientRejectsForeignTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	target := filepath.Join(home, ".local", "bin", "widget")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("foreign"), 0644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{{
					"name":                 "widget",
					"browser_download_url": "http://" + r.Host + "/assets/widget",
				}},
			})
			return
		}
		if r.URL.Path == "/assets/widget" {
			_, _ = io.WriteString(w, "replacement")
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite foreign file") {
		t.Fatalf("foreign target error = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "foreign" {
		t.Fatalf("foreign target changed to %q", got)
	}
}

func TestRunInstallWithClientRejectsInvalidRepository(t *testing.T) {
	err := runInstallWithClient(context.Background(), github.New(), "acme/widget/extra", "", "", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "expected org/repo") {
		t.Fatalf("invalid repository error = %v", err)
	}
}

func tarGzAsset(t *testing.T, name string, contents []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
func TestWrapperRecordsUsageWhenEnabled(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	// Enable usage tracking.
	cfg := &config.Config{UsageTracking: true}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	binary := []byte("#!/bin/sh\necho ran\nexit 42\n")
	asset := tarGzAsset(t, "widget", binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
				}},
			})
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}

	wrapperPath := filepath.Join(home, ".local", "bin", "widget")
	cmd := exec.Command("/bin/sh", wrapperPath)
	output, _ := cmd.CombinedOutput()
	if string(output) != "ran\n" {
		t.Fatalf("wrapper output = %q, want %q", output, "ran\n")
	}
	if cmd.ProcessState.ExitCode() != 42 {
		t.Fatalf("wrapper exit code = %d, want 42", cmd.ProcessState.ExitCode())
	}

	usageLog, err := wrapper.UsageLogPath("acme/widget")
	if err != nil {
		t.Fatalf("usage log path: %v", err)
	}
	data, err := os.ReadFile(usageLog)
	if err != nil {
		t.Fatalf("read usage log: %v", err)
	}
	if !strings.Contains(string(data), `"exit_code":42`) {
		t.Fatalf("usage log missing exit code 42: %s", data)
	}
}

func TestRunInstallWithClientAssetOverride(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	darwinBinary := []byte("#!/bin/sh\necho darwin\n")
	linuxBinary := []byte("#!/bin/sh\necho linux\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]string{
					{
						"name":                 "widget-linux-amd64.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget-linux-amd64.tar.gz",
					},
					{
						"name":                 "widget-darwin-arm64.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget-darwin-arm64.tar.gz",
					},
				},
			})
		case "/assets/widget-darwin-arm64.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(tarGzAsset(t, "widget", darwinBinary))
		case "/assets/widget-linux-amd64.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(tarGzAsset(t, "widget", linuxBinary))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "widget-darwin-arm64.tar.gz", "", "", "", &output); err != nil {
		t.Fatalf("install with --asset override: %v", err)
	}
	if !strings.Contains(output.String(), "Installed widget v1.2.3 to ") {
		t.Fatalf("output = %q", output.String())
	}

	realBin := installedRealPath(t, "acme/widget")
	gotBinary, err := os.ReadFile(realBin)
	if err != nil {
		t.Fatalf("read installed real binary: %v", err)
	}
	if !bytes.Equal(gotBinary, darwinBinary) {
		t.Fatalf("installed real binary = %q, want %q", gotBinary, darwinBinary)
	}
}

func TestWrapperForwardsWithoutTrackingWhenDisabled(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	binary := []byte("#!/bin/sh\necho hi\nexit 0\n")
	asset := tarGzAsset(t, "widget", binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
				}},
			})
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}

	wrapperPath := filepath.Join(home, ".local", "bin", "widget")
	cmd := exec.Command("/bin/sh", wrapperPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run wrapper: %v, output: %s", err, output)
	}
	if string(output) != "hi\n" {
		t.Fatalf("wrapper output = %q, want %q", output, "hi\n")
	}

	usageLog, err := wrapper.UsageLogPath("acme/widget")
	if err != nil {
		t.Fatalf("usage log path: %v", err)
	}
	if _, err := os.Stat(usageLog); !os.IsNotExist(err) {
		t.Fatalf("usage log should not exist: %v", err)
	}
}

func TestRunInstallWithClientSidecarChecksum(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho verified\n")
	asset := tarGzAsset(t, "widget", binary)
	digest := sha256.Sum256(asset)
	checksum := strings.ToLower(hex.EncodeToString(digest[:])) + "\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{
					{
						"name":                 "widget.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
					},
					{
						"name":                 "widget.tar.gz.sha256",
						"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz.sha256",
					},
				},
			})
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		case "/assets/widget.tar.gz.sha256":
			_, _ = w.Write([]byte(checksum))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(output.String(), "Verified SHA-256 checksum for widget.tar.gz") {
		t.Fatalf("output missing verification line: %q", output.String())
	}
}

func TestRunInstallWithClientAggregateChecksum(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho verified\n")
	asset := tarGzAsset(t, "widget", binary)
	digest := sha256.Sum256(asset)
	checksums := strings.ToLower(hex.EncodeToString(digest[:])) + "  widget.tar.gz\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{
					{
						"name":                 "widget.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
					},
					{
						"name":                 "checksums.txt",
						"browser_download_url": "http://" + r.Host + "/assets/checksums.txt",
					},
				},
			})
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		case "/assets/checksums.txt":
			_, _ = w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(output.String(), "Verified SHA-256 checksum for widget.tar.gz") {
		t.Fatalf("output missing verification line: %q", output.String())
	}
}

func TestRunInstallWithClientChecksumMismatchAborts(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho verified\n")
	asset := tarGzAsset(t, "widget", binary)
	checksum := strings.Repeat("0", 64) + "\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{
					{
						"name":                 "widget.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
					},
					{
						"name":                 "widget.tar.gz.sha256",
						"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz.sha256",
					},
				},
			})
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		case "/assets/widget.tar.gz.sha256":
			_, _ = w.Write([]byte(checksum))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err == nil {
		t.Fatal("expected checksum mismatch error")
	} else if !strings.Contains(err.Error(), "checksum mismatch for \"widget.tar.gz\"") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); !os.IsNotExist(err) {
		t.Fatalf("wrapper should not be installed after mismatch")
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, ok := st.Get("acme/widget"); ok {
		t.Fatal("state should not contain acme/widget after mismatch")
	}
}

func TestRunInstallWithClientMissingChecksumProceeds(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho hi\n")
	asset := tarGzAsset(t, "widget", binary)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{
					{
						"name":                 "widget.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
					},
				},
			})
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(output.String(), "Installed widget v1.0.0 to ") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunInstallWithClientAssetOverrideUsesChecksum(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	linuxBinary := []byte("#!/bin/sh\necho linux\n")
	linuxAsset := tarGzAsset(t, "widget", linuxBinary)
	linuxDigest := sha256.Sum256(linuxAsset)
	darwinBinary := []byte("#!/bin/sh\necho darwin\n")
	darwinAsset := tarGzAsset(t, "widget", darwinBinary)
	darwinDigest := sha256.Sum256(darwinAsset)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]string{
					{
						"name":                 "widget-linux-amd64.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget-linux-amd64.tar.gz",
					},
					{
						"name":                 "widget-linux-amd64.tar.gz.sha256",
						"browser_download_url": "http://" + r.Host + "/assets/widget-linux-amd64.tar.gz.sha256",
					},
					{
						"name":                 "widget-darwin-arm64.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget-darwin-arm64.tar.gz",
					},
					{
						"name":                 "widget-darwin-arm64.tar.gz.sha256",
						"browser_download_url": "http://" + r.Host + "/assets/widget-darwin-arm64.tar.gz.sha256",
					},
				},
			})
		case "/assets/widget-darwin-arm64.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(darwinAsset)
		case "/assets/widget-darwin-arm64.tar.gz.sha256":
			_, _ = w.Write([]byte(strings.ToLower(hex.EncodeToString(darwinDigest[:])) + "\n"))
		case "/assets/widget-linux-amd64.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(linuxAsset)
		case "/assets/widget-linux-amd64.tar.gz.sha256":
			_, _ = w.Write([]byte(strings.ToLower(hex.EncodeToString(linuxDigest[:])) + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "widget-darwin-arm64.tar.gz", "", "", "", &output); err != nil {
		t.Fatalf("install with --asset override: %v", err)
	}
	if !strings.Contains(output.String(), "Verified SHA-256 checksum for widget-darwin-arm64.tar.gz") {
		t.Fatalf("output missing verification line: %q", output.String())
	}

	realBin := installedRealPath(t, "acme/widget")
	gotBinary, err := os.ReadFile(realBin)
	if err != nil {
		t.Fatalf("read installed real binary: %v", err)
	}
	if !bytes.Equal(gotBinary, darwinBinary) {
		t.Fatalf("installed real binary = %q, want %q", gotBinary, darwinBinary)
	}
}

func TestRunInstallWithClientUsesConfiguredInstallDir(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	cfg := &config.Config{InstallDir: "~/bin"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	binary := []byte("#!/bin/sh\necho widget\n")
	asset := tarGzAsset(t, "widget", binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
				}},
			})
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("install: %v", err)
	}

	target := filepath.Join(home, "bin", "widget")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("wrapper not at configured install_dir: %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state missing acme/widget")
	}
	if app.InstallPath != target {
		t.Errorf("InstallPath = %q, want %q", app.InstallPath, target)
	}

	defaultTarget := filepath.Join(home, ".local", "bin", "widget")
	if _, err := os.Stat(defaultTarget); !os.IsNotExist(err) {
		t.Fatalf("wrapper should not be created at default path %q", defaultTarget)
	}
}

func TestRunInstallWithClientUpgradeMovesWrapperOnInstallDirChange(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var tag atomic.Value
	tag.Store("v1.0.0")
	var assetBody atomic.Value
	assetBody.Store([]byte("version one"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag.Load().(string),
				"assets": []map[string]string{{
					"name":                 "widget",
					"browser_download_url": "http://" + r.Host + "/assets/widget",
				}},
			})
			return
		}
		if r.URL.Path == "/assets/widget" {
			_, _ = w.Write(assetBody.Load().([]byte))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("first install: %v", err)
	}

	oldTarget := filepath.Join(home, ".local", "bin", "widget")
	newTarget := filepath.Join(home, "bin", "widget")

	cfg := &config.Config{InstallDir: "~/bin"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save new install_dir: %v", err)
	}

	tag.Store("v2.0.0")
	assetBody.Store([]byte("version two"))
	output.Reset()
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if got, want := output.String(), "Upgraded widget v1.0.0 -> v2.0.0\n"; got != want {
		t.Fatalf("upgrade output = %q, want %q", got, want)
	}

	if _, err := os.Stat(newTarget); err != nil {
		t.Fatalf("new wrapper missing: %v", err)
	}
	if _, err := os.Stat(oldTarget); !os.IsNotExist(err) {
		t.Fatalf("old wrapper should be removed after install_dir change: %q", oldTarget)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state missing acme/widget")
	}
	if app.InstallPath != newTarget {
		t.Errorf("InstallPath = %q, want %q", app.InstallPath, newTarget)
	}
}

func TestRunInstallWithClientUpgradeLeavesForeignWrapperBehind(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var tag atomic.Value
	tag.Store("v1.0.0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag.Load().(string),
				"assets": []map[string]string{{
					"name":                 "widget",
					"browser_download_url": "http://" + r.Host + "/assets/widget",
				}},
			})
			return
		}
		if r.URL.Path == "/assets/widget" {
			_, _ = w.Write([]byte("version one"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("first install: %v", err)
	}

	oldTarget := filepath.Join(home, ".local", "bin", "widget")
	foreignContent := []byte("foreign")
	if err := os.WriteFile(oldTarget, foreignContent, 0644); err != nil {
		t.Fatalf("replace old wrapper with foreign file: %v", err)
	}

	cfg := &config.Config{InstallDir: "~/bin"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save new install_dir: %v", err)
	}

	tag.Store("v2.0.0")
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	got, err := os.ReadFile(oldTarget)
	if err != nil {
		t.Fatalf("read old path: %v", err)
	}
	if !bytes.Equal(got, foreignContent) {
		t.Fatalf("foreign file was modified: %q", got)
	}
}

func TestRunInstallWithClientPinnedExactTag(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho pinned\n")
	asset := tarGzAsset(t, "widget", binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.2.3":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
				}},
			})
		case "/repos/acme/widget/releases/latest":
			t.Errorf("unexpected latest release request")
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "v1.2.3", "", "", &output); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(output.String(), "Installed widget v1.2.3 to ") {
		t.Fatalf("output = %q", output.String())
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state missing acme/widget")
	}
	if app.Version != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", app.Version)
	}
}

func TestRunInstallWithClientPinnedContainsFallback(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho fallback\n")
	asset := tarGzAsset(t, "widget", binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.2":
			http.NotFound(w, r)
		case "/repos/acme/widget/releases":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"tag_name":     "v1.2.3",
					"published_at": "2026-08-25T10:00:00Z",
					"assets": []map[string]string{{
						"name":                 "widget.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
					}},
				},
				{
					"tag_name":     "v1.2.2",
					"published_at": "2026-08-26T10:00:00Z",
					"assets": []map[string]string{{
						"name":                 "widget.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
					}},
				},
			})
		case "/assets/widget.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "v1.2", "", "", &output); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(output.String(), "Installed widget v1.2.2 to ") {
		t.Fatalf("output = %q", output.String())
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state missing acme/widget")
	}
	if app.Version != "v1.2.2" {
		t.Fatalf("version = %q, want v1.2.2", app.Version)
	}
}

func TestRunInstallWithClientPinnedNoMatchDoesNotModifyState(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/missing":
			http.NotFound(w, r)
		case "/repos/acme/widget/releases":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"tag_name":     "v1.0.0",
					"published_at": "2026-08-25T10:00:00Z",
					"assets":       []map[string]string{},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "missing", "", "", io.Discard); err == nil {
		t.Fatal("expected no match error")
	} else if !strings.Contains(err.Error(), "no release tag matches") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); !os.IsNotExist(err) {
		t.Fatalf("wrapper should not be created after failed tag lookup")
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, ok := st.Get("acme/widget"); ok {
		t.Fatal("state should not contain acme/widget after failed tag lookup")
	}
}

func TestRunInstallWithClientPinnedAlreadyInstalled(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho pinned\n")
	asset := tarGzAsset(t, "widget", binary)
	var assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.2.3":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
				}},
			})
		case "/assets/widget.tar.gz":
			assetRequests.Add(1)
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "v1.2.3", "", "", io.Discard); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if got, want := assetRequests.Load(), int32(1); got != want {
		t.Fatalf("asset requests after first install = %d, want %d", got, want)
	}

	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "v1.2.3", "", "", &output); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got, want := output.String(), "widget v1.2.3 is already installed\n"; got != want {
		t.Fatalf("second output = %q, want %q", got, want)
	}
	if got, want := assetRequests.Load(), int32(1); got != want {
		t.Fatalf("asset requests after second install = %d, want %d", got, want)
	}
}

func TestRunInstallWithClientPinnedUpgrade(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	v1Binary := []byte("#!/bin/sh\necho v1\n")
	v1Asset := tarGzAsset(t, "widget", v1Binary)
	v2Binary := []byte("#!/bin/sh\necho v2\n")
	v2Asset := tarGzAsset(t, "widget", v2Binary)

	var v123Requests, v200Requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/widget/releases/tags/v1.2.3":
			v123Requests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz?v=1.2.3",
				}},
			})
		case r.URL.Path == "/repos/acme/widget/releases/tags/v2.0.0":
			v200Requests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v2.0.0",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz?v=2.0.0",
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/assets/widget.tar.gz"):
			w.Header().Set("Content-Type", "application/gzip")
			if strings.Contains(r.URL.RawQuery, "v=2.0.0") {
				_, _ = w.Write(v2Asset)
			} else {
				_, _ = w.Write(v1Asset)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "v1.2.3", "", "", &output); err != nil {
		t.Fatalf("first install: %v", err)
	}

	output.Reset()
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "v2.0.0", "", "", &output); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if got, want := output.String(), "Upgraded widget v1.2.3 -> v2.0.0\n"; got != want {
		t.Fatalf("upgrade output = %q, want %q", got, want)
	}

	realBin := installedRealPath(t, "acme/widget")
	gotBinary, err := os.ReadFile(realBin)
	if err != nil {
		t.Fatalf("read installed real binary: %v", err)
	}
	if !bytes.Equal(gotBinary, v2Binary) {
		t.Fatalf("installed real binary = %q, want %q", gotBinary, v2Binary)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state missing acme/widget")
	}
	if app.Version != "v2.0.0" {
		t.Fatalf("version = %q, want v2.0.0", app.Version)
	}
}

func TestParseSystem(t *testing.T) {
	tests := []struct {
		name     string
		system   string
		wantOS   string
		wantArch string
		errText  string
	}{
		{"empty falls back to host", "", runtime.GOOS, runtime.GOARCH, ""},
		{"linux amd64", "linux/amd64", "linux", "amd64", ""},
		{"darwin arm64", "darwin/arm64", "darwin", "arm64", ""},
		{"windows 386", "windows/386", "windows", "386", ""},
		{"linux arm", "linux/arm", "linux", "arm", ""},
		{"missing slash", "linux", "", "", "invalid --system"},
		{"empty os", "/amd64", "", "", "invalid --system"},
		{"empty arch", "linux/", "", "", "invalid --system"},
		{"too many parts", "linux/amd64/extra", "", "", "invalid --system"},
		{"unsupported os", "plan9/amd64", "", "", "unsupported --system"},
		{"unsupported arch", "linux/mips", "", "", "unsupported --system"},
		{"unsupported both", "plan9/mips", "", "", "unsupported --system"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotOS, gotArch, err := parseSystem(test.system)
			if test.errText != "" {
				if err == nil {
					t.Fatal("parseSystem() error = nil, want error")
				}
				if !strings.Contains(err.Error(), test.errText) {
					t.Errorf("parseSystem() error = %q, want text %q", err, test.errText)
				}
				if strings.Contains(test.errText, "unsupported") {
					if !strings.Contains(err.Error(), "supported os values") || !strings.Contains(err.Error(), "supported arch values") {
						t.Errorf("parseSystem() error should list supported values: %q", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSystem() error = %v", err)
			}
			if gotOS != test.wantOS || gotArch != test.wantArch {
				t.Errorf("parseSystem() = %q/%q, want %q/%q", gotOS, gotArch, test.wantOS, test.wantArch)
			}
		})
	}
}

func TestRunInstallWithClientCrossPlatform(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	linuxBinary := []byte("#!/bin/sh\necho linux amd64\n")
	darwinBinary := []byte("#!/bin/sh\necho darwin arm64\n")

	var requestedAsset atomic.Value
	requestedAsset.Store("")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]string{
					{
						"name":                 "widget-linux-amd64.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget-linux-amd64.tar.gz",
					},
					{
						"name":                 "widget-darwin-arm64.tar.gz",
						"browser_download_url": "http://" + r.Host + "/assets/widget-darwin-arm64.tar.gz",
					},
				},
			})
		case "/assets/widget-linux-amd64.tar.gz":
			requestedAsset.Store("widget-linux-amd64.tar.gz")
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(tarGzAsset(t, "widget", linuxBinary))
		case "/assets/widget-darwin-arm64.tar.gz":
			requestedAsset.Store("widget-darwin-arm64.tar.gz")
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(tarGzAsset(t, "widget", darwinBinary))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "darwin", "arm64", &output); err != nil {
		t.Fatalf("install darwin/arm64: %v", err)
	}
	if !strings.Contains(output.String(), "Installed widget v1.2.3 to ") {
		t.Fatalf("output = %q", output.String())
	}
	if got := requestedAsset.Load().(string); got != "widget-darwin-arm64.tar.gz" {
		t.Fatalf("requested asset = %q, want widget-darwin-arm64.tar.gz", got)
	}

	realBin := installedRealPath(t, "acme/widget")
	gotBinary, err := os.ReadFile(realBin)
	if err != nil {
		t.Fatalf("read installed real binary: %v", err)
	}
	if !bytes.Equal(gotBinary, darwinBinary) {
		t.Fatalf("installed real binary = %q, want %q", gotBinary, darwinBinary)
	}
}

func TestRunInstallSystemFlagMalformed(t *testing.T) {
	cmd := &cobra.Command{Use: "install"}
	cmd.Flags().String("asset", "", "")
	cmd.Flags().String("tag", "", "")
	cmd.Flags().String("system", "", "")
	if err := cmd.Flags().Set("system", "linux"); err != nil {
		t.Fatalf("set --system flag: %v", err)
	}

	err := runInstall(cmd, []string{"acme/widget"})
	if err == nil || !strings.Contains(err.Error(), `invalid --system "linux"`) {
		t.Fatalf("error = %v, want invalid --system", err)
	}
	if strings.Contains(err.Error(), "fetch") {
		t.Fatalf("malformed --system triggered a network fetch: %v", err)
	}
}

func TestRunInstallSystemFlagUnsupported(t *testing.T) {
	cmd := &cobra.Command{Use: "install"}
	cmd.Flags().String("asset", "", "")
	cmd.Flags().String("tag", "", "")
	cmd.Flags().String("system", "", "")
	if err := cmd.Flags().Set("system", "linux/mips"); err != nil {
		t.Fatalf("set --system flag: %v", err)
	}

	err := runInstall(cmd, []string{"acme/widget"})
	if err == nil || !strings.Contains(err.Error(), `unsupported --system "linux/mips"`) {
		t.Fatalf("error = %v, want unsupported --system", err)
	}
	if !strings.Contains(err.Error(), "supported os values") || !strings.Contains(err.Error(), "supported arch values") {
		t.Fatalf("unsupported --system error should list supported values: %v", err)
	}
	if strings.Contains(err.Error(), "fetch") {
		t.Fatalf("unsupported --system triggered a network fetch: %v", err)
	}
}

func TestRunInstallWithMinAgeSelectsNewestEligibleStableRelease(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	now := time.Now().UTC()
	binary := []byte("stable old binary")
	asset := tarGzAsset(t, "widget", binary)
	releasePayload := func(tag string, publishedAt time.Time, draft, prerelease bool) map[string]any {
		return map[string]any{
			"tag_name":     tag,
			"draft":        draft,
			"prerelease":   prerelease,
			"published_at": publishedAt.Format(time.RFC3339),
			"assets": []map[string]string{{
				"name":                 "widget.tar.gz",
				"browser_download_url": "http://" + "unused" + "/assets/widget.tar.gz",
			}},
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases":
			payload := []map[string]any{
				releasePayload("v4.0.0", now.AddDate(0, 0, -90), true, false),
				releasePayload("v3.0.0", now.AddDate(0, 0, -60), false, true),
				releasePayload("v2.0.0", now.Add(-24*time.Hour), false, false),
				releasePayload("v1.0.0", now.AddDate(0, 0, -30), false, false),
			}
			for _, release := range payload {
				release["assets"].([]map[string]string)[0]["browser_download_url"] = "http://" + r.Host + "/assets/widget.tar.gz"
			}
			_ = json.NewEncoder(w).Encode(payload)
		case "/repos/acme/widget/releases/latest":
			t.Errorf("unexpected latest release request")
			http.NotFound(w, r)
		case "/assets/widget.tar.gz":
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithMinAge(context.Background(), client, "acme/widget", "", "", "", "", 7, &output); err != nil {
		t.Fatalf("install with minimum age: %v", err)
	}
	if got := output.String(); !strings.Contains(got, "Installed widget v1.0.0 to ") || !strings.Contains(got, "latest release too young for --min-age-days=7") {
		t.Fatalf("output = %q", got)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state does not contain acme/widget")
	}
	if app.Version != "v1.0.0" {
		t.Fatalf("installed version = %q, want v1.0.0", app.Version)
	}
}

func TestRunInstallWithMinAgeLeavesStateAndTargetUntouchedWhenNoReleaseQualifies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	target := filepath.Join(home, ".local", "bin", "widget")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("existing target"), 0644); err != nil {
		t.Fatal(err)
	}
	st := &state.State{Apps: map[string]state.App{
		"acme/widget": {Version: "v0.9.0", InstallPath: target},
	}}
	if err := st.Save(); err != nil {
		t.Fatalf("save state fixture: %v", err)
	}
	statePath, err := state.Path()
	if err != nil {
		t.Fatalf("state path: %v", err)
	}
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state fixture: %v", err)
	}
	targetBefore, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target fixture: %v", err)
	}

	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/releases" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"tag_name":     "v2.0.0",
				"published_at": now.Add(-24 * time.Hour).Format(time.RFC3339),
				"assets":       []map[string]string{},
			},
			{
				"tag_name":     "v3.0.0-draft",
				"draft":        true,
				"published_at": now.AddDate(0, 0, -90).Format(time.RFC3339),
				"assets":       []map[string]string{},
			},
		})
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	err = runInstallWithMinAge(context.Background(), client, "acme/widget", "", "", "", "", 7, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no release is at least 7 days old") {
		t.Fatalf("error = %v, want no qualifying release error", err)
	}

	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state after failed install: %v", err)
	}
	if !bytes.Equal(stateAfter, stateBefore) {
		t.Fatalf("state changed after failed install: before %q, after %q", stateBefore, stateAfter)
	}
	targetAfter, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target after failed install: %v", err)
	}
	if !bytes.Equal(targetAfter, targetBefore) {
		t.Fatalf("target changed after failed install: before %q, after %q", targetBefore, targetAfter)
	}
}

func TestRunInstallMinAgeFlagValidation(t *testing.T) {
	newCommand := func() *cobra.Command {
		cmd := &cobra.Command{Use: "install"}
		cmd.Flags().String("asset", "", "")
		cmd.Flags().String("tag", "", "")
		cmd.Flags().String("system", "", "")
		cmd.Flags().Int("min-age-days", 0, "")
		return cmd
	}

	for _, value := range []string{"0", "-1"} {
		t.Run("invalid value "+value, func(t *testing.T) {
			cmd := newCommand()
			if err := cmd.Flags().Set("min-age-days", value); err != nil {
				t.Fatalf("set --min-age-days: %v", err)
			}
			err := runInstall(cmd, []string{"acme/widget"})
			if err == nil || !strings.Contains(err.Error(), "--min-age-days must be a positive integer") {
				t.Fatalf("error = %v, want positive integer validation", err)
			}
		})
	}

	cmd := newCommand()
	if err := cmd.Flags().Set("tag", "v1.2.3"); err != nil {
		t.Fatalf("set --tag: %v", err)
	}
	if err := cmd.Flags().Set("min-age-days", "7"); err != nil {
		t.Fatalf("set --min-age-days: %v", err)
	}
	err := runInstall(cmd, []string{"acme/widget"})
	if err == nil || !strings.Contains(err.Error(), "--tag cannot be combined with --min-age-days") {
		t.Fatalf("error = %v, want incompatible flag validation", err)
	}

	if err := runInstallWithMinAge(context.Background(), nil, "acme/widget", "", "v1.2.3", "", "", 7, io.Discard); err == nil || !strings.Contains(err.Error(), "--tag cannot be combined with --min-age-days") {
		t.Fatalf("direct validation error = %v, want incompatibility before client use", err)
	}
}

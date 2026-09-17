package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"zenget/internal/appcfg"
	"zenget/internal/github"
	"zenget/internal/lockfile"
	"zenget/internal/state"
)

type lockTestAsset struct {
	Name   string
	ID     int64
	Data   []byte
	Digest string
}

func writeLockTestRelease(w http.ResponseWriter, serverURL, tag string, assets ...lockTestAsset) {
	responseAssets := make([]map[string]any, 0, len(assets))
	for _, asset := range assets {
		digest := asset.Digest
		if digest == "" {
			digest = ""
		}
		responseAssets = append(responseAssets, map[string]any{
			"id":                   asset.ID,
			"name":                 asset.Name,
			"size":                 len(asset.Data),
			"url":                  serverURL + "/assets/" + asset.Name,
			"browser_download_url": serverURL + "/assets/" + asset.Name,
			"digest":               digest,
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tag_name": tag,
		"assets":   responseAssets,
	})
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func saveLockTestManifest(t *testing.T, repository, tag, assetName string) string {
	t.Helper()
	return saveManifestFile(t, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps: []state.ManifestApp{{
			Repository:    repository,
			Provider:      manifestProviderGitHub,
			Tag:           tag,
			Asset:         assetName,
			PlatformOS:    runtime.GOOS,
			PlatformArch:  runtime.GOARCH,
			ArchiveBinary: "bin/" + repository[strings.IndexByte(repository, '/')+1:],
			TargetName:    "test-" + repository[strings.IndexByte(repository, '/')+1:],
		}},
	})
}

func runTestLock(t *testing.T, manifestPath, outputPath string) error {
	t.Helper()
	command, _, _ := newBufferedCommand()
	command.Flags().String("output", outputPath, "")
	previousForce := lockForce
	lockForce = false
	defer func() { lockForce = previousForce }()
	return runLock(command, []string{manifestPath})
}

func runTestApplyWithLock(t *testing.T, manifestPath, lockPath string, dryRun bool) (string, error) {
	t.Helper()
	command, output, _ := newBufferedCommand()
	command.Flags().String("lock", lockPath, "")
	previousDryRun := applyDryRun
	applyDryRun = dryRun
	defer func() { applyDryRun = previousDryRun }()
	err := runApply(command, []string{manifestPath})
	return output.String(), err
}

func assertNoLockInstallation(t *testing.T, repository string) {
	t.Helper()
	statePath, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state exists after rejected lock apply: %v", err)
	}
	configPath, err := appcfg.Path(repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("app config exists after rejected lock apply: %v", err)
	}
}

func TestRunLockUsesProviderDigestAndIsPortable(t *testing.T) {
	setupManifestEnvironment(t)
	data := []byte("provider-digest-archive")
	digest := sha256Hex(data)
	asset := lockTestAsset{Name: "widget.tar.gz", ID: 42, Data: data, Digest: "sha256:" + digest}
	var releaseRequests, assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.2.3":
			releaseRequests.Add(1)
			writeLockTestRelease(w, serverURLForRequest(r), "v1.2.3", asset)
		case "/assets/widget.tar.gz":
			assetRequests.Add(1)
			http.Error(w, "lock creation should use the provider digest", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveLockTestManifest(t, "acme/widget", "v1.2.3", asset.Name)
	firstPath := filepath.Join(t.TempDir(), "first.lock.json")
	secondPath := filepath.Join(t.TempDir(), "second.lock.json")
	if err := runTestLock(t, manifestPath, firstPath); err != nil {
		t.Fatalf("runLock: %v", err)
	}
	if err := runTestLock(t, manifestPath, secondPath); err != nil {
		t.Fatalf("second runLock: %v", err)
	}
	firstData, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstData, secondData) {
		t.Fatalf("lock output is not deterministic:\n%s\n---\n%s", firstData, secondData)
	}
	locked, err := loadLockFileForTest(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := locked.Apps[0]
	if entry.AssetID != asset.ID || entry.AssetName != asset.Name || entry.AssetSize != int64(len(data)) || entry.ArchiveSHA256 != digest || entry.ProviderDigest != asset.Digest {
		t.Fatalf("lock entry = %#v", entry)
	}
	if got := assetRequests.Load(); got != 0 {
		t.Fatalf("asset requests = %d, want 0 when provider digest and size are present", got)
	}
	if got := releaseRequests.Load(); got != 2 {
		t.Fatalf("release requests = %d, want 2", got)
	}
	assertNoLockInstallation(t, "acme/widget")

	original, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := runTestLock(t, manifestPath, firstPath); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("no-clobber error = %v", err)
	}
	if got, err := os.ReadFile(firstPath); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("lock changed after refused overwrite: %q, %v", got, err)
	}
}

func TestRunLockDownloadsWhenProviderDigestIsMissing(t *testing.T) {
	setupManifestEnvironment(t)
	data := []byte("download-and-hash-this-archive")
	asset := lockTestAsset{Name: "widget.tar.gz", ID: 7, Data: data}
	var assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.0.0":
			writeLockTestRelease(w, serverURLForRequest(r), "v1.0.0", asset)
		case "/assets/widget.tar.gz":
			assetRequests.Add(1)
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveLockTestManifest(t, "acme/widget", "v1.0.0", asset.Name)
	outputPath := filepath.Join(t.TempDir(), "widget.lock.json")
	if err := runTestLock(t, manifestPath, outputPath); err != nil {
		t.Fatalf("runLock: %v", err)
	}
	locked, err := loadLockFileForTest(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := locked.Apps[0]
	if entry.ProviderDigest != "" || entry.ArchiveSHA256 != sha256Hex(data) || entry.AssetSize != int64(len(data)) {
		t.Fatalf("downloaded lock entry = %#v", entry)
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests = %d, want 1", got)
	}
	assertNoLockInstallation(t, "acme/widget")
}

func TestRunLockRejectsAssetWithoutProviderID(t *testing.T) {
	setupManifestEnvironment(t)
	asset := lockTestAsset{Name: "widget.tar.gz", ID: 0, Data: []byte("asset")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/tags/v1.0.0" {
			writeLockTestRelease(w, serverURLForRequest(r), "v1.0.0", asset)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveLockTestManifest(t, "acme/widget", "v1.0.0", asset.Name)
	outputPath := filepath.Join(t.TempDir(), "invalid.lock.json")
	if err := runTestLock(t, manifestPath, outputPath); err == nil || !strings.Contains(err.Error(), "no provider asset ID") {
		t.Fatalf("runLock error = %v", err)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("invalid lock output exists: %v", err)
	}
	assertNoLockInstallation(t, "acme/widget")
}

func TestRunLockValidatesLocalOutputBeforeProviderWork(t *testing.T) {
	setupManifestEnvironment(t)
	manifestPath := saveLockTestManifest(t, "acme/widget", "v1.0.0", "widget.tar.gz")
	originalFactory := manifestClientFactory
	manifestClientFactory = func(string) (*github.Client, error) {
		t.Fatal("provider factory was called before lock output validation")
		return nil, nil
	}
	t.Cleanup(func() { manifestClientFactory = originalFactory })

	missingOutputCommand, _, _ := newBufferedCommand()
	if err := runLock(missingOutputCommand, []string{manifestPath}); err == nil || !strings.Contains(err.Error(), "--output is required") {
		t.Fatalf("missing output error = %v", err)
	}
	remoteOutputCommand, _, _ := newBufferedCommand()
	remoteOutputCommand.Flags().String("output", "https://example.test/lock.json", "")
	if err := runLock(remoteOutputCommand, []string{manifestPath}); err == nil || !strings.Contains(err.Error(), "local path") {
		t.Fatalf("remote output error = %v", err)
	}
}

func TestLockHelpersRejectInvalidInputs(t *testing.T) {
	if _, err := buildLockfile(context.Background(), nil, state.Manifest{}); err == nil {
		t.Fatal("buildLockfile(nil client) error = nil")
	}
	if _, _, err := normalizedProviderDigest("sha512:" + strings.Repeat("a", 64)); err == nil {
		t.Fatal("unsupported provider digest error = nil")
	}
	if _, _, err := lockArtifactDigest(context.Background(), github.New(), &github.Release{}, github.Asset{Name: "widget"}, ""); err == nil || !strings.Contains(err.Error(), "no download URL") {
		t.Fatalf("lockArtifactDigest() error = %v", err)
	}

	failedPlan := []applyPlanEntry{{manifest: state.ManifestApp{Repository: "acme/widget"}, status: applyFailed, reason: "release unavailable"}}
	if _, err := preflightLockedAssets(nil, nil, failedPlan, lockfile.Lockfile{}); err == nil || !strings.Contains(err.Error(), "release unavailable") { //nolint:staticcheck // this test covers nil-context normalization
		t.Fatalf("failed plan preflight error = %v", err)
	}
	missingLockPlan := []applyPlanEntry{{manifest: state.ManifestApp{Repository: "acme/widget"}, status: applyInstall}}
	if _, err := preflightLockedAssets(nil, nil, missingLockPlan, lockfile.Lockfile{}); err == nil || !strings.Contains(err.Error(), "lock entry is missing") { //nolint:staticcheck // this test covers nil-context normalization
		t.Fatalf("missing lock entry preflight error = %v", err)
	}
}

func TestApplyWithLockPreflightsAndReusesArtifactCache(t *testing.T) {
	setupManifestEnvironment(t)
	binary := []byte("#!/bin/sh\necho locked\n")
	archive := tarGzAsset(t, "bin/widget", binary)
	digest := sha256Hex(archive)
	asset := lockTestAsset{Name: "widget.tar.gz", ID: 42, Data: archive, Digest: "sha256:" + digest}
	var assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.2.3":
			writeLockTestRelease(w, serverURLForRequest(r), "v1.2.3", asset)
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
	manifestPath := saveLockTestManifest(t, "acme/widget", "v1.2.3", asset.Name)
	lockPath := filepath.Join(t.TempDir(), "widget.lock.json")
	if err := runTestLock(t, manifestPath, lockPath); err != nil {
		t.Fatalf("create lock: %v", err)
	}
	assetRequests.Store(0)
	output, err := runTestApplyWithLock(t, manifestPath, lockPath, false)
	if err != nil {
		t.Fatalf("locked apply: %v", err)
	}
	if !strings.Contains(output, "install acme/widget v1.2.3") {
		t.Fatalf("locked apply output = %q", output)
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests = %d, want one preflight download", got)
	}
	installed, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, ok := installed.Get("acme/widget")
	if !ok || app.Version != "v1.2.3" || app.RealPath == "" {
		t.Fatalf("installed state = %#v", app)
	}
	if got, err := os.ReadFile(app.RealPath); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("installed binary = %q, err = %v", got, err)
	}
	if err := os.Remove(app.RealPath); err != nil {
		t.Fatalf("remove artifact before locked cache restore: %v", err)
	}
	if _, err := runTestApplyWithLock(t, manifestPath, lockPath, false); err != nil {
		t.Fatalf("locked cache restore: %v", err)
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests after locked cache restore = %d, want 1", got)
	}
}

func TestRunApplyWithLockRejectsArchiveHashBeforeMutation(t *testing.T) {
	setupManifestEnvironment(t)
	goodArchive := tarGzAsset(t, "bin/widget", []byte("#!/bin/sh\necho good\n"))
	badArchive := tarGzAsset(t, "bin/widget", []byte("#!/bin/sh\necho bad!\n"))
	if len(goodArchive) != len(badArchive) {
		t.Fatalf("test archives have different sizes: %d and %d", len(goodArchive), len(badArchive))
	}
	asset := lockTestAsset{Name: "widget.tar.gz", ID: 9, Data: goodArchive, Digest: "sha256:" + sha256Hex(goodArchive)}
	serveGood := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.0.0":
			writeLockTestRelease(w, serverURLForRequest(r), "v1.0.0", asset)
		case "/assets/widget.tar.gz":
			if serveGood {
				_, _ = w.Write(goodArchive)
			} else {
				_, _ = w.Write(badArchive)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveLockTestManifest(t, "acme/widget", "v1.0.0", asset.Name)
	lockPath := filepath.Join(t.TempDir(), "widget.lock.json")
	if err := runTestLock(t, manifestPath, lockPath); err != nil {
		t.Fatalf("create lock: %v", err)
	}
	serveGood = false
	output, err := runTestApplyWithLock(t, manifestPath, lockPath, false)
	if err == nil || !strings.Contains(err.Error(), "lock artifact preflight failed") {
		t.Fatalf("hash rejection error/output = %v / %q", err, output)
	}
	assertNoLockInstallation(t, "acme/widget")
}

func TestRunApplyWithLockRejectsProviderMetadataDrift(t *testing.T) {
	setupManifestEnvironment(t)
	data := []byte("metadata-drift-archive")
	asset := lockTestAsset{Name: "widget.tar.gz", ID: 11, Data: data, Digest: "sha256:" + sha256Hex(data)}
	currentAsset := asset
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.0.0":
			writeLockTestRelease(w, serverURLForRequest(r), "v1.0.0", currentAsset)
		case "/assets/widget.tar.gz":
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveLockTestManifest(t, "acme/widget", "v1.0.0", asset.Name)
	lockPath := filepath.Join(t.TempDir(), "widget.lock.json")
	if err := runTestLock(t, manifestPath, lockPath); err != nil {
		t.Fatalf("create lock: %v", err)
	}
	currentAsset.ID++
	output, err := runTestApplyWithLock(t, manifestPath, lockPath, false)
	if err == nil || !strings.Contains(err.Error(), "provider asset metadata differs") || !strings.Contains(output, "install acme/widget v1.0.0") {
		t.Fatalf("metadata drift error/output = %v / %q", err, output)
	}
	assertNoLockInstallation(t, "acme/widget")
}

func TestRunApplyWithLockDryRunLeavesContentPending(t *testing.T) {
	setupManifestEnvironment(t)
	data := []byte("dry-run-lock-archive")
	asset := lockTestAsset{Name: "widget.tar.gz", ID: 13, Data: data, Digest: "sha256:" + sha256Hex(data)}
	var assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/v1.0.0":
			writeLockTestRelease(w, serverURLForRequest(r), "v1.0.0", asset)
		case "/assets/widget.tar.gz":
			assetRequests.Add(1)
			http.Error(w, "dry-run must not fetch content", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	useManifestClient(t, client)
	manifestPath := saveLockTestManifest(t, "acme/widget", "v1.0.0", asset.Name)
	lockPath := filepath.Join(t.TempDir(), "widget.lock.json")
	if err := runTestLock(t, manifestPath, lockPath); err != nil {
		t.Fatalf("create lock: %v", err)
	}
	output, err := runTestApplyWithLock(t, manifestPath, lockPath, true)
	if err != nil {
		t.Fatalf("locked dry-run: %v", err)
	}
	if !strings.Contains(output, "Lock content verification: pending for 1 artifact(s)") {
		t.Fatalf("dry-run output = %q", output)
	}
	if got := assetRequests.Load(); got != 0 {
		t.Fatalf("asset requests = %d, want 0", got)
	}
	assertNoLockInstallation(t, "acme/widget")
}

func TestRunApplyWithLockPreflightsAllArtifactsBeforeMutation(t *testing.T) {
	setupManifestEnvironment(t)
	first := tarGzAsset(t, "bin/alpha", []byte("#!/bin/sh\necho alpha\n"))
	second := tarGzAsset(t, "bin/widget", []byte("#!/bin/sh\necho widget\n"))
	firstAsset := lockTestAsset{Name: "alpha.tar.gz", ID: 21, Data: first, Digest: "sha256:" + sha256Hex(first)}
	secondAsset := lockTestAsset{Name: "widget.tar.gz", ID: 22, Data: second, Digest: "sha256:" + sha256Hex(second)}
	serveSecondGood := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/alpha/releases/tags/v1.0.0":
			writeLockTestRelease(w, serverURLForRequest(r), "v1.0.0", firstAsset)
		case "/repos/zeta/widget/releases/tags/v2.0.0":
			writeLockTestRelease(w, serverURLForRequest(r), "v2.0.0", secondAsset)
		case "/assets/alpha.tar.gz":
			_, _ = w.Write(first)
		case "/assets/widget.tar.gz":
			if serveSecondGood {
				_, _ = w.Write(second)
			} else {
				_, _ = w.Write([]byte("bad"))
			}
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
			{Repository: "zeta/widget", Provider: manifestProviderGitHub, Tag: "v2.0.0", Asset: secondAsset.Name, PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, ArchiveBinary: "bin/widget", TargetName: "widget"},
			{Repository: "acme/alpha", Provider: manifestProviderGitHub, Tag: "v1.0.0", Asset: firstAsset.Name, PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, ArchiveBinary: "bin/alpha", TargetName: "alpha"},
		},
	})
	lockPath := filepath.Join(t.TempDir(), "multi.lock.json")
	if err := runTestLock(t, manifestPath, lockPath); err != nil {
		t.Fatalf("create lock: %v", err)
	}
	serveSecondGood = false
	if output, err := runTestApplyWithLock(t, manifestPath, lockPath, false); err == nil || !strings.Contains(err.Error(), "lock artifact preflight failed") {
		t.Fatalf("multi-artifact preflight error/output = %v / %q", err, output)
	}
	assertNoLockInstallation(t, "acme/alpha")
	assertNoLockInstallation(t, "zeta/widget")
}

func loadLockFileForTest(path string) (lockfile.Lockfile, error) {
	return lockfile.LoadFile(path)
}

func serverURLForRequest(r *http.Request) string {
	return fmt.Sprintf("http://%s", r.Host)
}

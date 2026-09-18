package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zenget/internal/artifactcache"
	"zenget/internal/config"
	"zenget/internal/manifest"
	"zenget/internal/provider"
	"zenget/internal/state"
	"zenget/internal/trust"
)

type lazyTestProvider struct {
	release       *provider.Release
	releaseErr    error
	providerName  string
	releaseHook   func()
	downloadHook  func(string)
	body          []byte
	downloads     atomic.Int32
	latestCalls   atomic.Int32
	byTagCalls    atomic.Int32
	downloadLimit atomic.Int64
	downloadErr   error
	downloadStart chan struct{}
	downloadGate  <-chan struct{}
	once          sync.Once
}

func (p *lazyTestProvider) ProviderName() string {
	if p.providerName != "" {
		return p.providerName
	}
	return manifest.ProviderGitHub
}

func (p *lazyTestProvider) LatestRelease(context.Context, string, string) (*provider.Release, error) {
	p.latestCalls.Add(1)
	return p.release, nil
}

func (p *lazyTestProvider) ReleaseByTag(_ context.Context, _, _, tag string) (*provider.Release, error) {
	p.byTagCalls.Add(1)
	if p.releaseErr != nil {
		return nil, p.releaseErr
	}
	if p.releaseHook != nil {
		hook := p.releaseHook
		p.releaseHook = nil
		hook()
	}
	if p.release == nil || p.release.TagName != tag {
		return p.release, nil
	}
	return p.release, nil
}

func TestLazyProviderFactoryUsesGitHubClient(t *testing.T) {
	client, err := lazyProviderFactory()
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || providerNameFor(client) != manifest.ProviderGitHub {
		t.Fatalf("lazy provider = %#v", client)
	}
}

func (p *lazyTestProvider) ListReleases(context.Context, string, string) ([]provider.Release, error) {
	return nil, errors.New("lazy tests must not list releases")
}

func (p *lazyTestProvider) DownloadAsset(ctx context.Context, _ provider.Asset, destination string, maxBytes ...int64) error {
	p.downloads.Add(1)
	p.once.Do(func() {
		if p.downloadStart != nil {
			close(p.downloadStart)
		}
	})
	if p.downloadGate != nil {
		select {
		case <-p.downloadGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p.downloadErr != nil {
		return p.downloadErr
	}
	if len(maxBytes) > 0 {
		p.downloadLimit.Store(maxBytes[0])
		if int64(len(p.body)) > maxBytes[0] {
			return fmt.Errorf("test provider download exceeds limit %d", maxBytes[0])
		}
	}
	if err := os.WriteFile(destination, p.body, 0644); err != nil {
		return err
	}
	if p.downloadHook != nil {
		hook := p.downloadHook
		p.downloadHook = nil
		hook(destination)
	}
	return nil
}

func configureLazyProject(t *testing.T, body []byte) (string, *lazyTestProvider) {
	t.Helper()
	setupManifestEnvironment(t)
	project := safeProjectDirectory(t)
	manifestPath := filepath.Join(project, "zenget.json")
	digest := sha256.Sum256(body)
	writeCommandManifest(t, manifestPath, state.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Apps: []state.ManifestApp{{
			Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0",
			Asset: "tool", TargetName: "tool",
		}},
	})
	client := &lazyTestProvider{
		body: body,
		release: &provider.Release{
			TagName: "v1.0.0",
			Assets: []provider.Asset{{
				ID: 1, Name: "tool", Digest: "sha256:" + hex.EncodeToString(digest[:]),
			}},
		},
	}
	if err := (&config.Config{LazyInstall: true}).Save(); err != nil {
		t.Fatal(err)
	}
	store := trust.New()
	if _, _, err := store.Add(manifestPath); err != nil {
		t.Fatalf("trust project: %v", err)
	}
	if err := trust.Save(store); err != nil {
		t.Fatalf("save trust: %v", err)
	}
	t.Setenv("ZENGET_MANIFEST", manifestPath)
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "")
	originalFactory := lazyProviderFactory
	lazyProviderFactory = func() (provider.Provider, error) { return client, nil }
	t.Cleanup(func() { lazyProviderFactory = originalFactory })
	return manifestPath, client
}

func TestLazyInstallRunsExactProjectArtifactAndDoesNotWriteGlobalState(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "argv.txt")
	body := []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$0\" \"$1\" \"$ZENGET_LAZY_TEST\" > \"" + outputPath + "\"\nexit 17\n")
	_, client := configureLazyProject(t, body)
	t.Setenv("ZENGET_LAZY_TEST", "preserved")
	if _, err := ResolveTarget("tool", TargetResolveOptions{}); err != nil {
		t.Fatal(err)
	}
	err := dispatchTarget(context.Background(), "tool", []string{"first"})
	var exitErr *ShimExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 17 {
		t.Fatalf("dispatchTarget() error = %v, want exit code 17", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "|first|preserved") {
		t.Fatalf("lazy artifact did not preserve argv/environment: %q", data)
	}
	if client.downloads.Load() != 1 || client.latestCalls.Load() != 0 || client.byTagCalls.Load() == 0 {
		t.Fatalf("provider calls = downloads %d, latest %d, exact %d", client.downloads.Load(), client.latestCalls.Load(), client.byTagCalls.Load())
	}
	statePath, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("lazy install changed global state: %v", err)
	}
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := cache.List(artifactcache.Protection{})
	if err != nil || len(entries) != 1 || entries[0].Integrity != artifactcache.IntegrityValid {
		t.Fatalf("lazy cache entries = %#v, err %v", entries, err)
	}
}

func TestLazyInstallUsesVerifiedCacheWithoutReleaseLookup(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err != nil {
		t.Fatalf("initial lazy install: %v", err)
	}

	originalFactory := lazyProviderFactory
	lazyProviderFactory = func() (provider.Provider, error) {
		return nil, errors.New("provider must not be called for a cache hit")
	}
	t.Cleanup(func() { lazyProviderFactory = originalFactory })
	result, err := lazyInstallProjectTarget(context.Background(), selection)
	if err != nil {
		t.Fatalf("cached lazy install: %v", err)
	}
	if result.Integrity != artifactcache.IntegrityValid || result.ArtifactPath == "" {
		t.Fatalf("cached lazy selection = %#v", result)
	}
	if client.downloads.Load() != 1 || client.byTagCalls.Load() != 1 {
		t.Fatalf("cached provider calls = downloads %d, exact %d", client.downloads.Load(), client.byTagCalls.Load())
	}
}

func TestLazyInstallRejectsInvalidProjectState(t *testing.T) {
	if _, err := lazyInstallProjectTarget(context.Background(), TargetSelection{Source: selectionGlobal}); err == nil || !strings.Contains(err.Error(), "requires a project target") {
		t.Fatalf("non-project selection error = %v", err)
	}

	t.Run("manifest reload failure", testLazyInstallManifestReloadFailure)
	t.Run("manifest path changed", testLazyInstallManifestPathChanged)
	t.Run("target missing", testLazyInstallTargetMissing)
	t.Run("ambiguous target", testLazyInstallAmbiguousTarget)
}

func testLazyInstallManifestReloadFailure(t *testing.T) {
	manifestPath, _ := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(nil, selection); err == nil || !strings.Contains(err.Error(), "reload project manifest") { //nolint:staticcheck // this test covers nil-context normalization
		t.Fatalf("reload error = %v", err)
	}
}

func testLazyInstallManifestPathChanged(t *testing.T) {
	manifestPath, _ := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	selection.ManifestPath = filepath.Dir(manifestPath) + string(os.PathSeparator) + "." + string(os.PathSeparator) + "zenget.json"
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "path changed") {
		t.Fatalf("changed-path error = %v", err)
	}
}

func testLazyInstallTargetMissing(t *testing.T) {
	manifestPath, _ := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	writeCommandManifest(t, manifestPath, state.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/other", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool"}},
	})
	selection := TargetSelection{Source: selectionProject, ManifestPath: manifestPath, Target: "tool"}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("missing target error = %v", err)
	}
}

func testLazyInstallAmbiguousTarget(t *testing.T) {
	manifestPath, _ := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	writeCommandManifest(t, manifestPath, state.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Apps: []state.ManifestApp{
			{Repository: "acme/one", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool", TargetName: "tool"},
			{Repository: "acme/two", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool", TargetName: "tool"},
		},
	})
	selection := TargetSelection{Source: selectionProject, ManifestPath: manifestPath, Target: "tool"}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "matches multiple") {
		t.Fatalf("ambiguous target error = %v", err)
	}
}

func TestLazyInstallRejectsProviderAndReleaseFailures(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*lazyTestProvider)
		factoryErr  error
		nilProvider bool
		want        string
	}{
		{name: "provider factory", factoryErr: errors.New("provider setup failed"), want: "configure GitHub provider"},
		{name: "nil provider", nilProvider: true, want: "GitHub provider is nil"},
		{name: "wrong provider", configure: func(client *lazyTestProvider) { client.providerName = "forgejo" }, want: "only github is supported"},
		{name: "release lookup", configure: func(client *lazyTestProvider) { client.releaseErr = errors.New("release unavailable") }, want: "release unavailable"},
		{name: "asset lookup", configure: func(client *lazyTestProvider) { client.release.Assets = nil }, want: "resolve exact manifest asset"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
			if test.configure != nil {
				test.configure(client)
			}
			originalFactory := lazyProviderFactory
			lazyProviderFactory = func() (provider.Provider, error) {
				if test.factoryErr != nil {
					return nil, test.factoryErr
				}
				if test.nilProvider {
					return nil, nil
				}
				return client, nil
			}
			t.Cleanup(func() { lazyProviderFactory = originalFactory })
			selection, err := ResolveTarget("tool", TargetResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("failure = %v, want %q", err, test.want)
			}
			if client.downloads.Load() != 0 {
				t.Fatalf("failure downloaded %d assets", client.downloads.Load())
			}
		})
	}
}

func TestLazyInstallRejectsCorruptCacheIndexBeforeNetwork(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache.Root(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root(), "index.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "verify lazy artifact cache") {
		t.Fatalf("corrupt cache error = %v", err)
	}
	if client.downloads.Load() != 0 {
		t.Fatalf("corrupt cache downloaded %d assets", client.downloads.Load())
	}
}

func TestLazyInstallHandlesCacheLockAndStagingFailures(t *testing.T) {
	t.Run("lock directory", func(t *testing.T) {
		_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
		cache, err := artifactcache.New()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cache.Root(), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache.Root(), "locks"), []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		selection, err := ResolveTarget("tool", TargetResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "acquire lazy artifact lock") {
			t.Fatalf("lock failure = %v", err)
		}
		if client.downloads.Load() != 0 {
			t.Fatalf("lock failure downloaded %d assets", client.downloads.Load())
		}
	})

	t.Run("staging directory", func(t *testing.T) {
		_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
		cache, err := artifactcache.New()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cache.Root(), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache.Root(), "staging"), []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		selection, err := ResolveTarget("tool", TargetResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "publish lazy artifact cache") {
			t.Fatalf("staging failure = %v", err)
		}
		if client.downloads.Load() != 1 {
			t.Fatalf("staging failure downloads = %d, want one", client.downloads.Load())
		}
	})
}

func TestLazyInstallHandlesConfigurationAndExtractionFailures(t *testing.T) {
	t.Run("configuration load", testLazyInstallConfigurationLoadFailure)
	t.Run("temporary file", testLazyInstallTemporaryFileFailure)
	t.Run("empty extracted binary", testLazyInstallEmptyExtractedBinary)
}

func testLazyInstallConfigurationLoadFailure(t *testing.T) {
	manifestPath, _ := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	brokenConfigHome := filepath.Join(t.TempDir(), "config-file")
	if err := os.WriteFile(brokenConfigHome, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", brokenConfigHome)
	selection := TargetSelection{Source: selectionProject, ManifestPath: manifestPath, Target: "tool"}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "load config for lazy install") {
		t.Fatalf("configuration failure = %v", err)
	}
}

func testLazyInstallTemporaryFileFailure(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	temporaryDirectory := filepath.Join(t.TempDir(), "tmp-file")
	if err := os.WriteFile(temporaryDirectory, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temporaryDirectory)
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "staging file") {
		t.Fatalf("temporary-file failure = %v", err)
	}
	if client.downloads.Load() != 0 {
		t.Fatalf("temporary-file failure downloaded %d assets", client.downloads.Load())
	}
}

func testLazyInstallEmptyExtractedBinary(t *testing.T) {
	_, client := configureLazyProject(t, nil)
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "publish lazy artifact cache") {
		t.Fatalf("empty-binary failure = %v", err)
	}
	if client.downloads.Load() != 1 {
		t.Fatalf("extraction failure downloads = %d, want one", client.downloads.Load())
	}
	assertLazyCacheHasNoPartialPublish(t)
}

func TestLazyInstallUsesChecksumPolicyAfterDownload(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	client.downloadHook = func(string) {
		if err := os.WriteFile(configPath, []byte(`{"lazy_install":true,"checksum_policy":"off"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "load config for checksum policy") {
		t.Fatalf("checksum policy load failure = %v", err)
	}
	assertLazyCacheHasNoPartialPublish(t)
}

func TestLazyInstallRejectsInvalidAndUnselectableArchives(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{name: "invalid archive", body: []byte("not a tar gzip"), want: "resolve lazy archive binary"},
		{name: "no executable", body: nonExecutableTar(t), want: "extract lazy release asset"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, client := configureLazyProject(t, test.body)
			client.release.Assets[0].Name = "tool.tar"
			digest := sha256.Sum256(test.body)
			client.release.Assets[0].Digest = "sha256:" + hex.EncodeToString(digest[:])
			manifestPath := os.Getenv("ZENGET_MANIFEST")
			writeCommandManifest(t, manifestPath, state.Manifest{
				SchemaVersion: manifest.SchemaVersion,
				Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool.tar", TargetName: "tool"}},
			})
			selection := TargetSelection{Source: selectionProject, ManifestPath: manifestPath, Target: "tool"}
			if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("archive failure = %v, want %q", err, test.want)
			}
			assertLazyCacheHasNoPartialPublish(t)
		})
	}
}

func nonExecutableTar(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	contents := []byte("documentation")
	if err := writer.WriteHeader(&tar.Header{Name: "README", Mode: 0644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestLazyInstallRechecksCacheAfterReleaseResolution(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	client.releaseHook = func() {
		if err := os.MkdirAll(cache.Root(), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache.Root(), "index.json"), []byte("not-json"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "verify lazy artifact cache") {
		t.Fatalf("post-release cache error = %v", err)
	}
	if client.downloads.Load() != 0 {
		t.Fatalf("post-release cache failure downloaded %d assets", client.downloads.Load())
	}
}

func TestLazyInstallRejectsEmptyResolvedAssetName(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	manifestPath := os.Getenv("ZENGET_MANIFEST")
	writeCommandManifest(t, manifestPath, state.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", TargetName: "tool"}},
	})
	client.release.Assets = []provider.Asset{{ID: 1}}
	selection := TargetSelection{Source: selectionProject, ManifestPath: manifestPath, Target: "tool"}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "empty name") {
		t.Fatalf("empty asset name error = %v", err)
	}
}

func TestLazyInstallRequiresOptInTrustAndRuntimeOptOut(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))

	if err := (&config.Config{}).Save(); err != nil {
		t.Fatal(err)
	}
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled lazy install error = %v", err)
	}
	if client.downloads.Load() != 0 {
		t.Fatalf("disabled lazy install downloaded %d assets", client.downloads.Load())
	}

	if err := (&config.Config{LazyInstall: true}).Save(); err != nil {
		t.Fatal(err)
	}
	store, err := trust.Load()
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := store.Remove(os.Getenv("ZENGET_MANIFEST")); err != nil || !removed {
		t.Fatalf("remove trust = %t, %v", removed, err)
	}
	if err := trust.Save(store); err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("untrusted lazy install error = %v", err)
	}
	if client.downloads.Load() != 0 {
		t.Fatalf("untrusted lazy install downloaded %d assets", client.downloads.Load())
	}

	store = trust.New()
	if _, _, err := store.Add(os.Getenv("ZENGET_MANIFEST")); err != nil {
		t.Fatal(err)
	}
	if err := trust.Save(store); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENGET_LAZY_INSTALL", "0")
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("runtime opt-out error = %v", err)
	}
	if client.downloads.Load() != 0 {
		t.Fatalf("runtime opt-out downloaded %d assets", client.downloads.Load())
	}
}

func TestLazyInstallUsesOneDownloadForConcurrentFirstStarts(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	client.downloadStart = make(chan struct{})
	releaseDownload := make(chan struct{})
	client.downloadGate = releaseDownload
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := lazyInstallProjectTarget(context.Background(), selection)
			results <- err
		}()
	}
	select {
	case <-client.downloadStart:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent lazy install did not start a download")
	}
	close(releaseDownload)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent lazy install error = %v", err)
		}
	}
	if got := client.downloads.Load(); got != 1 {
		t.Fatalf("concurrent downloads = %d, want one", got)
	}
}

func TestLazyInstallCleansLockAndCacheOnDownloadOrChecksumFailure(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	client.downloadErr = errors.New("network unavailable")
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "network unavailable") {
		t.Fatalf("download failure = %v", err)
	}
	assertLazyCacheHasNoPartialPublish(t)

	client.downloadErr = nil
	client.release.Assets[0].Digest = "sha256:" + strings.Repeat("0", 64)
	selection, err = ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "checksum verification failed") {
		t.Fatalf("checksum failure = %v", err)
	}
	assertLazyCacheHasNoPartialPublish(t)
}

func TestLazyInstallHonorsChecksumPolicyAndDownloadLimit(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	if err := (&config.Config{LazyInstall: true, ChecksumPolicy: config.ChecksumPolicyRequired, MaxDownloadBytes: 1}).Save(); err != nil {
		t.Fatal(err)
	}
	client.release.Assets[0].Digest = ""
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || (!strings.Contains(err.Error(), "exceeds limit") && !strings.Contains(err.Error(), "no valid SHA-256")) {
		t.Fatalf("policy/limit error = %v", err)
	}
	if got := client.downloadLimit.Load(); got != 1 {
		t.Fatalf("provider download limit = %d, want 1", got)
	}
	assertLazyCacheHasNoPartialPublish(t)
}

func TestLazyInstallRejectsWrongTagWithoutLatestFallback(t *testing.T) {
	_, client := configureLazyProject(t, []byte("#!/bin/sh\nexit 0\n"))
	client.release.TagName = "v2.0.0"
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazyInstallProjectTarget(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "exact tag") {
		t.Fatalf("wrong-tag error = %v", err)
	}
	if client.latestCalls.Load() != 0 || client.downloads.Load() != 0 {
		t.Fatalf("wrong-tag fallback calls = latest %d, downloads %d", client.latestCalls.Load(), client.downloads.Load())
	}
}

func assertLazyCacheHasNoPartialPublish(t *testing.T) {
	t.Helper()
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache.Root(), "index.json")); !os.IsNotExist(err) {
		t.Fatalf("partial cache index exists: %v", err)
	}
	locks, err := filepath.Glob(filepath.Join(cache.Root(), "locks", "*.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 0 {
		t.Fatalf("cache locks remain: %v", locks)
	}
	staging, err := filepath.Glob(filepath.Join(cache.Root(), "staging", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staging) != 0 {
		t.Fatalf("cache staging remains: %v", staging)
	}
}

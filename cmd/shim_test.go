package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/artifactcache"
	"zenget/internal/config"
	"zenget/internal/hashx"
	"zenget/internal/manifest"
	"zenget/internal/projectmanifest"
	"zenget/internal/state"
	"zenget/internal/usage"
	"zenget/internal/wrapper"
)

func writeShimManifest(t *testing.T, directory string, file state.Manifest) string {
	t.Helper()
	data, err := manifest.Marshal(file)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(directory, projectmanifest.DefaultFileName)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func newShimCache(t *testing.T) *artifactcache.Cache {
	t.Helper()
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	return cache
}

func TestResolveTargetUsesProjectCacheWithoutTouchingUsageIndex(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	project := t.TempDir()
	writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps: []state.ManifestApp{{
			Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v2.0.0",
			Asset: "tool-linux-amd64.tar.gz", TargetName: "tool",
		}},
	})

	cache := newShimCache(t)
	key := artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v2.0.0",
		PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS),
		AssetName: "tool-linux-amd64.tar.gz", TargetName: "tool",
	}
	entry, err := cache.Publish(context.Background(), key, cacheTestDigest([]byte("archive")), []byte("cached binary"))
	if err != nil {
		t.Fatalf("publish cache: %v", err)
	}
	before := entry.LastSuccessfulUse

	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if selection.Source != selectionProject || selection.Repository != "acme/tool" || selection.Tag != "v2.0.0" {
		t.Fatalf("selection = %+v, want project acme/tool@v2.0.0", selection)
	}
	if selection.Integrity != artifactcache.IntegrityValid || selection.CacheIntegrity != artifactcache.IntegrityValid {
		t.Fatalf("selection integrity = (%s, %s), want valid", selection.Integrity, selection.CacheIntegrity)
	}
	wantPath := filepath.Join(cache.Root(), filepath.FromSlash(entry.RelativePath))
	if selection.ArtifactPath != wantPath {
		t.Fatalf("artifact path = %q, want %q", selection.ArtifactPath, wantPath)
	}
	entries, err := cache.List(artifactcache.Protection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Entry.LastSuccessfulUse.Equal(before) {
		t.Fatalf("which-style resolution touched cache usage index: %+v", entries)
	}
}

func TestResolveTargetReportsProjectCacheMissWithoutGlobalFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	project := t.TempDir()
	manifestPath := writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v2.0.0", TargetName: "tool"}},
	})

	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if selection.Source != selectionProject || selection.ManifestPath != manifestPath {
		t.Fatalf("selection = %+v, want project selection from %q", selection, manifestPath)
	}
	if selection.Integrity != artifactcache.IntegrityMissing || selection.CacheIntegrity != artifactcache.IntegrityMissing {
		t.Fatalf("selection integrity = (%s, %s), want missing", selection.Integrity, selection.CacheIntegrity)
	}
	if !strings.Contains(selection.Reason, "no matching") {
		t.Fatalf("selection reason = %q, want cache miss diagnostic", selection.Reason)
	}
}

func TestResolveTargetFallsBackToVerifiedGlobalArtifact(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	project := t.TempDir()

	artifactPath, err := wrapper.VersionedRealPath("github", "acme/tool", "v1.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0755); err != nil {
		t.Fatal(err)
	}
	contents := []byte("global binary")
	if err := os.WriteFile(artifactPath, contents, 0755); err != nil {
		t.Fatal(err)
	}
	if err := (&state.State{Apps: map[string]state.App{
		"acme/tool": {
			Provider: "github", Version: "v1.0.0", InstallPath: filepath.Join(t.TempDir(), "tool"),
			PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH,
			ActiveArtifact: &state.Artifact{Version: "v1.0.0", RealPath: artifactPath, XXH3_64: hashBytes(contents)},
		},
	}}).Save(); err != nil {
		t.Fatal(err)
	}

	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project, NoManifestDiscovery: true})
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if selection.Source != selectionGlobal || selection.Repository != "acme/tool" || selection.Integrity != artifactcache.IntegrityValid {
		t.Fatalf("selection = %+v, want valid global selection", selection)
	}
	if selection.ArtifactPath != artifactPath {
		t.Fatalf("global artifact path = %q, want %q", selection.ArtifactPath, artifactPath)
	}
}

func TestResolveTargetFailsClosedForAmbiguousProjectTarget(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps: []state.ManifestApp{
			{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0"},
			{Repository: "other/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0"},
		},
	})

	_, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err == nil || !strings.Contains(err.Error(), "matches multiple") {
		t.Fatalf("ResolveTarget() error = %v, want ambiguity diagnostic", err)
	}
}

func TestResolveTargetHonorsManifestDiscoveryOptOutEnvironment(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "1")
	project := t.TempDir()
	writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v2.0.0", TargetName: "tool"}},
	})

	artifactPath, err := wrapper.VersionedRealPath("github", "acme/tool", "v1.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	contents := []byte("fallback")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, contents, 0755); err != nil {
		t.Fatal(err)
	}
	if err := (&state.State{Apps: map[string]state.App{
		"acme/tool": {Version: "v1.0.0", InstallPath: filepath.Join(t.TempDir(), "tool"), RealPath: artifactPath, XXH3_64: hashBytes(contents)},
	}}).Save(); err != nil {
		t.Fatal(err)
	}

	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if selection.Source != selectionGlobal || selection.Tag != "v1.0.0" {
		t.Fatalf("selection = %+v, want global opt-out fallback", selection)
	}
}

func TestResolveTargetReportsCorruptProjectCache(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool.tar.gz", TargetName: "tool"}},
	})
	cache := newShimCache(t)
	entry, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1.0.0", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS), AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("cached"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root(), filepath.FromSlash(entry.RelativePath)), []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}

	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if selection.Integrity != artifactcache.IntegrityCorrupt || selection.CacheIntegrity != artifactcache.IntegrityCorrupt {
		t.Fatalf("selection integrity = (%s, %s), want corrupt", selection.Integrity, selection.CacheIntegrity)
	}
}

func TestResolveTargetReportsInvalidCacheIndex(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", TargetName: "tool"}},
	})
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache.Root(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root(), "index.json"), []byte("not-json"), 0644); err != nil {
		t.Fatal(err)
	}
	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if selection.CacheIntegrity != artifactcache.IntegrityCorrupt || !strings.Contains(selection.Reason, "decode cache index") {
		t.Fatalf("selection = %+v, want invalid cache index diagnostic", selection)
	}
}

func TestResolveTargetReturnsManifestLoadErrors(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	manifestPath := filepath.Join(t.TempDir(), "zenget.json")
	if err := os.WriteFile(manifestPath, []byte("not-json"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENGET_MANIFEST", manifestPath)
	if _, err := ResolveTarget("tool", TargetResolveOptions{}); err == nil || !strings.Contains(err.Error(), "manifest source=environment") {
		t.Fatalf("ResolveTarget() error = %v, want manifest load error", err)
	}
}

func TestResolveTargetFailsClosedForMultipleMatchingCacheEntries(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool.tar.gz", TargetName: "tool"}},
	})
	cache := newShimCache(t)
	base := artifactcache.Key{Provider: "github", Repository: "acme/tool", Tag: "v1.0.0", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS), AssetName: "tool.tar.gz", TargetName: "tool"}
	for _, id := range []int64{1, 2} {
		key := base
		key.AssetID = id
		if _, err := cache.Publish(context.Background(), key, cacheTestDigest([]byte("archive")), []byte("binary")); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if selection.Integrity != artifactcache.IntegrityCorrupt || !strings.Contains(selection.Reason, "multiple cache artifacts") {
		t.Fatalf("selection = %+v, want multiple-cache diagnostic", selection)
	}
}

func TestExecuteArtifactPreservesTargetArgumentsEnvironmentAndExitCode(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "tool")
	contents := []byte("#!/bin/sh\nprintf '%s|%s|%s|%s' \"$0\" \"$1\" \"$2\" \"$ZENGET_SHIM_TEST\" > \"" + outputPath + "\"\nexit 17\n")
	if err := os.WriteFile(script, contents, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENGET_SHIM_TEST", "preserved")
	selection := TargetSelection{Target: "tool", ArtifactPath: script, Provider: "github", Repository: "acme/tool", Tag: "v1"}
	err := executeArtifact(context.Background(), selection, []string{"first", "second"})
	var exitErr *ShimExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 17 {
		t.Fatalf("executeArtifact() error = %v, want exit code 17", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "|first|second|preserved") {
		t.Fatalf("child argv/environment = %q, want preserved arguments and environment", data)
	}
}

func TestExecuteArtifactRecordsRepositoryAndVersionOnlyWhenTrackingEnabled(t *testing.T) {
	dataHome := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	script := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	selection := TargetSelection{Target: "tool", ArtifactPath: script, Provider: "github", Repository: "acme/tool", Tag: "v3.2.1"}

	if err := (&config.Config{UsageTracking: true}).Save(); err != nil {
		t.Fatal(err)
	}
	if err := executeArtifact(context.Background(), selection, nil); err != nil {
		t.Fatalf("executeArtifact() error = %v", err)
	}
	logPath, err := wrapper.UsageLogPathFor("github", "acme/tool")
	if err != nil {
		t.Fatal(err)
	}
	events, err := usage.ReadLog(logPath)
	if err != nil || len(events) != 1 {
		t.Fatalf("usage events = (%+v, %v), want one event", events, err)
	}
	if events[0].Repository != "acme/tool" || events[0].Version != "v3.2.1" || events[0].ExitCode != 0 {
		t.Fatalf("usage event = %+v, want repository/version/exit code", events[0])
	}

	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := (&config.Config{}).Save(); err != nil {
		t.Fatal(err)
	}
	if err := executeArtifact(context.Background(), selection, nil); err != nil {
		t.Fatalf("executeArtifact() with tracking disabled error = %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("usage log exists with tracking disabled: %v", err)
	}
}

func TestDispatchTargetExecutesProjectCacheArtifact(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	manifestPath := writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool.tar.gz", TargetName: "tool"}},
	})
	t.Setenv("ZENGET_MANIFEST", manifestPath)
	binary := []byte("#!/bin/sh\nexit 0\n")
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1.0.0", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS), AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), binary); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTarget(context.Background(), "tool", nil); err != nil {
		t.Fatalf("dispatchTarget() error = %v", err)
	}
}

func TestRunShimCommandDispatchesTarget(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	manifestPath := writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool.tar.gz", TargetName: "tool"}},
	})
	t.Setenv("ZENGET_MANIFEST", manifestPath)
	if _, err := newShimCache(t).Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1.0.0", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS), AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("#!/bin/sh\nexit 0\n")); err != nil {
		t.Fatal(err)
	}
	if err := runShim(&cobra.Command{}, []string{"tool"}); err != nil {
		t.Fatalf("runShim() error = %v", err)
	}
}

func TestDispatchTargetRejectsProjectCacheMissWithApplyAdvice(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	manifestPath := writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", TargetName: "tool"}},
	})
	t.Setenv("ZENGET_MANIFEST", manifestPath)
	err := dispatchTarget(context.Background(), "tool", nil)
	if err == nil || !strings.Contains(err.Error(), "zenget apply") {
		t.Fatalf("dispatchTarget() error = %v, want apply advice", err)
	}
}

func TestDispatchTargetReturnsGlobalExitCodeAndSignalStatus(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "1")
	script := filepath.Join(dataHome, "zenget", "bin", "github", "acme", "tool", "v1.0.0", "tool")
	if err := os.MkdirAll(filepath.Dir(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nkill -TERM $$\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := (&state.State{Apps: map[string]state.App{
		"acme/tool": {Version: "v1.0.0", InstallPath: filepath.Join(t.TempDir(), "tool"), RealPath: script, XXH3_64: hashBytes([]byte("#!/bin/sh\nkill -TERM $$\n"))},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	err := dispatchTarget(context.Background(), "tool", nil)
	var exitErr *ShimExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 128+int(syscall.SIGTERM) {
		t.Fatalf("dispatchTarget() error = %v, want SIGTERM status", err)
	}
}

func TestDispatchTargetRejectsMissingGlobalArtifact(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "1")
	missingPath := filepath.Join(dataHome, "zenget", "bin", "github", "acme", "tool", "v1", "tool")
	if err := (&state.State{Apps: map[string]state.App{
		"acme/tool": {Version: "v1", InstallPath: filepath.Join(t.TempDir(), "tool"), RealPath: missingPath, XXH3_64: strings.Repeat("a", 16)},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	err := dispatchTarget(context.Background(), "tool", nil)
	if err == nil || !strings.Contains(err.Error(), "active artifact file is missing") {
		t.Fatalf("dispatchTarget() error = %v, want missing-artifact diagnostic", err)
	}
}

func TestResolveTargetReportsGlobalTargetAmbiguityAndMissingTarget(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "1")
	installDir := t.TempDir()
	if err := (&state.State{Apps: map[string]state.App{
		"acme/tool":  {Version: "v1", InstallPath: filepath.Join(installDir, "tool")},
		"other/tool": {Version: "v1", InstallPath: filepath.Join(installDir, "tool")},
	}}).Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTarget("tool", TargetResolveOptions{}); err == nil || !strings.Contains(err.Error(), "multiple globally installed") {
		t.Fatalf("ambiguous global resolution error = %v", err)
	}
	if _, err := ResolveTarget("missing", TargetResolveOptions{}); err == nil || !strings.Contains(err.Error(), "no installed target") {
		t.Fatalf("missing global resolution error = %v", err)
	}
}

func TestGlobalSelectionReportsValidCacheAndCacheArtifactValidationErrors(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "1")
	contents := []byte("global binary")
	artifactPath, err := wrapper.VersionedRealPath("github", "acme/tool", "v1.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, contents, 0755); err != nil {
		t.Fatal(err)
	}
	if err := (&state.State{Apps: map[string]state.App{"acme/tool": {Version: "v1.0.0", InstallPath: filepath.Join(t.TempDir(), "tool"), RealPath: artifactPath, XXH3_64: hashBytes(contents)}}}).Save(); err != nil {
		t.Fatal(err)
	}
	cache := newShimCache(t)
	entry, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1.0.0", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS), AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("cached"))
	if err != nil {
		t.Fatal(err)
	}
	selection, err := ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveTarget() error = %v", err)
	}
	if selection.Source != selectionGlobal || selection.Integrity != artifactcache.IntegrityValid || selection.CacheIntegrity != artifactcache.IntegrityValid {
		t.Fatalf("selection = %+v, want valid global/cache integrity", selection)
	}
	if err := os.Remove(filepath.Join(cache.Root(), filepath.FromSlash(entry.RelativePath))); err != nil {
		t.Fatal(err)
	}
	selection, err = ResolveTarget("tool", TargetResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveTarget() after cache removal error = %v", err)
	}
	if selection.CacheIntegrity != artifactcache.IntegrityMissing || selection.Integrity != artifactcache.IntegrityValid {
		t.Fatalf("selection after cache removal = %+v, want valid active/missing cache", selection)
	}
}

func TestValidateCacheArtifactReportsMissingAndHashMismatch(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cache := newShimCache(t)
	entry, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1", PlatformOS: "linux", PlatformArch: "amd64", PlatformLibc: "glibc", AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache.Root(), filepath.FromSlash(entry.RelativePath))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := validateCacheArtifact(path, entry); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("validateCacheArtifact() missing error = %v", err)
	}
	if err := os.WriteFile(path, []byte("different"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validateCacheArtifact(path, entry); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("validateCacheArtifact() mismatch error = %v", err)
	}
	if err := os.WriteFile(path, []byte("binarx"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validateCacheArtifact(path, entry); err == nil || !strings.Contains(err.Error(), "XXH3-64 mismatch") {
		t.Fatalf("validateCacheArtifact() hash error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(linkTarget, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTarget, path); err != nil {
		t.Fatal(err)
	}
	if err := validateCacheArtifact(path, entry); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("validateCacheArtifact() symlink error = %v", err)
	}
}

func TestValidationHelpersRejectUnsafeAndInvalidArtifacts(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	inside := filepath.Join(dataHome, "zenget", "bin", "github", "acme", "tool", "v1", "tool")
	if status, _ := validateManagedArtifact(inside, "hash"); status != artifactcache.IntegrityMissing {
		t.Fatalf("missing managed artifact status = %s, want missing", status)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0755); err != nil {
		t.Fatal(err)
	}
	if status, _ := validateManagedArtifact(outside, hashBytes([]byte("outside"))); status != artifactcache.IntegrityCorrupt {
		t.Fatalf("outside artifact status = %s, want corrupt", status)
	}
	if err := os.MkdirAll(filepath.Dir(inside), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("actual"), 0755); err != nil {
		t.Fatal(err)
	}
	if status, _ := validateManagedArtifact(inside, hashBytes([]byte("expected"))); status != artifactcache.IntegrityCorrupt {
		t.Fatalf("mismatched artifact status = %s, want corrupt", status)
	}
	if err := os.Remove(inside); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(t.TempDir(), "link-target")
	if err := os.WriteFile(linkTarget, []byte("target"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTarget, inside); err != nil {
		t.Fatal(err)
	}
	if status, _ := validateManagedArtifact(inside, hashBytes([]byte("target"))); status != artifactcache.IntegrityCorrupt {
		t.Fatalf("symlink artifact status = %s, want corrupt", status)
	}
	if _, err := normalizeTarget("../tool"); err == nil {
		t.Fatal("normalizeTarget() accepted path traversal")
	}
	if _, err := normalizeTarget("tool/name"); err == nil {
		t.Fatal("normalizeTarget() accepted nested target")
	}
	if err := validatePathIsRegular(filepath.Dir(inside)); err == nil {
		t.Fatal("validatePathIsRegular() accepted directory")
	}
}

func TestShimExitErrorAndLibcHelpers(t *testing.T) {
	exitErr := &ShimExitError{Code: 23}
	if exitErr.Error() != "managed artifact exited with status 23" || exitErr.ExitCode() != 23 {
		t.Fatalf("ShimExitError = (%q, %d)", exitErr.Error(), exitErr.ExitCode())
	}
	var nilExit *ShimExitError
	if nilExit.ExitCode() != 1 {
		t.Fatalf("nil ShimExitError.ExitCode() = %d, want 1", nilExit.ExitCode())
	}
	if got := libcIdentity("darwin"); got != "unknown" {
		t.Fatalf("libcIdentity(darwin) = %q, want unknown", got)
	}
	if got := selectionReason(TargetSelection{Integrity: artifactcache.IntegrityMissing}); got != "missing" {
		t.Fatalf("selectionReason() = %q, want missing", got)
	}
}

func TestShimHelperErrorPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := validatePathIsRegular(missing); err == nil {
		t.Fatal("validatePathIsRegular() accepted missing file")
	}
	directory := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := validatePathIsRegular(directory); err == nil {
		t.Fatal("validatePathIsRegular() accepted directory")
	}
	regular := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(regular, []byte("regular"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validatePathIsRegular(regular); err != nil {
		t.Fatalf("validatePathIsRegular() rejected regular file: %v", err)
	}
	command := exec.Command("/bin/false")
	if got := processExitCode(command, errors.New("not started")); got != 1 {
		t.Fatalf("processExitCode() = %d, want fallback status 1", got)
	}
	if got := processExitCode(exec.Command("/bin/false"), nil); got != 0 {
		t.Fatalf("processExitCode() without process state = %d, want 0", got)
	}
	if (&ShimExitError{Code: -1}).ExitCode() != 1 {
		t.Fatal("negative ShimExitError status was not normalized")
	}
	if got := globalTargetName("acme/tool", state.App{}); got != "tool" {
		t.Fatalf("globalTargetName() = %q, want repository basename", got)
	}
}

func hashBytes(data []byte) string {
	return hashx.Bytes(data)
}

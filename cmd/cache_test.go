package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/artifactcache"
	"zenget/internal/manifest"
	"zenget/internal/state"
)

func cacheTestDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func cacheTestCommand(out *bytes.Buffer) *cobra.Command {
	command := &cobra.Command{}
	command.SetOut(out)
	command.Flags().StringArray("manifest", nil, "")
	command.Flags().Duration("max-age", 0, "")
	command.Flags().Int64("max-bytes", 0, "")
	return command
}

func TestRunCacheListPrintsHumanAndJSONOutput(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	previousDiscovery := noManifestDiscovery
	noManifestDiscovery = true
	t.Cleanup(func() { noManifestDiscovery = previousDiscovery })

	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1", PlatformOS: "linux", PlatformArch: "amd64", PlatformLibc: "glibc", AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("binary")); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "zenget.json")
	manifestData, err := manifest.Marshal(state.Manifest{SchemaVersion: manifest.SchemaVersion, Apps: []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1", Asset: "tool.tar.gz", TargetName: "tool"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0644); err != nil {
		t.Fatal(err)
	}

	var human bytes.Buffer
	humanCommand := cacheTestCommand(&human)
	if err := humanCommand.Flags().Set("manifest", manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := runCacheList(humanCommand, nil); err != nil {
		t.Fatalf("runCacheList() error = %v", err)
	}
	if !strings.Contains(human.String(), "IDENTITY") || !strings.Contains(human.String(), "valid") {
		t.Fatalf("human cache list = %q", human.String())
	}

	previousJSON := cacheListJSON
	cacheListJSON = true
	t.Cleanup(func() { cacheListJSON = previousJSON })
	var jsonOutput bytes.Buffer
	jsonCommand := cacheTestCommand(&jsonOutput)
	if err := jsonCommand.Flags().Set("manifest", manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := runCacheList(jsonCommand, nil); err != nil {
		t.Fatalf("runCacheList(JSON) error = %v", err)
	}
	if !strings.Contains(jsonOutput.String(), `"integrity": "valid"`) {
		t.Fatalf("JSON cache list = %q", jsonOutput.String())
	}
}

func TestRunCacheGCRejectsInvalidManifestBeforeMutation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1", PlatformOS: "linux", PlatformArch: "amd64", PlatformLibc: "glibc", AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	badManifest := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badManifest, []byte(`{"schema_version":1,"apps":[{"repository":"acme/tool","provider":"github","tag":"v1","unknown":true}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	command := cacheTestCommand(&bytes.Buffer{})
	if err := command.Flags().Set("manifest", badManifest); err != nil {
		t.Fatal(err)
	}
	previousDryRun := cacheGCDryRun
	cacheGCDryRun = false
	t.Cleanup(func() { cacheGCDryRun = previousDryRun })
	if err := runCacheGC(command, nil); err == nil {
		t.Fatal("runCacheGC() error = nil for invalid manifest")
	}
	if _, err := os.Stat(filepath.Join(cache.Root(), entry.RelativePath)); err != nil {
		t.Fatalf("cache artifact after rejected GC: %v", err)
	}
}

func TestProtectionFromStateAndManifest(t *testing.T) {
	st := &state.State{Apps: map[string]state.App{
		"acme/tool": {Version: "v1", PlatformOS: "linux", PlatformArch: "amd64", ActiveArtifact: &state.Artifact{Version: "v1"}, PreviousArtifact: &state.Artifact{Version: "v0"}},
	}}
	protection := protectionFromState(st)
	if len(protection.Active) != 2 || protection.Active[0].Repository != "acme/tool" {
		t.Fatalf("state protection = %+v", protection.Active)
	}
	file := state.Manifest{SchemaVersion: manifest.SchemaVersion, Apps: []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v2", PlatformOS: "linux", PlatformArch: "amd64"}}}
	manifestKeys := protectionFromManifest(file)
	if len(manifestKeys) != 1 || manifestKeys[0].Tag != "v2" || manifestKeys[0].PlatformLibc == "" {
		t.Fatalf("manifest protection = %+v", manifestKeys)
	}
}

func TestRunCacheGCDryRunPrintsPlan(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	previousDiscovery := noManifestDiscovery
	noManifestDiscovery = true
	t.Cleanup(func() { noManifestDiscovery = previousDiscovery })
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1", PlatformOS: "linux", PlatformArch: "amd64", PlatformLibc: "glibc", AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("binary")); err != nil {
		t.Fatal(err)
	}
	command := cacheTestCommand(&bytes.Buffer{})
	if err := command.Flags().Set("max-age", "1ns"); err != nil {
		t.Fatal(err)
	}
	previousDryRun, previousAge := cacheGCDryRun, cacheGCMaxAge
	cacheGCDryRun, cacheGCMaxAge = true, time.Nanosecond
	t.Cleanup(func() { cacheGCDryRun, cacheGCMaxAge = previousDryRun, previousAge })
	if err := runCacheGC(command, nil); err != nil {
		t.Fatalf("runCacheGC() error = %v", err)
	}
	if !strings.Contains(command.OutOrStdout().(*bytes.Buffer).String(), "dry-run") {
		t.Fatalf("dry-run output = %q", command.OutOrStdout().(*bytes.Buffer).String())
	}
}

func TestRunCacheGCActualRemovesExplicitAgeCandidate(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	previousDiscovery := noManifestDiscovery
	noManifestDiscovery = true
	t.Cleanup(func() { noManifestDiscovery = previousDiscovery })
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1", PlatformOS: "linux", PlatformArch: "amd64", PlatformLibc: "glibc", AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("binary")); err != nil {
		t.Fatal(err)
	}
	command := cacheTestCommand(&bytes.Buffer{})
	if err := command.Flags().Set("max-age", "1ns"); err != nil {
		t.Fatal(err)
	}
	previousDryRun, previousAge := cacheGCDryRun, cacheGCMaxAge
	cacheGCDryRun, cacheGCMaxAge = false, time.Nanosecond
	t.Cleanup(func() { cacheGCDryRun, cacheGCMaxAge = previousDryRun, previousAge })
	if err := runCacheGC(command, nil); err != nil {
		t.Fatalf("runCacheGC() error = %v", err)
	}
	if !strings.Contains(command.OutOrStdout().(*bytes.Buffer).String(), "completed") {
		t.Fatalf("actual GC output = %q", command.OutOrStdout().(*bytes.Buffer).String())
	}
	entries, err := cache.List(artifactcache.Protection{})
	if err != nil || len(entries) != 0 {
		t.Fatalf("cache entries after actual GC = (%+v, %v)", entries, err)
	}
}

func TestCacheOutputHelpersCoverEmptyAndReasons(t *testing.T) {
	var empty bytes.Buffer
	if err := printCacheEntries(&empty, nil); err != nil || !strings.Contains(empty.String(), "No cached artifacts") {
		t.Fatalf("empty cache output = (%q, %v)", empty.String(), err)
	}
	var entries bytes.Buffer
	if err := printCacheEntries(&entries, []artifactcache.Inspection{{Identity: "x", Integrity: artifactcache.IntegrityCorrupt, Size: 3, Reason: "bad", Active: true, Manifest: true}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(entries.String(), "active,manifest") || !strings.Contains(entries.String(), "reason: bad") {
		t.Fatalf("cache entry output = %q", entries.String())
	}
	for _, dryRun := range []bool{false, true} {
		var output bytes.Buffer
		if err := printCacheGCPlan(&output, artifactcache.GCPlan{DryRun: dryRun, DeleteEntries: []artifactcache.Inspection{{Identity: "x", Size: 1}}, DeleteStaging: []string{"staging/orphan"}, ReclaimedBytes: 1, RemainingBytes: 2}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "x") || !strings.Contains(output.String(), "staging/orphan") {
			t.Fatalf("GC output = %q", output.String())
		}
	}
	if err := writeJSON(&bytes.Buffer{}, make(chan int)); err == nil {
		t.Fatal("writeJSON() error = nil for unsupported value")
	}
}

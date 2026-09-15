package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/artifactcache"
	"zenget/internal/manifest"
	"zenget/internal/state"
)

func TestRunWhichPrintsProjectSelectionHumanReadable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	project := t.TempDir()
	manifestPath := writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v2.0.0", Asset: "tool.tar.gz", TargetName: "tool"}},
	})
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v2.0.0", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS), AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("binary")); err != nil {
		t.Fatal(err)
	}

	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	if err := printTargetSelection(&output, selection); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Selection: project", "Manifest: " + manifestPath, "Repository: acme/tool", "Tag: v2.0.0", "Integrity: valid", "Cache integrity: valid"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("which output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRunWhichPrintsJSONSelection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	project := t.TempDir()
	writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.2.3", Asset: "tool.tar.gz", TargetName: "tool"}},
	})
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1.2.3", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS), AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := writeJSON(&output, selection); err != nil {
		t.Fatal(err)
	}
	var got TargetSelection
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse which JSON: %v", err)
	}
	if got.Source != selectionProject || got.Repository != "acme/tool" || got.Tag != "v1.2.3" || got.ArtifactPath != filepath.Join(cache.Root(), filepath.FromSlash(entry.RelativePath)) {
		t.Fatalf("JSON selection = %+v, want project cache selection", got)
	}
}

func TestRunWhichCommandUsesJSONFlag(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	manifestPath := writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "tool.tar.gz", TargetName: "tool"}},
	})
	t.Setenv("ZENGET_MANIFEST", manifestPath)
	previousJSON := whichJSON
	whichJSON = true
	t.Cleanup(func() { whichJSON = previousJSON })
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Publish(context.Background(), artifactcache.Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1.0.0", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS), AssetName: "tool.tar.gz", TargetName: "tool",
	}, cacheTestDigest([]byte("archive")), []byte("binary")); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runWhich(command, []string{"tool"}); err != nil {
		t.Fatalf("runWhich() error = %v", err)
	}
	var got TargetSelection
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse runWhich JSON: %v", err)
	}
	if got.ManifestPath != manifestPath || got.Source != selectionProject {
		t.Fatalf("runWhich selection = %+v, want manifest-backed project", got)
	}
}

func TestRunWhichCommandUsesHumanOutput(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	manifestPath := writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", TargetName: "tool"}},
	})
	t.Setenv("ZENGET_MANIFEST", manifestPath)
	previousJSON := whichJSON
	whichJSON = false
	t.Cleanup(func() { whichJSON = previousJSON })
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runWhich(command, []string{"tool"}); err != nil {
		t.Fatalf("runWhich() error = %v", err)
	}
	if !strings.Contains(output.String(), "Selection: project") || !strings.Contains(output.String(), "Cache integrity: missing") {
		t.Fatalf("runWhich human output = %q", output.String())
	}
}

func TestRunWhichReportsCacheMissWithoutChangingState(t *testing.T) {
	configHome := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("ZENGET_MANIFEST", "")
	project := t.TempDir()
	writeShimManifest(t, project, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "acme/tool", Provider: manifest.ProviderGitHub, Tag: "v9.0.0", TargetName: "tool"}},
	})

	previous := noManifestDiscovery
	noManifestDiscovery = false
	t.Cleanup(func() { noManifestDiscovery = previous })
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := printTargetSelection(command.OutOrStdout(), mustResolveWhich(t, project)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Cache integrity: missing") || !strings.Contains(output.String(), "Reason: no matching") {
		t.Fatalf("which cache miss output = %q", output.String())
	}
}

func TestRunWhichRejectsInvalidTarget(t *testing.T) {
	command := &cobra.Command{}
	if err := runWhich(command, []string{"../tool"}); err == nil || !strings.Contains(err.Error(), "invalid target") {
		t.Fatalf("runWhich() error = %v, want invalid target", err)
	}
}

func mustResolveWhich(t *testing.T, project string) TargetSelection {
	t.Helper()
	selection, err := ResolveTarget("tool", TargetResolveOptions{WorkingDirectory: project})
	if err != nil {
		t.Fatalf("ResolveTarget(): %v", err)
	}
	return selection
}

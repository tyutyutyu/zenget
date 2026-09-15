package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/artifactcache"
	"zenget/internal/recipe"
	"zenget/internal/recipepolicy"
	"zenget/internal/registry"
)

func TestRegistryLocalCommandBranches(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	source := "github:acme/catalog@" + commandRegistryCommit + ":recipes"
	cmd, output, _ := newBufferedCommand()
	if err := runRegistryAdd(cmd, []string{"stable", source}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runRegistryAdd(cmd, []string{"stable", source}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "already configured") {
		t.Fatalf("idempotent add output = %q", output.String())
	}

	output.Reset()
	registryListJSON = false
	if err := runRegistryList(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "- stable: "+source) {
		t.Fatalf("human list output = %q", output.String())
	}

	output.Reset()
	if err := runRegistryRemove(cmd, []string{"stable"}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runRegistryRemove(cmd, []string{"stable"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "was not configured") {
		t.Fatalf("missing remove output = %q", output.String())
	}
	output.Reset()
	if err := runRegistryList(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Registries: none") {
		t.Fatalf("empty human list output = %q", output.String())
	}

	if err := runRegistryAdd(cmd, []string{"stable", "github:acme/catalog@main:recipes"}); err == nil {
		t.Fatal("invalid add source unexpectedly succeeded")
	}
}

func TestRegistryCommandHelperBranches(t *testing.T) {
	config := registry.DefaultConfig()
	if _, err := config.Add("stable", "github:acme/catalog@"+commandRegistryCommit+":recipes"); err != nil {
		t.Fatal(err)
	}
	selected, err := selectRegistries(config, nil, true)
	if err != nil || len(selected) != 1 || selected[0].Name != "stable" {
		t.Fatalf("select all = %#v, error %v", selected, err)
	}
	if _, err := selectRegistries(config, []string{"missing"}, false); err == nil || !strings.Contains(err.Error(), "unknown registry") {
		t.Fatalf("select missing error = %v", err)
	}
	if got, err := registryJSONFlag(nil, true); err != nil || !got {
		t.Fatalf("registryJSONFlag(nil) = %t, error %v", got, err)
	}
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if got, err := registryJSONFlag(cmd, false); err != nil || !got {
		t.Fatalf("registryJSONFlag(flag) = %t, error %v", got, err)
	}
	if err := errorsForRegistrySync("  bad arguments  "); err == nil || err.Error() != "invalid registry sync arguments: bad arguments" {
		t.Fatalf("errorsForRegistrySync() = %v", err)
	}
	if err := checkRegistryPolicy(mustCommandSource(t), "../escape"); err == nil {
		t.Fatal("unsafe checkRegistryPolicy() unexpectedly succeeded")
	}
	if _, err := loadRegistryInstallRecipe("", "acme/widget"); err == nil || !strings.Contains(err.Error(), "must name") {
		t.Fatalf("empty registry recipe error = %v", err)
	}
}

func TestRegistrySyncArgumentAndEmptyConfigBranches(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cmd, output, _ := newBufferedCommand()
	registrySyncAll = true
	registrySyncOffline = false
	if err := runRegistrySync(cmd, []string{"stable"}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("--all with name error = %v", err)
	}
	registrySyncAll = false
	if err := runRegistrySync(cmd, nil); err == nil || !strings.Contains(err.Error(), "specify a registry name") {
		t.Fatalf("missing sync selection error = %v", err)
	}
	registrySyncAll = true
	if err := runRegistrySync(cmd, nil); err != nil {
		t.Fatalf("empty --all sync error = %v", err)
	}
	if !strings.Contains(output.String(), "No registries configured") {
		t.Fatalf("empty --all output = %q", output.String())
	}
	registrySyncAll = false
}

func TestRegistryCommandsRejectMalformedAndInsecureConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configPath, err := registry.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd, _, _ := newBufferedCommand()
	for _, run := range []struct {
		name string
		call func() error
	}{
		{name: "add", call: func() error {
			return runRegistryAdd(cmd, []string{"stable", "github:acme/catalog@" + commandRegistryCommit + ":recipes"})
		}},
		{name: "remove", call: func() error { return runRegistryRemove(cmd, []string{"stable"}) }},
		{name: "list", call: func() error { return runRegistryList(cmd, nil) }},
	} {
		t.Run(run.name, func(t *testing.T) {
			if err := run.call(); err == nil || !strings.Contains(err.Error(), "load registry config") {
				t.Fatalf("malformed config error = %v", err)
			}
		})
	}

	config := registry.DefaultConfig()
	if _, err := config.Add("stable", "github:acme/catalog@"+commandRegistryCommit+":recipes"); err != nil {
		t.Fatal(err)
	}
	if err := registry.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := runRegistryAdd(cmd, []string{"stable", "github:acme/other@" + commandRegistryCommit + ":recipes"}); err == nil || !strings.Contains(err.Error(), "different source") {
		t.Fatalf("different-source command error = %v", err)
	}
	if err := os.Chmod(configPath, 0644); err != nil {
		t.Fatal(err)
	}
	if err := runRegistryAdd(cmd, []string{"new", "github:acme/new@" + commandRegistryCommit + ":recipes"}); err == nil || !strings.Contains(err.Error(), "load registry config") {
		t.Fatalf("insecure add error = %v", err)
	}
	if err := runRegistryRemove(cmd, []string{"stable"}); err == nil || !strings.Contains(err.Error(), "load registry config") {
		t.Fatalf("insecure remove error = %v", err)
	}
}

func TestResolveOfflineReleaseReconstructsUniqueCachedRelease(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cache, err := artifactcache.New()
	if err != nil {
		t.Fatal(err)
	}
	key := artifactcache.Key{
		Provider: "github", Repository: "acme/widget", Tag: "v1.2.3",
		PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH, PlatformLibc: libcIdentity(runtime.GOOS),
		AssetID: 42, AssetName: "widget.tar.gz", TargetName: "widget",
	}
	archiveDigest := registry.SHA256([]byte("archive"))
	if _, err := cache.Publish(context.Background(), key, archiveDigest, []byte("binary")); err != nil {
		t.Fatal(err)
	}
	secondKey := key
	secondKey.AssetID = 43
	secondKey.AssetName = "widget.sha256"
	secondArchiveDigest := registry.SHA256([]byte("second archive"))
	if _, err := cache.Publish(context.Background(), secondKey, secondArchiveDigest, []byte("second binary")); err != nil {
		t.Fatal(err)
	}
	release, err := resolveOfflineRelease("acme/widget", installOptions{goos: runtime.GOOS, goarch: runtime.GOARCH})
	if err != nil {
		t.Fatalf("resolveOfflineRelease() error = %v", err)
	}
	if release.TagName != key.Tag || len(release.Assets) != 2 {
		t.Fatalf("offline release = %#v", release)
	}
	asset := release.Assets[0]
	if asset.ID != secondKey.AssetID || asset.Name != secondKey.AssetName || asset.Digest != "sha256:"+secondArchiveDigest {
		t.Fatalf("offline asset = %#v", asset)
	}
}

func TestSyncOneRegistryRejectsClientAndSnapshotValidationErrors(t *testing.T) {
	source := mustCommandSource(t)
	configured := registry.Registry{Name: "stable", Source: source.String()}
	cache, err := registry.NewCacheAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := syncOneRegistry(context.Background(), nil, configured, cache); err == nil || !strings.Contains(err.Error(), "client is nil") {
		t.Fatalf("nil client error = %v", err)
	}
	if err := syncOneRegistry(context.Background(), &fakeRegistryContentsClient{}, configured, nil); err == nil || !strings.Contains(err.Error(), "cache is nil") {
		t.Fatalf("nil cache error = %v", err)
	}
	if err := syncOneRegistry(context.Background(), &fakeRegistryContentsClient{}, registry.Registry{Name: "stable", Source: "github:acme/catalog@main:recipes"}, cache); err == nil {
		t.Fatal("invalid configured source unexpectedly succeeded")
	}

	if err := recipepolicy.Save(recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{
		Host: recipepolicy.GitHubHost, Owner: source.Owner, Repository: source.Repository, PathPrefix: "recipes",
	}}}); err != nil {
		t.Fatal(err)
	}
	recipeData, err := recipe.Marshal(recipe.Recipe{SchemaVersion: recipe.SchemaVersion, Repository: "acme/widget"})
	if err != nil {
		t.Fatal(err)
	}
	validIndex := registry.Index{SchemaVersion: registry.SchemaVersion, Entries: []registry.IndexEntry{{
		Repository: "acme/widget", Path: "widget.json", SHA256: registry.SHA256(recipeData),
	}}}
	indexData, err := registry.MarshalIndex(validIndex)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		index    []byte
		recipe   []byte
		fetchErr error
		want     string
	}{
		{name: "index fetch", fetchErr: errors.New("network down"), want: "fetch registry index"},
		{name: "invalid index", index: []byte(`{"schema_version":1,"entries":[],"extra":true}`), want: "validate registry index"},
		{name: "hash mismatch", index: indexData, recipe: []byte("wrong"), want: "SHA-256"},
		{name: "invalid recipe", index: mustCommandIndex(t, registry.Index{SchemaVersion: registry.SchemaVersion, Entries: []registry.IndexEntry{{Repository: "acme/widget", Path: "widget.json", SHA256: registry.SHA256([]byte("{}"))}}}), recipe: []byte("{}"), want: "validate recipe"},
		{name: "repository mismatch", index: mustCommandIndex(t, registry.Index{SchemaVersion: registry.SchemaVersion, Entries: []registry.IndexEntry{{Repository: "acme/widget", Path: "widget.json", SHA256: registry.SHA256(mustCommandRecipe(t, "acme/other"))}}}), recipe: mustCommandRecipe(t, "acme/other"), want: "does not match index"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeRegistryContentsClient{index: test.index, recipe: test.recipe, fetchErr: test.fetchErr}
			if client.index == nil {
				client.index = indexData
			}
			if client.recipe == nil {
				client.recipe = recipeData
			}
			if err := syncOneRegistry(context.Background(), client, configured, cache); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("syncOneRegistry() error = %v, want %q", err, test.want)
			}
		})
	}
}

type fakeRegistryContentsClient struct {
	index    []byte
	recipe   []byte
	fetchErr error
}

func (f *fakeRegistryContentsClient) ContentsAtCommit(_ context.Context, _, _, _, path string) ([]byte, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	if strings.HasSuffix(path, registry.IndexFileName) {
		return f.index, nil
	}
	return f.recipe, nil
}

func mustCommandSource(t *testing.T) registry.Source {
	t.Helper()
	source, err := registry.ParseSource("github:acme/catalog@" + commandRegistryCommit + ":recipes")
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func mustCommandRecipe(t *testing.T, repository string) []byte {
	t.Helper()
	data, err := recipe.Marshal(recipe.Recipe{SchemaVersion: recipe.SchemaVersion, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustCommandIndex(t *testing.T, index registry.Index) []byte {
	t.Helper()
	data, err := registry.MarshalIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

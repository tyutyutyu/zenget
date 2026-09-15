package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/recipe"
	"zenget/internal/recipepolicy"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func TestParseSourceCanonicalizesCommitAndResolvesPaths(t *testing.T) {
	source, err := ParseSource("github:Acme/recipes@" + strings.ToUpper(testCommit) + ":catalog")
	if err != nil {
		t.Fatalf("ParseSource() error = %v", err)
	}
	if got, want := source.String(), "github:Acme/recipes@"+testCommit+":catalog"; got != want {
		t.Fatalf("source.String() = %q, want %q", got, want)
	}
	if got, want := source.RepositoryName(), "Acme/recipes"; got != want {
		t.Fatalf("RepositoryName() = %q, want %q", got, want)
	}
	if got, err := source.IndexPath(); err != nil || got != "catalog/registry.json" {
		t.Fatalf("IndexPath() = %q, error = %v", got, err)
	}
	identity, err := source.PolicyIdentity("recipes/acme.json")
	if err != nil {
		t.Fatalf("PolicyIdentity() error = %v", err)
	}
	if identity.Path != "catalog/recipes/acme.json" {
		t.Fatalf("policy path = %q, want catalog/recipes/acme.json", identity.Path)
	}
}

func TestParseSourceRejectsMovingOrUnsafeReferences(t *testing.T) {
	values := []string{
		"github:acme/recipes@main:catalog",
		"github:acme/recipes@" + testCommit[:12] + ":catalog",
		"github:acme/recipes@" + testCommit + ":../catalog",
		"github:acme/recipes@" + testCommit + ":/catalog",
		"github:acme/recipes@" + testCommit + ":catalog/../other",
		"https://github.com/acme/recipes@" + testCommit + ":catalog",
		"github:acme/recipes@" + testCommit + ":catalog?token=secret",
		"github:acme/recipes@" + testCommit + ":catalog//nested",
	}
	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseSource(value); err == nil {
				t.Fatalf("ParseSource(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestConfigRoundTripIsAtomicAndDeterministic(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if loaded, err := LoadConfig(); err != nil || loaded.SchemaVersion != SchemaVersion || len(loaded.Registries) != 0 {
		t.Fatalf("empty LoadConfig() = %#v, error %v", loaded, err)
	}
	config := DefaultConfig()
	changed, err := config.Add("zeta", "github:acme/z@"+testCommit+":catalog")
	if err != nil || !changed {
		t.Fatalf("first Add() = changed %t, error %v", changed, err)
	}
	changed, err = config.Add("alpha", "github:acme/a@"+testCommit+":")
	if err != nil || !changed {
		t.Fatalf("second Add() = changed %t, error %v", changed, err)
	}
	if changed, err = config.Add("alpha", "github:acme/a@"+testCommit+":"); err != nil || changed {
		t.Fatalf("idempotent Add() = changed %t, error %v", changed, err)
	}
	if err := SaveConfig(config); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	if err := SaveConfig(config); err != nil {
		t.Fatalf("second SaveConfig() error = %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"name": "alpha"`)) || bytes.Index(data, []byte(`"name": "alpha"`)) > bytes.Index(data, []byte(`"name": "zeta"`)) {
		t.Fatalf("config is not sorted: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config mode = %v, error = %v, want 0600", info.Mode().Perm(), err)
	}
	loaded, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if got, want := loaded.Registries, config.Registries; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("loaded registries = %#v, want %#v", got, want)
	}

	if _, err := DecodeConfig([]byte(`{"schema_version":1,"registries":[{"name":"a","source":"github:acme/a@` + testCommit + `:"},{"name":"a","source":"github:acme/a@` + testCommit + `:"}]}`)); err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("duplicate config error = %v", err)
	}
	if _, err := DecodeConfig([]byte(`{"schema_version":1,"registries":[],"unknown":true}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown config error = %v", err)
	}
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{"missing schema", `{"registries":[]}`, "missing schema_version"},
		{"null registries", `{"schema_version":1,"registries":null}`, "must be an array"},
		{"multiple values", `{"schema_version":1,"registries":[]} {}`, "multiple JSON values"},
		{"not object", `[]`, "cannot unmarshal array"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeConfig([]byte(test.data)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestConfigMutationsAndRegistryUtilities(t *testing.T) {
	var nilConfig *Config
	if _, err := nilConfig.Add("name", "github:acme/repo@"+testCommit+":"); err == nil {
		t.Fatal("nil Add() unexpectedly succeeded")
	}
	if _, err := nilConfig.Remove("name"); err == nil {
		t.Fatal("nil Remove() unexpectedly succeeded")
	}

	config := DefaultConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	source := "github:acme/repo@" + testCommit + ":"
	if changed, err := config.Add("stable", source); err != nil || !changed {
		t.Fatalf("Add() = changed %t, error %v", changed, err)
	}
	if changed, err := config.Add("stable", "github:acme/other@"+testCommit+":"); err == nil || changed || !strings.Contains(err.Error(), "different source") {
		t.Fatalf("different-source Add() = changed %t, error %v", changed, err)
	}
	if entry, found, err := config.Find("stable"); err != nil || !found || entry.Source != source {
		t.Fatalf("Find(existing) = %#v, found %t, error %v", entry, found, err)
	}
	if _, found, err := config.Find("missing"); err != nil || found {
		t.Fatalf("Find(missing) = found %t, error %v", found, err)
	}
	if changed, err := config.Remove("stable"); err != nil || !changed {
		t.Fatalf("Remove(existing) = changed %t, error %v", changed, err)
	}
	if changed, err := config.Remove("missing"); err != nil || changed {
		t.Fatalf("Remove(missing) = changed %t, error %v", changed, err)
	}
	expectedDigest := sha256.Sum256([]byte("registry"))
	if got, want := SHA256([]byte("registry")), hex.EncodeToString(expectedDigest[:]); got != want {
		t.Fatalf("SHA256() = %q, want %q", got, want)
	}

	if got := (Source{}).String(); got != "<invalid-registry-source>" {
		t.Fatalf("invalid Source.String() = %q", got)
	}
	root := mustSource(t, "github:acme/repo@"+testCommit+":")
	if got, err := root.Path("recipe.json"); err != nil || got != "recipe.json" {
		t.Fatalf("root Path() = %q, error %v", got, err)
	}
	if _, err := root.Path(""); err == nil {
		t.Fatal("empty Path() unexpectedly succeeded")
	}
	if _, err := root.PolicyIdentity("../secret"); err == nil {
		t.Fatal("unsafe PolicyIdentity() unexpectedly succeeded")
	}

	snapshot := Snapshot{Index: Index{Entries: []IndexEntry{{Repository: "acme/repo", Path: "recipe.json", SHA256: strings.Repeat("a", 64)}}}, Recipes: map[string][]byte{}}
	if _, _, ok := snapshot.Recipe("missing"); ok {
		t.Fatal("missing Snapshot.Recipe() unexpectedly succeeded")
	}
	if _, _, ok := snapshot.Recipe("acme/repo"); ok {
		t.Fatal("Snapshot.Recipe() succeeded without recipe bytes")
	}
}

func TestCacheConstructorsAndPrivateFilesystemHelpers(t *testing.T) {
	if got := (*Cache)(nil).Root(); got != "" {
		t.Fatalf("nil Cache.Root() = %q", got)
	}
	if _, err := NewCacheAt(""); err == nil {
		t.Fatal("empty NewCacheAt() unexpectedly succeeded")
	}
	fileRoot := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(fileRoot, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCacheAt(fileRoot); err == nil {
		t.Fatal("file NewCacheAt() unexpectedly succeeded")
	}
	symlinkRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(t.TempDir(), symlinkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCacheAt(symlinkRoot); err == nil {
		t.Fatal("symlink NewCacheAt() unexpectedly succeeded")
	}

	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	root, err := CacheRoot()
	if err != nil || root != filepath.Join(cacheHome, "zenget", "registries") {
		t.Fatalf("CacheRoot() = %q, error %v", root, err)
	}
	cache, err := NewCache()
	if err != nil || cache.Root() != root {
		t.Fatalf("NewCache() = %#v, error %v", cache, err)
	}

	fallbackHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", fallbackHome)
	if got, err := CacheRoot(); err != nil || got != filepath.Join(fallbackHome, ".cache", "zenget", "registries") {
		t.Fatalf("fallback CacheRoot() = %q, error %v", got, err)
	}
	if got, err := Path(); err != nil || got != filepath.Join(fallbackHome, ".config", "zenget", ConfigFileName) {
		t.Fatalf("fallback Path() = %q, error %v", got, err)
	}

	privateDir := filepath.Join(t.TempDir(), "private")
	if err := ensurePrivateDirectory(privateDir, "test directory"); err != nil {
		t.Fatalf("ensurePrivateDirectory() error = %v", err)
	}
	if info, err := os.Lstat(privateDir); err != nil {
		t.Fatal(err)
	} else if err := validatePrivateDirectory(info, "test directory"); err != nil {
		t.Fatalf("validatePrivateDirectory(valid) error = %v", err)
	}
	if err := os.Chmod(privateDir, 0777); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(privateDir); err != nil {
		t.Fatal(err)
	} else if err := validatePrivateDirectory(info, "test directory"); err == nil {
		t.Fatal("world-writable directory unexpectedly accepted")
	}
	if err := os.Chmod(privateDir, 0700|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(privateDir); err != nil {
		t.Fatal(err)
	} else if err := validatePrivateDirectory(info, "test directory"); err == nil {
		t.Fatal("sticky directory unexpectedly accepted")
	}

	privateFile := filepath.Join(t.TempDir(), "private.json")
	if err := os.WriteFile(privateFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := readPrivateFile(privateFile, "test file"); err != nil || string(data) != "{}" {
		t.Fatalf("readPrivateFile(valid) = %q, error %v", data, err)
	}
	if _, err := readPrivateFile(filepath.Join(filepath.Dir(privateFile), "missing"), "test file"); err == nil {
		t.Fatal("missing readPrivateFile() unexpectedly succeeded")
	}
	if err := os.Chmod(privateFile, 0644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(privateFile); err != nil {
		t.Fatal(err)
	} else if err := validatePrivateFile(info, "test file"); err == nil {
		t.Fatal("permissive file unexpectedly accepted")
	}
	linkFile := filepath.Join(filepath.Dir(privateFile), "link.json")
	if err := os.Symlink(privateFile, linkFile); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(linkFile); err != nil {
		t.Fatal(err)
	} else if err := validatePrivateFile(info, "test file"); err == nil {
		t.Fatal("symlink file unexpectedly accepted")
	}

	writePath := filepath.Join(t.TempDir(), "write.json")
	if err := writeFileSync(writePath, []byte("one"), 0600, "test file"); err != nil {
		t.Fatalf("writeFileSync() error = %v", err)
	}
	if err := writeFileSync(writePath, []byte("two"), 0600, "test file"); err == nil {
		t.Fatal("duplicate writeFileSync() unexpectedly succeeded")
	}
	if err := atomicWrite(writePath, filepath.Dir(writePath), ".test-*", []byte("three"), 0600, "test file"); err != nil {
		t.Fatalf("atomicWrite(existing) error = %v", err)
	}
	if data, err := os.ReadFile(writePath); err != nil || string(data) != "three" {
		t.Fatalf("atomicWrite() result = %q, error %v", data, err)
	}
}

func TestStrictMetadataAndMarshalValidation(t *testing.T) {
	validSource := "github:acme/registry@" + testCommit + ":catalog"
	validPointer := snapshotPointer{SchemaVersion: SchemaVersion, Source: validSource, IndexSHA256: strings.Repeat("a", 64)}
	if data, err := marshalPointer(validPointer); err != nil {
		t.Fatalf("marshalPointer() error = %v", err)
	} else if got, err := decodePointer(data); err != nil || got != validPointer {
		t.Fatalf("decodePointer(valid) = %#v, error %v", got, err)
	}
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{"missing field", `{}`, "missing schema_version"},
		{"bad schema", `{"schema_version":2,"source":"` + validSource + `","index_sha256":"` + strings.Repeat("a", 64) + `"}`, "unsupported"},
		{"bad source", `{"schema_version":1,"source":"github:acme/registry@main:catalog","index_sha256":"` + strings.Repeat("a", 64) + `"}`, "metadata source"},
		{"bad digest", `{"schema_version":1,"source":"` + validSource + `","index_sha256":"bad"}`, "invalid index digest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodePointer([]byte(test.data)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decodePointer() error = %v, want %q", err, test.want)
			}
		})
	}
	if _, err := MarshalIndex(Index{SchemaVersion: SchemaVersion + 1}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("MarshalIndex() error = %v", err)
	}
	if _, err := MarshalConfig(Config{SchemaVersion: SchemaVersion + 1}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("MarshalConfig() error = %v", err)
	}
}

func TestRegistryConfigRejectsInsecureExistingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	config := DefaultConfig()
	if _, err := config.Add("stable", "github:acme/catalog@"+testCommit+":recipes"); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	configPath, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("permissive LoadConfig() error = %v", err)
	}
	if err := SaveConfig(config); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("permissive SaveConfig() error = %v", err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink LoadConfig() error = %v", err)
	}
	if err := SaveConfig(config); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink SaveConfig() error = %v", err)
	}
}

func TestCacheRejectsMissingRecipesAndInvalidInputs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	source := mustSource(t, "github:acme/registry@"+testCommit+":catalog")
	if err := recipepolicy.Save(recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{
		Host: recipepolicy.GitHubHost, Owner: source.Owner, Repository: source.Repository, PathPrefix: "catalog",
	}}}); err != nil {
		t.Fatal(err)
	}
	cache, err := NewCacheAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(source); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("missing Load() error = %v", err)
	}
	if _, _, err := cache.Lookup(source, "acme/widget"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("missing Lookup() error = %v", err)
	}
	if err := (*Cache)(nil).Save(source, Index{}, nil); err == nil {
		t.Fatal("nil Cache.Save() unexpectedly succeeded")
	}
	if err := cache.Save(Source{}, Index{}, nil); err == nil {
		t.Fatal("invalid source Save() unexpectedly succeeded")
	}

	recipeData := mustRecipe(t, "acme/widget")
	index := Index{SchemaVersion: SchemaVersion, Entries: []IndexEntry{{
		Repository: "acme/widget", Path: "widget.json", SHA256: SHA256(recipeData),
	}}}
	for _, test := range []struct {
		name    string
		recipes map[string][]byte
		want    string
	}{
		{"wrong map length", nil, "recipe set does not match index"},
		{"missing path", map[string][]byte{"other.json": recipeData}, "missing recipe"},
		{"wrong digest", map[string][]byte{"widget.json": []byte("wrong")}, "SHA-256"},
		{"invalid recipe", map[string][]byte{"widget.json": []byte("{}")}, "validate recipe"},
		{"wrong repository", map[string][]byte{"widget.json": mustRecipe(t, "acme/other")}, "does not match index"},
	} {
		t.Run(test.name, func(t *testing.T) {
			saveIndex := index
			if test.name == "invalid recipe" || test.name == "wrong repository" {
				saveIndex.Entries = append([]IndexEntry(nil), index.Entries...)
				saveIndex.Entries[0].SHA256 = SHA256(test.recipes["widget.json"])
			}
			if err := cache.Save(source, saveIndex, test.recipes); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Save() error = %v, want %q", err, test.want)
			}
		})
	}

	if err := cache.Save(source, index, map[string][]byte{"widget.json": recipeData}); err != nil {
		t.Fatalf("valid Save() error = %v", err)
	}
	if _, _, err := cache.Lookup(source, "acme/missing"); !errors.Is(err, ErrRecipeNotFound) || !strings.Contains(err.Error(), `"acme/missing"`) {
		t.Fatalf("missing recipe Lookup() error = %v", err)
	}
	if _, _, err := cache.Lookup(source, "acme/widget"); err != nil {
		t.Fatalf("valid Lookup() error = %v", err)
	}
}

func TestIndexStrictValidation(t *testing.T) {
	valid := Index{SchemaVersion: SchemaVersion, Entries: []IndexEntry{
		{Repository: "acme/widget", Path: "recipes/widget.json", SHA256: strings.Repeat("a", 64)},
		{Repository: "acme/zap", Path: "recipes/zap.json", SHA256: strings.Repeat("b", 64)},
	}}
	data, err := MarshalIndex(valid)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeIndex(data); err != nil || !bytes.Equal(gotEntriesJSON(t, got), gotEntriesJSON(t, valid)) {
		t.Fatalf("valid index decode = %#v, error %v", got, err)
	}
	tests := []struct {
		name string
		data string
		want string
	}{
		{"duplicate key", `{"schema_version":1,"entries":[],"entries":[]}`, "duplicate JSON object key"},
		{"unknown field", `{"schema_version":1,"entries":[],"extra":true}`, "unknown field"},
		{"duplicate repository", `{"schema_version":1,"entries":[{"repository":"acme/a","path":"a.json","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"repository":"acme/a","path":"b.json","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`, "duplicate repository"},
		{"duplicate path", `{"schema_version":1,"entries":[{"repository":"acme/a","path":"same.json","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"repository":"acme/b","path":"same.json","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`, "duplicate path"},
		{"bad digest", `{"schema_version":1,"entries":[{"repository":"acme/a","path":"a.json","sha256":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]}`, "lowercase"},
		{"traversal", `{"schema_version":1,"entries":[{"repository":"acme/a","path":"../a.json","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`, "dot-segments"},
		{"unsorted", `{"schema_version":1,"entries":[{"repository":"acme/z","path":"z.json","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},{"repository":"acme/a","path":"a.json","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`, "canonically sorted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeIndex([]byte(test.data)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeIndex() error = %v, want %q", err, test.want)
			}
		})
	}
}

func gotEntriesJSON(t *testing.T, index Index) []byte {
	t.Helper()
	data, err := json.Marshal(index.Entries)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCachePublishesAndValidatesOneActiveSnapshot(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	source := mustSource(t, "github:acme/registry@"+testCommit+":catalog")
	policy := recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{
		Host: recipepolicy.GitHubHost, Owner: source.Owner, Repository: source.Repository, PathPrefix: "catalog",
	}}}
	if err := recipepolicy.Save(policy); err != nil {
		t.Fatal(err)
	}
	recipeData := mustRecipe(t, "acme/widget")
	digest := sha256.Sum256(recipeData)
	index := Index{SchemaVersion: SchemaVersion, Entries: []IndexEntry{{
		Repository: "acme/widget", Path: "recipes/widget.json", SHA256: hex.EncodeToString(digest[:]),
	}}}
	cache, err := NewCacheAt(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(source, index, map[string][]byte{"recipes/widget.json": recipeData}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	pointerPath := filepath.Join(cache.Root(), sourceNamespaceForTest(source), SnapshotPointerFileName)
	pointerData, err := os.ReadFile(pointerPath)
	if err != nil {
		t.Fatal(err)
	}
	var pointer snapshotPointer
	if err := json.Unmarshal(pointerData, &pointer); err != nil {
		t.Fatal(err)
	}
	snapshotDir := filepath.Join(cache.Root(), sourceNamespaceForTest(source), "snapshots", pointer.IndexSHA256)
	if err := cache.Save(source, index, map[string][]byte{"recipes/widget.json": recipeData}); err != nil {
		t.Fatalf("duplicate Save() error = %v", err)
	}
	if _, err := os.Stat(snapshotDir); err != nil {
		t.Fatalf("snapshot after duplicate Save() = %v", err)
	}
	if err := os.Remove(pointerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pointerPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(source, index, map[string][]byte{"recipes/widget.json": recipeData}); err == nil {
		t.Fatal("Save() with invalid pointer destination unexpectedly succeeded")
	}
	if _, err := os.Stat(snapshotDir); err != nil {
		t.Fatalf("active snapshot was removed after pointer failure: %v", err)
	}
	if err := os.RemoveAll(pointerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pointerPath, pointerData, 0600); err != nil {
		t.Fatal(err)
	}
	entry, got, err := cache.Lookup(source, "acme/widget")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if entry.Repository != "acme/widget" || !bytes.Equal(got, recipeData) {
		t.Fatalf("Lookup() = %#v, %q", entry, got)
	}
	got[0] = '{' ^ got[0]
	_, gotAgain, err := cache.Lookup(source, "acme/widget")
	if err != nil || !bytes.Equal(gotAgain, recipeData) {
		t.Fatalf("defensive Lookup() = %q, error %v", gotAgain, err)
	}

	if err := os.WriteFile(filepath.Join(cache.Root(), "bad"), []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recipepolicy.Save(recipepolicy.Default()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Lookup(source, "acme/widget"); err == nil || !strings.Contains(err.Error(), "policy denied") {
		t.Fatalf("revoked policy Lookup() error = %v", err)
	}
}

func TestCacheRejectsCorruptRecipeWithoutFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	source := mustSource(t, "github:acme/registry@"+testCommit+":catalog")
	if err := recipepolicy.Save(recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{Host: recipepolicy.GitHubHost, Owner: source.Owner, Repository: source.Repository, PathPrefix: "catalog"}}}); err != nil {
		t.Fatal(err)
	}
	recipeData := mustRecipe(t, "acme/widget")
	digest := sha256.Sum256(recipeData)
	index := Index{SchemaVersion: SchemaVersion, Entries: []IndexEntry{{Repository: "acme/widget", Path: "recipes/widget.json", SHA256: hex.EncodeToString(digest[:])}}}
	cache, err := NewCacheAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(source, index, map[string][]byte{"recipes/widget.json": recipeData}); err != nil {
		t.Fatal(err)
	}
	pointerPath := filepath.Join(cache.Root(), sourceNamespaceForTest(source), SnapshotPointerFileName)
	pointerData, err := os.ReadFile(pointerPath)
	if err != nil {
		t.Fatal(err)
	}
	var pointer snapshotPointer
	if err := json.Unmarshal(pointerData, &pointer); err != nil {
		t.Fatal(err)
	}
	recipePath := filepath.Join(cache.Root(), sourceNamespaceForTest(source), "snapshots", pointer.IndexSHA256, "recipes", index.Entries[0].SHA256+".json")
	if err := os.WriteFile(recipePath, []byte(`{"schema_version":1,"repository":"acme/other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Lookup(source, "acme/widget"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt Lookup() error = %v", err)
	}
	if errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("corrupt cache was treated as missing: %v", err)
	}
}

func TestCacheLoadFailsClosedForSnapshotTampering(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	source := mustSource(t, "github:acme/registry@"+testCommit+":catalog")
	if err := recipepolicy.Save(recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{
		Host: recipepolicy.GitHubHost, Owner: source.Owner, Repository: source.Repository, PathPrefix: "catalog",
	}}}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, cache *Cache, source Source, snapshotDir, recipePath string, pointer snapshotPointer)
		want   string
	}{
		{
			name: "pointer source mismatch",
			mutate: func(t *testing.T, cache *Cache, source Source, _ string, _ string, pointer snapshotPointer) {
				pointerPath := filepath.Join(cache.sourceDir(source), SnapshotPointerFileName)
				other := mustSource(t, "github:acme/other@"+testCommit+":catalog")
				pointer.Source = other.String()
				data, err := marshalPointer(pointer)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(pointerPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			},
			want: "does not match configuration",
		},
		{
			name: "pointer digest invalid",
			mutate: func(t *testing.T, cache *Cache, source Source, _ string, _ string, pointer snapshotPointer) {
				pointerPath := filepath.Join(cache.sourceDir(source), SnapshotPointerFileName)
				pointer.IndexSHA256 = "invalid"
				data, err := marshalPointer(pointer)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(pointerPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			},
			want: "invalid index digest",
		},
		{
			name: "pointer missing",
			mutate: func(t *testing.T, cache *Cache, source Source, _ string, _ string, _ snapshotPointer) {
				pointerPath := filepath.Join(cache.sourceDir(source), SnapshotPointerFileName)
				if err := os.Remove(pointerPath); err != nil {
					t.Fatal(err)
				}
			},
			want: "not synchronized",
		},
		{
			name: "snapshot target missing",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, _ snapshotPointer) {
				if err := os.RemoveAll(snapshotDir); err != nil {
					t.Fatal(err)
				}
			},
			want: "target is missing",
		},
		{
			name: "namespace symlink",
			mutate: func(t *testing.T, cache *Cache, source Source, _, _ string, _ snapshotPointer) {
				namespace := cache.sourceDir(source)
				if err := os.RemoveAll(namespace); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), namespace); err != nil {
					t.Fatal(err)
				}
			},
			want: "namespace is not a safe directory",
		},
		{
			name: "snapshot directory symlink",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, _ snapshotPointer) {
				snapshotsDir := filepath.Dir(snapshotDir)
				if err := os.RemoveAll(snapshotsDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), snapshotsDir); err != nil {
					t.Fatal(err)
				}
			},
			want: "registry snapshot directory is not a safe directory",
		},
		{
			name: "snapshot target symlink",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, _ snapshotPointer) {
				if err := os.RemoveAll(snapshotDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), snapshotDir); err != nil {
					t.Fatal(err)
				}
			},
			want: "snapshot target is not a directory",
		},
		{
			name: "unexpected snapshot file",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, _ snapshotPointer) {
				if err := os.WriteFile(filepath.Join(snapshotDir, "unexpected"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected file",
		},
		{
			name: "metadata mismatch",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, pointer snapshotPointer) {
				pointer.Source = "github:acme/other@" + testCommit + ":catalog"
				data, err := marshalPointer(pointer)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(snapshotDir, "snapshot.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			},
			want: "metadata does not match",
		},
		{
			name: "snapshot root symlink",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, _ snapshotPointer) {
				metadataPath := filepath.Join(snapshotDir, "snapshot.json")
				if err := os.Remove(metadataPath); err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(t.TempDir(), "snapshot.json")
				if err := os.WriteFile(replacement, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(replacement, metadataPath); err != nil {
					t.Fatal(err)
				}
			},
			want: "unsafe entry",
		},
		{
			name: "invalid index",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, _ snapshotPointer) {
				if err := os.WriteFile(filepath.Join(snapshotDir, "index.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			want: "missing schema_version",
		},
		{
			name: "recipe directory missing",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, _ snapshotPointer) {
				if err := os.RemoveAll(filepath.Join(snapshotDir, "recipes")); err != nil {
					t.Fatal(err)
				}
			},
			want: "recipe directory is missing",
		},
		{
			name: "recipe directory symlink",
			mutate: func(t *testing.T, _ *Cache, _ Source, snapshotDir, _ string, _ snapshotPointer) {
				recipesDir := filepath.Join(snapshotDir, "recipes")
				if err := os.RemoveAll(recipesDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), recipesDir); err != nil {
					t.Fatal(err)
				}
			},
			want: "unsafe entry",
		},
		{
			name: "unexpected recipe file",
			mutate: func(t *testing.T, _ *Cache, _ Source, _, recipePath string, _ snapshotPointer) {
				if err := os.WriteFile(filepath.Join(filepath.Dir(recipePath), strings.Repeat("f", 64)+".json"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			want: "unexpected recipe file",
		},
		{
			name: "recipe missing",
			mutate: func(t *testing.T, _ *Cache, _ Source, _, recipePath string, _ snapshotPointer) {
				if err := os.Remove(recipePath); err != nil {
					t.Fatal(err)
				}
			},
			want: "registry recipe snapshot is missing",
		},
		{
			name: "recipe symlink",
			mutate: func(t *testing.T, _ *Cache, _ Source, _, recipePath string, _ snapshotPointer) {
				if err := os.Remove(recipePath); err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(t.TempDir(), "recipe.json")
				if err := os.WriteFile(replacement, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(replacement, recipePath); err != nil {
					t.Fatal(err)
				}
			},
			want: "through symlink",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			recipeData := mustRecipe(t, "acme/widget")
			index := Index{SchemaVersion: SchemaVersion, Entries: []IndexEntry{{
				Repository: "acme/widget", Path: "recipes/widget.json", SHA256: SHA256(recipeData),
			}}}
			cache, err := NewCacheAt(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := cache.Save(source, index, map[string][]byte{"recipes/widget.json": recipeData}); err != nil {
				t.Fatal(err)
			}
			namespace := cache.sourceDir(source)
			pointerPath := filepath.Join(namespace, SnapshotPointerFileName)
			pointerData, err := os.ReadFile(pointerPath)
			if err != nil {
				t.Fatal(err)
			}
			pointer, err := decodePointer(pointerData)
			if err != nil {
				t.Fatal(err)
			}
			snapshotDir := filepath.Join(namespace, "snapshots", pointer.IndexSHA256)
			recipePath := filepath.Join(snapshotDir, "recipes", index.Entries[0].SHA256+".json")
			test.mutate(t, cache, source, snapshotDir, recipePath, pointer)
			if _, err := cache.Load(source); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("tampered Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func mustSource(t *testing.T, value string) Source {
	t.Helper()
	source, err := ParseSource(value)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func mustRecipe(t *testing.T, repository string) []byte {
	t.Helper()
	data, err := recipe.Marshal(recipe.Recipe{SchemaVersion: recipe.SchemaVersion, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sourceNamespaceForTest(source Source) string {
	digest := sha256.Sum256([]byte(source.String()))
	return hex.EncodeToString(digest[:])
}

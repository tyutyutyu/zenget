package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"zenget/internal/recipe"
	"zenget/internal/recipepolicy"
	"zenget/internal/registry"
)

const commandRegistryCommit = "0123456789abcdef0123456789abcdef01234567"

func TestRegistryAddRemoveAndList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	source := "github:acme/catalog@" + commandRegistryCommit + ":recipes"
	cmd, output, _ := newBufferedCommand()
	if err := runRegistryAdd(cmd, []string{"stable", source}); err != nil {
		t.Fatalf("runRegistryAdd() error = %v", err)
	}
	if !strings.Contains(output.String(), "Added registry stable") {
		t.Fatalf("add output = %q", output.String())
	}

	output.Reset()
	registryListJSON = true
	t.Cleanup(func() { registryListJSON = false })
	if err := runRegistryList(cmd, nil); err != nil {
		t.Fatalf("runRegistryList() error = %v", err)
	}
	var listed registry.Config
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil {
		t.Fatalf("list JSON error = %v; output=%q", err, output.String())
	}
	if len(listed.Registries) != 1 || listed.Registries[0].Name != "stable" || listed.Registries[0].Source != source {
		t.Fatalf("listed config = %#v", listed)
	}

	output.Reset()
	if err := runRegistryRemove(cmd, []string{"stable"}); err != nil {
		t.Fatalf("runRegistryRemove() error = %v", err)
	}
	if !strings.Contains(output.String(), "Removed registry stable") {
		t.Fatalf("remove output = %q", output.String())
	}
}

func TestRegistrySyncDeniedPolicyDoesNotMakeNetworkRequest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := saveRegistryConfigForTest(t, "stable", "github:acme/catalog@"+commandRegistryCommit+":recipes"); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	cmd, _, _ := newBufferedCommand()
	cmd.Flags().String("api-base-url", server.URL, "")
	cmd.Flags().String("forge", "github", "")
	registrySyncAll = false
	registrySyncOffline = false
	if err := runRegistrySync(cmd, []string{"stable"}); err == nil || !strings.Contains(err.Error(), "policy denied") {
		t.Fatalf("denied sync error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("denied sync requests = %d, want 0", got)
	}
}

func TestRegistrySyncPublishesAndOfflineValidationUsesNoNetwork(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	sourceValue := "github:acme/catalog@" + commandRegistryCommit + ":recipes"
	if err := saveRegistryConfigForTest(t, "stable", sourceValue); err != nil {
		t.Fatal(err)
	}
	source, err := registry.ParseSource(sourceValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := recipepolicy.Save(recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{
		Host: recipepolicy.GitHubHost, Owner: source.Owner, Repository: source.Repository, PathPrefix: "recipes",
	}}}); err != nil {
		t.Fatal(err)
	}
	recipeData, err := recipe.Marshal(recipe.Recipe{SchemaVersion: recipe.SchemaVersion, Repository: "acme/widget", TargetName: "widget"})
	if err != nil {
		t.Fatal(err)
	}
	index := registry.Index{SchemaVersion: registry.SchemaVersion, Entries: []registry.IndexEntry{{
		Repository: "acme/widget", Path: "widget.json", SHA256: registry.SHA256(recipeData),
	}}}
	indexData, err := registry.MarshalIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.URL.Query().Get("ref"); got != commandRegistryCommit {
			t.Errorf("ref = %q", got)
		}
		switch r.URL.Path {
		case "/repos/acme/catalog/contents/recipes/registry.json":
			writeContentsResponse(w, "recipes/registry.json", indexData)
		case "/repos/acme/catalog/contents/recipes/widget.json":
			writeContentsResponse(w, "recipes/widget.json", recipeData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cmd, output, _ := newBufferedCommand()
	cmd.Flags().String("api-base-url", server.URL, "")
	cmd.Flags().String("forge", "github", "")
	registrySyncAll = false
	registrySyncOffline = false
	if err := runRegistrySync(cmd, []string{"stable"}); err != nil {
		t.Fatalf("online sync error = %v", err)
	}
	if !strings.Contains(output.String(), "Synchronized registry stable") {
		t.Fatalf("sync output = %q", output.String())
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("sync requests = %d, want 2", got)
	}

	output.Reset()
	registrySyncOffline = true
	t.Cleanup(func() { registrySyncOffline = false })
	if err := runRegistrySync(cmd, []string{"stable"}); err != nil {
		t.Fatalf("offline validation error = %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("offline validation requests = %d, want 2", got)
	}
	if !strings.Contains(output.String(), "Validated registry stable") {
		t.Fatalf("offline output = %q", output.String())
	}
}

func TestRegistrySyncPartialFailurePreservesPreviousSnapshot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	sourceValue := "github:acme/catalog@" + commandRegistryCommit + ":recipes"
	if err := saveRegistryConfigForTest(t, "stable", sourceValue); err != nil {
		t.Fatal(err)
	}
	source, err := registry.ParseSource(sourceValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := recipepolicy.Save(recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{Host: recipepolicy.GitHubHost, Owner: source.Owner, Repository: source.Repository, PathPrefix: "recipes"}}}); err != nil {
		t.Fatal(err)
	}
	oldRecipe, err := recipe.Marshal(recipe.Recipe{SchemaVersion: recipe.SchemaVersion, Repository: "acme/widget", TargetName: "old"})
	if err != nil {
		t.Fatal(err)
	}
	newRecipe, err := recipe.Marshal(recipe.Recipe{SchemaVersion: recipe.SchemaVersion, Repository: "acme/widget", TargetName: "new"})
	if err != nil {
		t.Fatal(err)
	}
	oldIndex := registry.Index{SchemaVersion: registry.SchemaVersion, Entries: []registry.IndexEntry{{Repository: "acme/widget", Path: "widget.json", SHA256: registry.SHA256(oldRecipe)}}}
	newIndex := registry.Index{SchemaVersion: registry.SchemaVersion, Entries: []registry.IndexEntry{{Repository: "acme/widget", Path: "widget.json", SHA256: registry.SHA256(newRecipe)}}}
	var indexData atomic.Value
	indexData.Store(mustRegistryIndex(t, oldIndex))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/catalog/contents/recipes/registry.json":
			writeContentsResponse(w, "recipes/registry.json", indexData.Load().([]byte))
		case "/repos/acme/catalog/contents/recipes/widget.json":
			writeContentsResponse(w, "recipes/widget.json", oldRecipe)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cmd, _, _ := newBufferedCommand()
	cmd.Flags().String("api-base-url", server.URL, "")
	cmd.Flags().String("forge", "github", "")
	registrySyncAll = false
	registrySyncOffline = false
	if err := runRegistrySync(cmd, []string{"stable"}); err != nil {
		t.Fatalf("initial sync error = %v", err)
	}

	indexData.Store(mustRegistryIndex(t, newIndex))
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/catalog/contents/recipes/registry.json" {
			writeContentsResponse(w, "recipes/registry.json", indexData.Load().([]byte))
			return
		}
		if r.URL.Path == "/repos/acme/catalog/contents/recipes/widget.json" {
			writeContentsResponse(w, "recipes/widget.json", []byte("corrupt"))
			return
		}
		http.NotFound(w, r)
	})
	if err := runRegistrySync(cmd, []string{"stable"}); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("partial sync error = %v", err)
	}
	cache, err := registry.NewCache()
	if err != nil {
		t.Fatal(err)
	}
	entry, data, err := cache.Lookup(source, "acme/widget")
	if err != nil {
		t.Fatalf("lookup after failed sync = %v", err)
	}
	if entry.SHA256 != oldIndex.Entries[0].SHA256 || !bytes.Equal(data, oldRecipe) {
		t.Fatalf("snapshot after failed sync = %#v, %q", entry, data)
	}
}

func TestRegistrySyncDeniedRecipePreflightsBeforeRecipeNetwork(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	sourceValue := "github:acme/catalog@" + commandRegistryCommit + ":recipes"
	if err := saveRegistryConfigForTest(t, "stable", sourceValue); err != nil {
		t.Fatal(err)
	}
	source, err := registry.ParseSource(sourceValue)
	if err != nil {
		t.Fatal(err)
	}
	if err := recipepolicy.Save(recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{Host: recipepolicy.GitHubHost, Owner: source.Owner, Repository: source.Repository, PathPrefix: "recipes/allowed"}}}); err != nil {
		t.Fatal(err)
	}
	recipeData, err := recipe.Marshal(recipe.Recipe{SchemaVersion: recipe.SchemaVersion, Repository: "acme/widget"})
	if err != nil {
		t.Fatal(err)
	}
	index := registry.Index{SchemaVersion: registry.SchemaVersion, Entries: []registry.IndexEntry{{Repository: "acme/widget", Path: "blocked/widget.json", SHA256: registry.SHA256(recipeData)}}}
	var recipeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/registry.json") {
			writeContentsResponse(w, "recipes/registry.json", mustRegistryIndex(t, index))
			return
		}
		recipeRequests.Add(1)
		writeContentsResponse(w, "recipes/blocked/widget.json", recipeData)
	}))
	defer server.Close()
	cmd, _, _ := newBufferedCommand()
	cmd.Flags().String("api-base-url", server.URL, "")
	cmd.Flags().String("forge", "github", "")
	registrySyncAll = false
	registrySyncOffline = false
	if err := runRegistrySync(cmd, []string{"stable"}); err == nil || !strings.Contains(err.Error(), "policy denied") {
		t.Fatalf("denied recipe sync error = %v", err)
	}
	if got := recipeRequests.Load(); got != 0 {
		t.Fatalf("denied recipe requests = %d, want 0", got)
	}
}

func saveRegistryConfigForTest(t *testing.T, name, source string) error {
	t.Helper()
	config := registry.DefaultConfig()
	if _, err := config.Add(name, source); err != nil {
		return err
	}
	return registry.SaveConfig(config)
}

func writeContentsResponse(w http.ResponseWriter, path string, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "file", "encoding": "base64", "path": path,
		"size": len(data), "content": encodeBase64(data),
	})
}

func encodeBase64(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var encoded strings.Builder
	encoded.Grow((len(data) + 2) / 3 * 4)
	for len(data) >= 3 {
		value := uint(data[0])<<16 | uint(data[1])<<8 | uint(data[2])
		encoded.WriteByte(alphabet[(value>>18)&63])
		encoded.WriteByte(alphabet[(value>>12)&63])
		encoded.WriteByte(alphabet[(value>>6)&63])
		encoded.WriteByte(alphabet[value&63])
		data = data[3:]
	}
	if len(data) == 1 {
		value := uint(data[0]) << 16
		encoded.WriteByte(alphabet[(value>>18)&63])
		encoded.WriteByte(alphabet[(value>>12)&63])
		encoded.WriteString("==")
	} else if len(data) == 2 {
		value := uint(data[0])<<16 | uint(data[1])<<8
		encoded.WriteByte(alphabet[(value>>18)&63])
		encoded.WriteByte(alphabet[(value>>12)&63])
		encoded.WriteByte(alphabet[(value>>6)&63])
		encoded.WriteByte('=')
	}
	return encoded.String()
}

func mustRegistryIndex(t *testing.T, index registry.Index) []byte {
	t.Helper()
	data, err := registry.MarshalIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

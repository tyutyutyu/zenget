package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"zenget/internal/github"
	"zenget/internal/recipe"
	"zenget/internal/recipepolicy"
	"zenget/internal/registry"
)

func TestInstallUsesExactlyOneRegistrySnapshotRecipe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedRegistrySnapshot(t, "stable", "acme/widget", "widget")

	binary := []byte("#!/bin/sh\necho registry\n")
	asset := tarGzAsset(t, "widget", binary)
	server := releaseServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(asset)
	})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = jsonEncode(w, releasePayload(server, "widget.tar.gz"))
			return
		}
		if r.URL.Path == "/assets/widget.tar.gz" {
			_, _ = w.Write(asset)
			return
		}
		http.NotFound(w, r)
	})
	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		registryName: "stable", registryNameSet: true, goos: "linux", goarch: "amd64", input: strings.NewReader(""),
	}, io.Discard); err != nil {
		t.Fatalf("registry install error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); err != nil {
		t.Fatalf("registry-selected target missing: %v", err)
	}
}

func TestInstallRegistryRejectsMutualExclusionUnknownAndCorruptSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedRegistrySnapshot(t, "stable", "acme/widget", "widget")

	mutualErr := runInstallWithOptions(context.Background(), github.New(), "acme/widget", installOptions{
		registryName: "stable", registryNameSet: true, recipePathSet: true, goos: "linux", goarch: "amd64",
	}, io.Discard)
	if mutualErr == nil || !strings.Contains(mutualErr.Error(), "mutually exclusive") {
		t.Fatalf("mutual exclusion error = %v", mutualErr)
	}

	unknownErr := runInstallWithOptions(context.Background(), github.New(), "acme/widget", installOptions{
		registryName: "missing", registryNameSet: true, goos: "linux", goarch: "amd64",
	}, io.Discard)
	if unknownErr == nil || !strings.Contains(unknownErr.Error(), "unknown registry") {
		t.Fatalf("unknown registry error = %v", unknownErr)
	}

	cache, err := registry.NewCache()
	if err != nil {
		t.Fatal(err)
	}
	source, err := registry.ParseSource("github:acme/catalog@" + commandRegistryCommit + ":recipes")
	if err != nil {
		t.Fatal(err)
	}
	pointerPath := filepath.Join(cache.Root(), sourceNamespaceForCommandTest(source), registry.SnapshotPointerFileName)
	pointer, err := os.ReadFile(pointerPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		IndexSHA256 string `json:"index_sha256"`
	}
	if err := jsonUnmarshal(pointer, &metadata); err != nil {
		t.Fatal(err)
	}
	recipePath := filepath.Join(cache.Root(), sourceNamespaceForCommandTest(source), "snapshots", metadata.IndexSHA256, "recipes")
	entries, err := os.ReadDir(recipePath)
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot recipes = %v, error %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(recipePath, entries[0].Name()), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	corruptErr := runInstallWithOptions(context.Background(), github.New(), "acme/widget", installOptions{
		registryName: "stable", registryNameSet: true, goos: "linux", goarch: "amd64",
	}, io.Discard)
	if corruptErr == nil || !strings.Contains(corruptErr.Error(), "SHA-256") {
		t.Fatalf("corrupt registry error = %v", corruptErr)
	}
}

func TestInstallOfflineRegistryMakesNoNetworkRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedRegistrySnapshot(t, "stable", "acme/widget", "widget")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "network must not be used", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := github.New()
	client.BaseURL = server.URL
	err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		registryName: "stable", registryNameSet: true, offline: true, goos: "linux", goarch: "amd64",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no network request") {
		t.Fatalf("offline error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("offline requests = %d, want 0", got)
	}
}

func TestInstallOfflineRegistryReusesVerifiedArtifactCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	seedRegistrySnapshot(t, "stable", "acme/widget", "widget")
	binary := []byte("#!/bin/sh\necho offline\n")
	asset := tarGzAsset(t, "widget", binary)
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = jsonEncode(w, releasePayload(server, "widget.tar.gz"))
			return
		}
		if r.URL.Path == "/assets/widget.tar.gz" {
			_, _ = w.Write(asset)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		registryName: "stable", registryNameSet: true, goos: "linux", goarch: "amd64", input: strings.NewReader(""),
	}, io.Discard); err != nil {
		t.Fatalf("seed install error = %v", err)
	}
	initialRequests := requests.Load()
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "offline network request", http.StatusInternalServerError)
	})
	if err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		registryName: "stable", registryNameSet: true, offline: true, force: true, tag: "v1.0.0", goos: "linux", goarch: "amd64", input: strings.NewReader(""),
	}, io.Discard); err != nil {
		t.Fatalf("offline cache install error = %v", err)
	}
	if got := requests.Load(); got != initialRequests {
		t.Fatalf("offline cache requests = %d, want %d", got, initialRequests)
	}
}

func seedRegistrySnapshot(t *testing.T, name, repository, target string) {
	t.Helper()
	sourceValue := "github:acme/catalog@" + commandRegistryCommit + ":recipes"
	config := registry.DefaultConfig()
	if _, err := config.Add(name, sourceValue); err != nil {
		t.Fatal(err)
	}
	if err := registry.SaveConfig(config); err != nil {
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
	installRecipe := recipe.Recipe{SchemaVersion: recipe.SchemaVersion, Repository: repository, TargetName: target}
	data, err := recipe.Marshal(installRecipe)
	if err != nil {
		t.Fatal(err)
	}
	index := registry.Index{SchemaVersion: registry.SchemaVersion, Entries: []registry.IndexEntry{{
		Repository: repository, Path: "widget.json", SHA256: registry.SHA256(data),
	}}}
	cache, err := registry.NewCache()
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(source, index, map[string][]byte{"widget.json": data}); err != nil {
		t.Fatal(err)
	}
}

func sourceNamespaceForCommandTest(source registry.Source) string {
	digest := sha256.Sum256([]byte(source.String()))
	return hex.EncodeToString(digest[:])
}

func jsonEncode(w http.ResponseWriter, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func jsonUnmarshal(data []byte, value any) error {
	return json.Unmarshal(data, value)
}

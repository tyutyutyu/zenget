package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/github"
	"zenget/internal/recipepolicy"
	"zenget/internal/registry"
)

// setupAutoRegistryEnv isolates every automatic-registry test from the real
// home, config, state, and cache directories and returns the fake home.
func setupAutoRegistryEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return home
}

// autoRegistryReleaseServer serves one release whose only asset is a
// widget.tar.gz archive containing one executable named "widget".
func autoRegistryReleaseServer(t *testing.T) *httptest.Server {
	t.Helper()
	binary := []byte("#!/bin/sh\necho widget\n")
	asset := tarGzAsset(t, "widget", binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = jsonEncode(w, map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{{
					"name":                 "widget.tar.gz",
					"browser_download_url": "http://" + r.Host + "/assets/widget.tar.gz",
				}},
			})
		case "/assets/widget.tar.gz":
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func runPlainAutoInstall(t *testing.T, server *httptest.Server, options installOptions) (string, error) {
	t.Helper()
	if options.goos == "" {
		options.goos = "linux"
	}
	if options.goarch == "" {
		options.goarch = "amd64"
	}
	if options.input == nil {
		options.input = strings.NewReader("")
	}
	client := github.New()
	client.BaseURL = server.URL
	var output strings.Builder
	err := runInstallWithOptions(context.Background(), client, "acme/widget", options, &output)
	return output.String(), err
}

func TestInstallResolvesRegistryRecipeWithoutFlags(t *testing.T) {
	home := setupAutoRegistryEnv(t)
	seedRegistrySnapshot(t, "stable", "acme/widget", "registry-widget")
	server := autoRegistryReleaseServer(t)

	output, err := runPlainAutoInstall(t, server, installOptions{})
	if err != nil {
		t.Fatalf("automatic registry install error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "registry-widget")); err != nil {
		t.Fatalf("registry-selected target missing: %v", err)
	}
	if !strings.Contains(output, "Using recipe for acme/widget from registry stable (github:acme/catalog@"+commandRegistryCommit+":recipes)") {
		t.Fatalf("output = %q", output)
	}
}

func TestInstallFallsBackToHeuristicsWhenRegistryMisses(t *testing.T) {
	home := setupAutoRegistryEnv(t)
	seedRegistrySnapshot(t, "stable", "acme/other", "other")
	server := autoRegistryReleaseServer(t)

	output, err := runPlainAutoInstall(t, server, installOptions{})
	if err != nil {
		t.Fatalf("heuristic fallback install error = %v", err)
	}
	if !strings.Contains(output, "Installed widget v1.0.0 to ") {
		t.Fatalf("output = %q", output)
	}
	if strings.Contains(output, "Using recipe for") {
		t.Fatalf("fallback output mentions a registry recipe: %q", output)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); err != nil {
		t.Fatalf("heuristic target missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "other")); !os.IsNotExist(err) {
		t.Fatalf("registry-only target exists after fallback: %v", err)
	}

	matched, name, _, reasons := resolveAutomaticRegistryRecipe("acme/widget")
	if matched != nil || name != "" {
		t.Fatalf("resolveAutomaticRegistryRecipe() = %#v, %q; want nil, empty", matched, name)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "no recipe") {
		t.Fatalf("skip reasons = %v, want one no-recipe reason", reasons)
	}
}

func TestInstallFallsBackToHeuristicsWithoutRegistries(t *testing.T) {
	home := setupAutoRegistryEnv(t)
	server := autoRegistryReleaseServer(t)

	output, err := runPlainAutoInstall(t, server, installOptions{})
	if err != nil {
		t.Fatalf("no-registry install error = %v", err)
	}
	if !strings.Contains(output, "Installed widget v1.0.0 to ") {
		t.Fatalf("output = %q", output)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); err != nil {
		t.Fatalf("heuristic target missing: %v", err)
	}

	matched, _, _, reasons := resolveAutomaticRegistryRecipe("acme/widget")
	if matched != nil || len(reasons) != 0 {
		t.Fatalf("resolveAutomaticRegistryRecipe() = %#v, %v; want nil, no reasons", matched, reasons)
	}
}

func TestInstallFallsBackToHeuristicsWhenPolicyDenies(t *testing.T) {
	home := setupAutoRegistryEnv(t)
	seedRegistrySnapshot(t, "stable", "acme/widget", "registry-widget")
	if err := recipepolicy.Save(recipepolicy.Default()); err != nil {
		t.Fatal(err)
	}
	server := autoRegistryReleaseServer(t)

	output, err := runPlainAutoInstall(t, server, installOptions{})
	if err != nil {
		t.Fatalf("policy-denied fallback install error = %v", err)
	}
	if !strings.Contains(output, "Installed widget v1.0.0 to ") {
		t.Fatalf("output = %q", output)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); err != nil {
		t.Fatalf("heuristic target missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "registry-widget")); !os.IsNotExist(err) {
		t.Fatalf("denied registry target exists: %v", err)
	}

	matched, _, _, reasons := resolveAutomaticRegistryRecipe("acme/widget")
	if matched != nil {
		t.Fatalf("resolveAutomaticRegistryRecipe() = %#v; want nil", matched)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "policy denied") {
		t.Fatalf("skip reasons = %v, want one policy-denied reason", reasons)
	}
}

func TestInstallNoRegistryFlagSkipsAutomaticResolution(t *testing.T) {
	home := setupAutoRegistryEnv(t)
	seedRegistrySnapshot(t, "stable", "acme/widget", "registry-widget")
	server := autoRegistryReleaseServer(t)

	output, err := runPlainAutoInstall(t, server, installOptions{noRegistry: true})
	if err != nil {
		t.Fatalf("--no-registry install error = %v", err)
	}
	if strings.Contains(output, "Using recipe for") {
		t.Fatalf("--no-registry output mentions a registry recipe: %q", output)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); err != nil {
		t.Fatalf("heuristic target missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "registry-widget")); !os.IsNotExist(err) {
		t.Fatalf("registry target exists with --no-registry: %v", err)
	}
}

func TestInstallExplicitRegistryMissStillFailsClosed(t *testing.T) {
	setupAutoRegistryEnv(t)
	seedRegistrySnapshot(t, "stable", "acme/other", "other")
	server := autoRegistryReleaseServer(t)

	_, err := runPlainAutoInstall(t, server, installOptions{registryName: "stable", registryNameSet: true})
	if err == nil || !strings.Contains(err.Error(), `registry snapshot has no recipe for repository: "acme/widget"`) {
		t.Fatalf("explicit registry miss error = %v", err)
	}
}

func TestResolveAutomaticRegistryRecipeReportsUnavailableConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configPath := filepath.Join(configHome, "zenget", registry.ConfigFileName)
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}

	matched, _, _, reasons := resolveAutomaticRegistryRecipe("acme/widget")
	if matched != nil {
		t.Fatalf("resolveAutomaticRegistryRecipe() = %#v; want nil", matched)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "registry config unavailable") {
		t.Fatalf("skip reasons = %v, want one config-unavailable reason", reasons)
	}
}

func TestInstallFallsBackToHeuristicsWithUnsyncedRegistry(t *testing.T) {
	home := setupAutoRegistryEnv(t)
	// A configured registry that was never synced: Lookup reports the missing
	// snapshot and resolution silently continues to heuristics.
	config := registry.DefaultConfig()
	if _, err := config.Add("pending", "github:acme/catalog@"+commandRegistryCommit+":recipes"); err != nil {
		t.Fatal(err)
	}
	if err := registry.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := recipepolicy.Save(recipepolicy.Policy{SchemaVersion: recipepolicy.SchemaVersion, Allow: []recipepolicy.Rule{{
		Host: recipepolicy.GitHubHost, Owner: "acme", Repository: "catalog", PathPrefix: "recipes",
	}}}); err != nil {
		t.Fatal(err)
	}
	server := autoRegistryReleaseServer(t)

	matched, _, _, reasons := resolveAutomaticRegistryRecipe("acme/widget")
	if matched != nil {
		t.Fatalf("resolveAutomaticRegistryRecipe() = %#v; want nil", matched)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], registry.ErrSnapshotNotFound.Error()) {
		t.Fatalf("skip reasons = %v, want one not-synchronized reason", reasons)
	}

	output, err := runPlainAutoInstall(t, server, installOptions{})
	if err != nil {
		t.Fatalf("unsynced-registry fallback install error = %v", err)
	}
	if !strings.Contains(output, "Installed widget v1.0.0 to ") {
		t.Fatalf("output = %q", output)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); err != nil {
		t.Fatalf("heuristic target missing: %v", err)
	}
}

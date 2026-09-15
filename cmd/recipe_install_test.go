package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"zenget/internal/appcfg"
	"zenget/internal/assetselector"
	"zenget/internal/github"
	"zenget/internal/recipe"
	"zenget/internal/state"
)

func TestInstallRecipeAppliesArchiveTargetAndChecksumWithoutPersistingRecipe(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binary := []byte("#!/bin/sh\necho recipe\n")
	assetData := tarGzAsset(t, "bin/widget", binary)
	digest := sha256.Sum256(assetData)
	digestText := hex.EncodeToString(digest[:])
	assetName := "widget-linux-amd64-v1.tar.gz"
	checksumName := assetName + ".sha256"
	var assetDownloads, checksumDownloads atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]any{
					{"id": 1, "name": assetName, "browser_download_url": server.URL + "/asset"},
					{"id": 2, "name": "widget-linux-arm64-v1.tar.gz", "browser_download_url": server.URL + "/arm64"},
					{"id": 3, "name": checksumName, "browser_download_url": server.URL + "/checksum"},
				},
			})
		case "/asset":
			assetDownloads.Add(1)
			_, _ = w.Write(assetData)
		case "/checksum":
			checksumDownloads.Add(1)
			_, _ = w.Write([]byte(digestText + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	selector := assetselector.NewRegex(`^widget-linux-amd64-v[0-9]+\.tar\.gz$`)
	installRecipe := recipe.Recipe{
		SchemaVersion:    recipe.SchemaVersion,
		Repository:       "acme/widget",
		SupportedSystems: []string{"linux/amd64"},
		AssetSelector:    &selector,
		ArchiveBinary:    "bin/widget",
		TargetName:       "widget-recipe",
		Checksum: &recipe.Checksum{
			Type:    recipe.ChecksumTypeExact,
			Pattern: checksumName,
			Format:  recipe.ChecksumFormatRaw,
		},
	}
	recipePath := filepath.Join(t.TempDir(), "widget-recipe.json")
	recipeData, err := recipe.Marshal(installRecipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recipePath, recipeData, 0644); err != nil {
		t.Fatal(err)
	}

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		recipePath:    recipePath,
		recipePathSet: true,
		goos:          "linux",
		goarch:        "amd64",
		input:         strings.NewReader(""),
	}, &output); err != nil {
		t.Fatalf("recipe install: %v", err)
	}
	if assetDownloads.Load() != 1 || checksumDownloads.Load() != 1 {
		t.Fatalf("downloads = asset %d, checksum %d; want 1, 1", assetDownloads.Load(), checksumDownloads.Load())
	}
	if !strings.Contains(output.String(), "Verified SHA-256 checksum for "+assetName+" from "+checksumName) {
		t.Fatalf("output = %q", output.String())
	}

	installed, err := os.ReadFile(filepath.Join(home, ".local", "bin", "widget-recipe"))
	if err != nil {
		t.Fatalf("read wrapper: %v", err)
	}
	if !bytes.Contains(installed, []byte("widget-recipe")) {
		t.Fatalf("wrapper = %q", installed)
	}
	realPath := installedRealPath(t, "acme/widget")
	gotBinary, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if !bytes.Equal(gotBinary, binary) {
		t.Fatalf("installed binary = %q, want %q", gotBinary, binary)
	}

	config, err := appcfg.Load("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if config.Asset != assetName || config.AssetSelector != nil || config.ArchiveBinary != "bin/widget" || config.TargetName != "widget-recipe" {
		t.Fatalf("saved app config = %#v", config)
	}
	rawConfig, err := os.ReadFile(filepathForSelectorTest(t, "acme/widget"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawConfig), recipePath) || strings.Contains(string(rawConfig), "supported_systems") || strings.Contains(string(rawConfig), "checksum") {
		t.Fatalf("saved app config contains recipe data: %s", rawConfig)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if app, ok := st.Get("acme/widget"); !ok || strings.Contains(app.DownloadURL, recipePath) {
		t.Fatalf("saved state = %#v, exists = %v", app, ok)
	}
}

func TestRecipeSelectionPrecedence(t *testing.T) {
	release := &github.Release{TagName: "v1.0.0", Assets: []github.Asset{
		{Name: "explicit-linux-amd64.tar.gz"},
		{Name: "saved-linux-amd64.tar.gz"},
		{Name: "recipe-linux-amd64.tar.gz"},
		{Name: "automatic-linux-amd64.tar.gz"},
	}}
	recipeSelector := assetselector.NewSubstring("recipe-linux-amd64")
	explicitSelector := assetselector.NewSubstring("explicit-linux-amd64")
	tests := []struct {
		name          string
		explicit      string
		explicitMatch *appcfg.AssetSelector
		saved         string
		recipe        *recipe.Recipe
		want          string
	}{
		{name: "explicit asset", explicit: "explicit-linux-amd64.tar.gz", saved: "saved-linux-amd64.tar.gz", recipe: &recipe.Recipe{AssetSelector: &recipeSelector}, want: "explicit-linux-amd64.tar.gz"},
		{name: "explicit selector", explicitMatch: &explicitSelector, saved: "saved-linux-amd64.tar.gz", recipe: &recipe.Recipe{AssetSelector: &recipeSelector}, want: "explicit-linux-amd64.tar.gz"},
		{name: "saved asset", saved: "saved-linux-amd64.tar.gz", recipe: &recipe.Recipe{AssetSelector: &recipeSelector}, want: "saved-linux-amd64.tar.gz"},
		{name: "recipe selector", recipe: &recipe.Recipe{AssetSelector: &recipeSelector}, want: "recipe-linux-amd64.tar.gz"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _, _, err := resolveInstallAssetWithSelectors(release, "linux", "amd64", test.explicit, test.explicitMatch, test.saved, nil, test.recipe, false, nil)
			if err != nil {
				t.Fatalf("resolveInstallAssetWithSelectors() error = %v", err)
			}
			if got.Name != test.want {
				t.Fatalf("selected asset = %q, want %q", got.Name, test.want)
			}
		})
	}
}

func TestRecipeAssetSelectorFailsClosedWithoutAutomaticFallback(t *testing.T) {
	release := &github.Release{TagName: "v1.0.0", Assets: []github.Asset{
		{Name: "widget-linux-amd64.tar.gz"},
	}}
	selector := assetselector.NewSubstring("does-not-exist")
	_, _, _, err := resolveInstallAssetWithSelectors(release, "linux", "amd64", "", nil, "", nil, &recipe.Recipe{AssetSelector: &selector}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "no assets matching") {
		t.Fatalf("recipe selector error = %v", err)
	}
}

func TestRecipeChecksumAssetMustResolveExactlyOneReleaseAsset(t *testing.T) {
	release := &github.Release{TagName: "v1.0.0", Assets: []github.Asset{
		{Name: "widget.tar.gz"},
		{Name: "checksums-v1.txt"},
		{Name: "checksums-v2.txt"},
	}}
	tests := []struct {
		name     string
		checksum recipe.Checksum
		want     string
		contains string
	}{
		{name: "exact", checksum: recipe.Checksum{Type: recipe.ChecksumTypeExact, Pattern: "checksums-v1.txt", Format: recipe.ChecksumFormatRaw}, want: "checksums-v1.txt"},
		{name: "regex", checksum: recipe.Checksum{Type: recipe.ChecksumTypeRegex, Pattern: `^checksums-v1\.txt$`, Format: recipe.ChecksumFormatRaw}, want: "checksums-v1.txt"},
		{name: "none", checksum: recipe.Checksum{Type: recipe.ChecksumTypeExact, Pattern: "missing.txt", Format: recipe.ChecksumFormatRaw}, contains: "matched no assets"},
		{name: "multiple", checksum: recipe.Checksum{Type: recipe.ChecksumTypeRegex, Pattern: `^checksums-v[12]\.txt$`, Format: recipe.ChecksumFormatRaw}, contains: "matched multiple"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveRecipeChecksumAsset(release, test.checksum)
			if test.want != "" {
				if err != nil || got.Name != test.want {
					t.Fatalf("resolveRecipeChecksumAsset() = %#v, %v; want %q", got, err, test.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("resolveRecipeChecksumAsset() error = %v, want %q", err, test.contains)
			}
		})
	}
}

func TestRecipeChecksumMismatchLeavesInstallUntouched(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	assetName := "widget-linux-amd64"
	assetData := []byte("recipe binary")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{
					{"name": assetName, "browser_download_url": "http://" + r.Host + "/asset"},
					{"name": "checksums.txt", "browser_download_url": "http://" + r.Host + "/checksum"},
				},
			})
		case "/asset":
			_, _ = w.Write(assetData)
		case "/checksum":
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	selector := assetselector.NewSubstring("linux-amd64")
	installRecipe := recipe.Recipe{
		SchemaVersion: recipe.SchemaVersion,
		Repository:    "acme/widget",
		AssetSelector: &selector,
		Checksum: &recipe.Checksum{
			Type:    recipe.ChecksumTypeExact,
			Pattern: "checksums.txt",
			Format:  recipe.ChecksumFormatRaw,
		},
	}
	client := github.New()
	client.BaseURL = server.URL
	err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		installRecipe: &installRecipe,
		goos:          "linux",
		goarch:        "amd64",
		input:         strings.NewReader(""),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("recipe checksum error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); !os.IsNotExist(err) {
		t.Fatalf("wrapper exists after checksum mismatch: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("acme/widget"); ok {
		t.Fatal("state contains app after checksum mismatch")
	}
	configPath, err := appcfg.Path("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("app config exists after checksum mismatch: %v", err)
	}
}

func TestRecipePreflightRejectsBeforeClientOrState(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	recipeFile := filepath.Join(t.TempDir(), "bad-recipe.json")
	data := []byte(`{"schema_version":1,"repository":"other/tool"}`)
	if err := os.WriteFile(recipeFile, data, 0644); err != nil {
		t.Fatal(err)
	}
	err := runInstallWithOptions(context.Background(), nil, "acme/widget", installOptions{
		recipePath:    recipeFile,
		recipePathSet: true,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), `recipe repository "other/tool" does not match command repository "acme/widget"`) {
		t.Fatalf("preflight error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "widget")); !os.IsNotExist(err) {
		t.Fatalf("preflight created wrapper: %v", err)
	}
	if _, err := appcfg.Load("acme/widget"); err != nil {
		t.Fatalf("load app config after preflight: %v", err)
	}
}

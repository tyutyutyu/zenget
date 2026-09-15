package appcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathAndRoundTrip(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	want := &AppConfig{Asset: "tool-linux-amd64.tar.gz", ArchiveBinary: "bin/tool", TargetName: "tool-custom"}
	if err := want.Save("acme/widget"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path, err := Path("acme/widget")
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if got, wantPath := path, filepath.Join(configHome, "zenget", "apps", "acme__widget.json"); got != wantPath {
		t.Fatalf("Path() = %q, want %q", got, wantPath)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("app config mode = %o, want 0644", info.Mode().Perm())
	}

	got, err := Load("acme/widget")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if *got != *want {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("app config directory entries = %v, want only %q", entries, filepath.Base(path))
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got, err := Load("acme/widget")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if *got != (AppConfig{}) {
		t.Fatalf("Load() = %#v, want empty config", got)
	}
}

func TestSelectorConfigRoundTripAndLegacyCompatibility(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	want := &AppConfig{
		AssetSelector: &AssetSelector{Version: 1, Type: AssetSelectorRegex, Pattern: `^tool-linux-amd64-v[0-9]+\.tar\.gz$`},
		ArchiveBinary: "bin/tool",
		TargetName:    "tool",
	}
	if err := want.Save("acme/widget"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load("acme/widget")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.AssetSelector == nil || *got.AssetSelector != *want.AssetSelector || got.ArchiveBinary != want.ArchiveBinary || got.TargetName != want.TargetName {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}

	legacy := []byte(`{"asset":"tool-linux-amd64-v1.tar.gz"}`)
	path, err := Path("acme/legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacy, 0644); err != nil {
		t.Fatal(err)
	}
	loadedLegacy, err := Load("acme/legacy")
	if err != nil {
		t.Fatalf("Load() legacy error = %v", err)
	}
	if loadedLegacy.Asset != "tool-linux-amd64-v1.tar.gz" || loadedLegacy.AssetSelector != nil {
		t.Fatalf("legacy config = %#v", loadedLegacy)
	}
}

func TestInvalidSelectorConfigIsRejectedBeforeUse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := Path("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"asset_selector": map[string]any{"version": 1, "type": "regex", "pattern": "["},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("acme/widget"); err == nil || !strings.Contains(err.Error(), "invalid asset selector regex") {
		t.Fatalf("Load() error = %v, want invalid selector regex", err)
	}
	if err := (&AppConfig{AssetSelector: &AssetSelector{Version: 1, Type: AssetSelectorRegex, Pattern: "["}}).Save("acme/other"); err == nil || !strings.Contains(err.Error(), "invalid asset selector regex") {
		t.Fatalf("Save() error = %v, want invalid selector regex", err)
	}
}

func TestInvalidRepositoryAndCorruptFile(t *testing.T) {
	for _, repository := range []string{"widget", "../widget", "acme/widget/extra", "acme/"} {
		if _, err := Path(repository); err == nil {
			t.Errorf("Path(%q) error = nil, want validation error", repository)
		}
	}

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path, err := Path("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("acme/widget"); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("Load() error = %v, want path-bearing parse error", err)
	}
}

func TestPathFallbackAndNilSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	path, err := Path("acme/widget")
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if want := filepath.Join(home, ".config", "zenget", "apps", "acme__widget.json"); path != want {
		t.Fatalf("Path() = %q, want %q", path, want)
	}

	var cfg *AppConfig
	if err := cfg.Save("acme/widget"); err == nil {
		t.Fatal("nil Save() error = nil, want error")
	}
}

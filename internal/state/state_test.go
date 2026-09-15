package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileReturnsEmptyState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("Load() returned nil state")
	}
	if loaded.Apps == nil {
		t.Fatal("Load() returned nil Apps map")
	}
	if len(loaded.Apps) != 0 {
		t.Fatalf("Load() returned %d apps, want 0", len(loaded.Apps))
	}
}

func TestStateSaveLoadRoundTrip(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	installedAt := time.Date(2026, time.August, 19, 12, 34, 56, 0, time.UTC)
	want := App{
		Version:     "v1.2.3",
		InstalledAt: installedAt,
		DownloadURL: "https://github.com/org/repo/releases/download/v1.2.3/repo",
		InstallPath: "/home/user/.local/bin/repo",
		XXH3_64:     "0123456789abcdef",
		Pinned:      true,
	}

	original := &State{}
	original.Set("org/repo", want)
	if err := original.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Errorf("state file mode = %04o, want 0644", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", filepath.Dir(path), err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("state directory entries = %v, want only %q", entries, filepath.Base(path))
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, ok := loaded.Get("org/repo")
	if !ok {
		t.Fatal("Load() did not return saved application")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loaded app = %#v, want %#v", got, want)
	}
}

func TestStateSaveLoadRoundTripWithVersionedArtifacts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	want := App{
		Version:     "v2.0.0",
		RealPath:    "/data/zenget/bin/github/acme/repo/v2.0.0/repo",
		XXH3_64:     "active-hash",
		InstallPath: "/home/user/.local/bin/repo",
		ActiveArtifact: &Artifact{
			Version:  "v2.0.0",
			RealPath: "/data/zenget/bin/github/acme/repo/v2.0.0/repo",
			XXH3_64:  "active-hash",
		},
		PreviousArtifact: &Artifact{
			Version:  "v1.0.0",
			RealPath: "/data/zenget/bin/github/acme/repo/v1.0.0/repo",
			XXH3_64:  "previous-hash",
		},
	}

	original := &State{}
	original.Set("acme/repo", want)
	if err := original.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, ok := loaded.Get("acme/repo")
	if !ok {
		t.Fatal("Load() did not return saved application")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loaded app = %#v, want %#v", got, want)
	}
}

func TestAppActiveFallsBackToLegacyFieldsAndClones(t *testing.T) {
	app := App{Version: "v1.0.0", RealPath: "/data/zenget/bin/repo", XXH3_64: "hash"}
	active := app.Active()
	if active == nil || active.Version != app.Version || active.RealPath != app.RealPath || active.XXH3_64 != app.XXH3_64 {
		t.Fatalf("Active() = %#v, want legacy metadata", active)
	}
	active.RealPath = "/changed"
	if app.RealPath == active.RealPath {
		t.Fatal("Active() returned aliases to legacy application fields")
	}

	app.PreviousArtifact = &Artifact{Version: "v0.9.0", RealPath: "/data/old", XXH3_64: "old-hash"}
	artifacts := app.ArtifactPaths()
	if len(artifacts) != 2 {
		t.Fatalf("ArtifactPaths() returned %d artifacts, want 2", len(artifacts))
	}
	artifacts[1].RealPath = "/changed"
	if app.PreviousArtifact.RealPath == artifacts[1].RealPath {
		t.Fatal("ArtifactPaths() returned aliases to previous metadata")
	}
}

func TestLoadLegacyStateDefaultsPinnedToFalse(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	legacy := `{"apps":{"org/repo":{"version":"v1.0.0","install_path":"/home/user/.local/bin/repo"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	app, ok := loaded.Get("org/repo")
	if !ok {
		t.Fatal("Load() did not return legacy application")
	}
	if app.Pinned {
		t.Fatal("legacy application is pinned, want false")
	}
}

func TestStateDelete(t *testing.T) {
	state := &State{}
	state.Set("org/repo", App{Version: "v1.0.0"})

	state.Delete("org/repo")
	if _, ok := state.Get("org/repo"); ok {
		t.Fatal("Get() found application after Delete()")
	}
}

func TestLoadCorruptedJSONReturnsError(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err = Load()
	if err == nil {
		t.Fatal("Load() error = nil, want corrupted JSON error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("Load() error = %q, want path %q", err, path)
	}
}

func TestPathHonorsXDGConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	want := filepath.Join(configHome, "zenget", "state.json")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestPathFallsBackToUserConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	want := filepath.Join(home, ".config", "zenget", "state.json")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestLoadNullAppsInitializesEmptyMap(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"apps": null}`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Apps == nil {
		t.Fatal("Load() returned nil Apps map")
	}
	if len(loaded.Apps) != 0 {
		t.Fatalf("Load() returned %d apps, want 0", len(loaded.Apps))
	}
}

func TestLoadUnreadableFileReturnsError(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	// A directory at the state file path makes ReadFile fail with an error
	// other than "file does not exist".
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	_, err = Load()
	if err == nil {
		t.Fatal("Load() error = nil, want read error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("Load() error = %q, want path %q", err, path)
	}
}

func TestSaveMkdirAllError(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	// A regular file where the state directory should be makes MkdirAll fail.
	if err := os.WriteFile(filepath.Join(configHome, "zenget"), []byte("file"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	state := &State{}
	state.Set("org/repo", App{Version: "v1.0.0"})

	err := state.Save()
	if err == nil {
		t.Fatal("Save() error = nil, want create directory error")
	}
	if !strings.Contains(err.Error(), "create state directory") {
		t.Errorf("Save() error = %q, want create directory error", err)
	}
}

func TestSaveCreateTempError(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	// A read-only state directory makes CreateTemp fail.
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	defer func() {
		if err := os.Chmod(dir, 0755); err != nil {
			t.Errorf("restore Chmod() error = %v", err)
		}
	}()

	state := &State{}
	state.Set("org/repo", App{Version: "v1.0.0"})

	err = state.Save()
	if err == nil {
		t.Fatal("Save() error = nil, want create temporary file error")
	}
	if !strings.Contains(err.Error(), "create temporary state file") {
		t.Errorf("Save() error = %q, want create temporary file error", err)
	}
}

func TestSaveRenameError(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	// A directory at the state file path makes the final rename fail.
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	state := &State{}
	state.Set("org/repo", App{Version: "v1.0.0"})

	err = state.Save()
	if err == nil {
		t.Fatal("Save() error = nil, want rename error")
	}
	if !strings.Contains(err.Error(), "replace state file") {
		t.Errorf("Save() error = %q, want rename error", err)
	}

	// The temporary file must be cleaned up after the failed rename.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", filepath.Dir(path), err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Errorf("state directory entries = %v, want only %q", entries, filepath.Base(path))
	}
}

func TestGetNilReceiver(t *testing.T) {
	var state *State

	app, ok := state.Get("org/repo")
	if ok {
		t.Fatal("Get() ok = true on nil state, want false")
	}
	if !reflect.DeepEqual(app, App{}) {
		t.Errorf("Get() app = %#v, want zero App", app)
	}
}

func TestDeleteNilReceiver(t *testing.T) {
	var state *State
	state.Delete("org/repo")
}

func TestDeleteMissingKey(t *testing.T) {
	state := &State{}
	state.Set("org/repo", App{Version: "v1.0.0"})

	state.Delete("other/repo")
	if _, ok := state.Get("org/repo"); !ok {
		t.Fatal("Get() did not find application after Delete() of a different key")
	}
}

func TestPathWithoutHomeReturnsError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	_, err := Path()
	if err == nil {
		t.Fatal("Path() error = nil, want user home directory error")
	}
	if !strings.Contains(err.Error(), "find user home directory") {
		t.Errorf("Path() error = %q, want user home directory error", err)
	}
}

func TestLoadWithoutHomeReturnsError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want user home directory error")
	}
}

func TestSaveWithoutHomeReturnsError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	state := &State{}
	state.Set("org/repo", App{Version: "v1.0.0"})

	if err := state.Save(); err == nil {
		t.Fatal("Save() error = nil, want user home directory error")
	}
}

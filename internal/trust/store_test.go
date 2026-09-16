package trust

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"zenget/internal/fileowner"
)

func TestPathHonorsXDGConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configHome, "zenget", "project-trust.json")
	if got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestPathUsesHomeWhenXDGConfigHomeIsUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if runtime.GOOS == "windows" {
		resolvedHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		home = resolvedHome
	}

	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "zenget", "project-trust.json")
	if got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestProjectRootForCanonicalizesAndRejectsInvalidPaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "..", "zenget.json")
	got, err := ProjectRootFor(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("ProjectRootFor() = %q, want %q", got, root)
	}
	for _, invalid := range []string{"", "   ", "bad\x00path"} {
		if _, err := ProjectRootFor(invalid); err == nil {
			t.Fatalf("ProjectRootFor(%q) error = nil", invalid)
		}
	}
}

func TestAddSaveLoadAndRemoveTrustRecord(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	project := t.TempDir()
	if err := os.Chmod(project, 0755); err != nil {
		t.Fatal(err)
	}
	secureTestPath(t, project, true)
	manifestPath := filepath.Join(project, "zenget.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"apps":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	secureTestPath(t, manifestPath, false)

	store := New()
	record, changed, err := store.Add(manifestPath)
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if !changed || record.ManifestPath != manifestPath || record.ProjectRoot != project {
		t.Fatalf("Add() = %#v, changed=%t", record, changed)
	}
	if _, changed, err := store.Add(manifestPath); err != nil || changed {
		t.Fatalf("duplicate Add() = changed %t, err %v; want unchanged", changed, err)
	}
	if err := Save(store); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		got := info.Mode().Perm()
		t.Fatalf("trust store mode = %04o, want 0600", got)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	found, ok, err := loaded.Find(manifestPath)
	if err != nil || !ok || found != record {
		t.Fatalf("Find() = %#v, %t, %v; want %#v, true, nil", found, ok, err, record)
	}
	if removed, err := loaded.Remove(manifestPath); err != nil || !removed {
		t.Fatalf("Remove() = %t, %v; want true, nil", removed, err)
	}
	if err := Save(loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Projects) != 0 {
		t.Fatalf("loaded projects after removal = %#v, want empty", loaded.Projects)
	}
}

func TestLoadMissingReturnsEmptyCurrentStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if store.SchemaVersion != SchemaVersion || store.Projects == nil || len(store.Projects) != 0 {
		t.Fatalf("Load() = %#v, want empty current store", store)
	}
}

func TestCheckPathSafetyRejectsUnsafeManifestAndDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode-bit rejection is covered on Unix; Windows ACL rejection has dedicated tests")
	}
	project := t.TempDir()
	if err := os.Chmod(project, 0755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(project, "nested")
	if err := os.Mkdir(nested, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(nested, "zenget.json")
	if err := os.WriteFile(manifestPath, []byte("manifest"), 0644); err != nil {
		t.Fatal(err)
	}
	record := Record{ManifestPath: manifestPath, ProjectRoot: project}
	if err := CheckPathSafety(record); err != nil {
		t.Fatalf("CheckPathSafety() error = %v", err)
	}

	if err := os.Chmod(nested, 0775); err != nil {
		t.Fatal(err)
	}
	if err := CheckPathSafety(record); err == nil || !strings.Contains(err.Error(), "group/world writable") {
		t.Fatalf("unsafe directory error = %v, want group/world writable", err)
	}
	if err := os.Chmod(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(manifestPath, 0664); err != nil {
		t.Fatal(err)
	}
	if err := CheckPathSafety(record); err == nil || !strings.Contains(err.Error(), "group/world writable") {
		t.Fatalf("unsafe manifest error = %v, want group/world writable", err)
	}
}

func TestCheckPathSafetyRejectsSymlinkAndInvalidRoot(t *testing.T) {
	project := t.TempDir()
	manifestPath := filepath.Join(project, "zenget.json")
	if err := os.Chmod(project, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real-manifest")
	if err := os.WriteFile(target, []byte("manifest"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, manifestPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a Windows symlink is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := CheckPathSafety(Record{ManifestPath: manifestPath, ProjectRoot: project}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink manifest error = %v, want symlink rejection", err)
	}
	if err := CheckPathSafety(Record{ManifestPath: target, ProjectRoot: project}); err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("invalid root error = %v, want ancestor rejection", err)
	}
}

func TestAddRejectsMissingAndUnsafePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode-bit rejection is covered on Unix; Windows ACL rejection has dedicated tests")
	}
	store := New()
	if _, _, err := store.Add(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Add() missing manifest error = nil")
	}
	project := t.TempDir()
	manifestPath := filepath.Join(project, "zenget.json")
	if err := os.WriteFile(manifestPath, []byte("manifest"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(project, 0775); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Add(manifestPath); err == nil || !strings.Contains(err.Error(), "group/world writable") {
		t.Fatalf("Add() unsafe path error = %v, want permission error", err)
	}
}

func TestLoadRejectsMalformedOrUnsafeStore(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data string
		want string
	}{
		{"missing schema", `{"projects":[]}`, "missing schema_version"},
		{"unsupported schema", `{"schema_version":2,"projects":[]}`, "unsupported schema_version"},
		{"unknown field", `{"schema_version":1,"projects":[],"extra":true}`, "unknown field"},
		{"duplicate key", `{"schema_version":1,"schema_version":1,"projects":[]}`, "duplicate object key"},
		{"relative path", `{"schema_version":1,"projects":[{"manifest_path":"zenget.json","project_root":"."}]}`, "absolute"},
		{"missing projects", `{"schema_version":1}`, "missing projects"},
		{"null projects", `{"schema_version":1,"projects":null}`, "missing projects"},
		{"multiple values", `{"schema_version":1,"projects":[]} {}`, "multiple JSON values"},
		{"trailing data", `{"schema_version":1,"projects":[]} trailing`, "decode trust store"},
		{"non-object", `[]`, "cannot unmarshal array"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			secureTestPath(t, path, false)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadRejectsUnsafeStoreFileTypesAndModes(t *testing.T) {
	tests := []struct {
		name string
		make func(string) error
		want string
	}{
		{
			name: "symlink",
			make: func(path string) error {
				target := path + ".target"
				if err := os.WriteFile(target, []byte(`{"schema_version":1,"projects":[]}`), 0600); err != nil {
					return err
				}
				return os.Symlink(target, path)
			},
			want: "symlink",
		},
		{
			name: "directory",
			make: func(path string) error { return os.Mkdir(path, 0700) },
			want: "not a regular file",
		},
		{
			name: "unsafe mode",
			make: func(path string) error {
				if err := os.WriteFile(path, []byte(`{"schema_version":1,"projects":[]}`), 0664); err != nil {
					return err
				}
				return os.Chmod(path, 0664)
			},
			want: "group/world writable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if runtime.GOOS == "windows" && test.name == "unsafe mode" {
				t.Skip("POSIX mode-bit rejection is covered on Unix; Windows ACL rejection has dedicated tests")
			}
			path, err := Path()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := test.make(path); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("Windows filesystem does not provide this test fixture: %v", err)
				}
				t.Fatal(err)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestStoreRejectsInvalidReceiversAndRecords(t *testing.T) {
	var nilStore *Store
	if _, _, err := nilStore.Add("manifest.json"); err == nil || !strings.Contains(err.Error(), "store is nil") {
		t.Fatalf("nil Add() error = %v", err)
	}
	if _, err := nilStore.Remove("manifest.json"); err == nil || !strings.Contains(err.Error(), "store is nil") {
		t.Fatalf("nil Remove() error = %v", err)
	}

	store := New()
	if _, _, err := store.Find("\x00"); err == nil {
		t.Fatal("Find() invalid path error = nil")
	}
	if removed, err := store.Remove(filepath.Join(t.TempDir(), "not-trusted.json")); err != nil || removed {
		t.Fatalf("Remove() missing record = %t, %v", removed, err)
	}
	if _, _, err := store.Add(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Add() missing record error = nil")
	}

	if err := Save(Store{SchemaVersion: SchemaVersion + 1}); err == nil || !strings.Contains(err.Error(), "unsupported schema_version") {
		t.Fatalf("Save() future schema error = %v", err)
	}
	if err := Save(Store{SchemaVersion: SchemaVersion, Projects: []Record{{ManifestPath: "relative", ProjectRoot: "."}}}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Save() relative record error = %v", err)
	}
}

func TestSaveRejectsExistingUnsafeTargets(t *testing.T) {
	tests := []struct {
		name string
		make func(string) error
		want string
	}{
		{
			name: "symlink",
			make: func(path string) error {
				if err := os.WriteFile(path+".target", []byte("target"), 0600); err != nil {
					return err
				}
				return os.Symlink(path+".target", path)
			},
			want: "symlink",
		},
		{
			name: "directory",
			make: func(path string) error { return os.Mkdir(path, 0700) },
			want: "not a regular file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			path, err := Path()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := test.make(path); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("Windows filesystem does not provide this test fixture: %v", err)
				}
				t.Fatal(err)
			}
			if err := Save(New()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Save() error = %v, want %q", err, test.want)
			}
		})
	}
}

func secureTestPath(t *testing.T, path string, directory bool) {
	t.Helper()
	if err := fileowner.SecurePath(path, directory); err != nil {
		t.Fatalf("SecurePath(%q) error = %v", path, err)
	}
}

func TestSaveIsAtomicAndDoesNotLeaveTemporaryFile(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := Save(Store{}); err != nil {
		t.Fatalf("Save(empty) error = %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Store
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != SchemaVersion || decoded.Projects == nil {
		t.Fatalf("saved store = %#v", decoded)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".project-trust-") {
			t.Errorf("temporary trust file remains: %s", entry.Name())
		}
	}
	if bytes.Contains(data, []byte("manifest_path")) {
		t.Fatalf("empty trust store unexpectedly contains a record: %s", data)
	}
}

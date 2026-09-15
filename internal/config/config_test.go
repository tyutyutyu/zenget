package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zenget/internal/limits"
)

func TestLoadMissingFileReturnsDefaultConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
	if cfg.TrackingEnabled() {
		t.Fatal("Load() returned usage tracking enabled, want disabled")
	}
	if cfg.LazyInstall || cfg.LazyInstallEnabled() {
		t.Fatal("Load() returned lazy install enabled, want disabled")
	}
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	original := &Config{UsageTracking: true}
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
		t.Errorf("config file mode = %04o, want 0644", got)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, original) {
		t.Errorf("loaded config = %#v, want %#v", loaded, original)
	}
	if !loaded.TrackingEnabled() {
		t.Error("Load() returned usage tracking disabled, want enabled")
	}
}

func TestChecksumPolicyDefaultsAndRoundTrip(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var empty *Config
	if got := empty.EffectiveChecksumPolicy(); got != ChecksumPolicyIfPresent {
		t.Fatalf("nil EffectiveChecksumPolicy() = %q, want %q", got, ChecksumPolicyIfPresent)
	}
	cfg := &Config{}
	if got := cfg.EffectiveChecksumPolicy(); got != ChecksumPolicyIfPresent {
		t.Fatalf("empty EffectiveChecksumPolicy() = %q, want %q", got, ChecksumPolicyIfPresent)
	}
	if err := cfg.SetChecksumPolicy(" REQUIRED "); err != nil {
		t.Fatalf("SetChecksumPolicy() error = %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.ChecksumPolicy != ChecksumPolicyRequired {
		t.Fatalf("ChecksumPolicy = %q, want %q", loaded.ChecksumPolicy, ChecksumPolicyRequired)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"checksum_policy": "required"`) {
		t.Fatalf("saved config = %s, want checksum policy", data)
	}
}

func TestSetChecksumPolicyRejectsInvalidWithoutChangingValue(t *testing.T) {
	cfg := &Config{ChecksumPolicy: ChecksumPolicyRequired}
	if err := cfg.SetChecksumPolicy("off"); err == nil {
		t.Fatal("SetChecksumPolicy() error = nil, want invalid policy error")
	}
	if cfg.ChecksumPolicy != ChecksumPolicyRequired {
		t.Fatalf("ChecksumPolicy = %q after rejected value, want %q", cfg.ChecksumPolicy, ChecksumPolicyRequired)
	}
}

func TestLoadRejectsInvalidChecksumPolicy(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"checksum_policy":"off"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "checksum_policy") {
		t.Fatalf("Load() error = %v, want checksum policy validation error", err)
	}
}

func TestLoadMissingKeyDefaultsToFalse(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"other": true}`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TrackingEnabled() {
		t.Fatal("Load() returned usage tracking enabled, want disabled")
	}
}

func TestTrackingEnabledNilReceiver(t *testing.T) {
	var cfg *Config
	if cfg.TrackingEnabled() {
		t.Fatal("TrackingEnabled() = true on nil config, want false")
	}
}

func TestLazyInstallEnabledHonorsRuntimeOptOut(t *testing.T) {
	cfg := &Config{LazyInstall: true}
	t.Setenv("ZENGET_LAZY_INSTALL", "")
	if !cfg.LazyInstallEnabled() {
		t.Fatal("LazyInstallEnabled() = false, want true")
	}
	t.Setenv("ZENGET_LAZY_INSTALL", "0")
	if cfg.LazyInstallEnabled() {
		t.Fatal("LazyInstallEnabled() = true with runtime opt-out, want false")
	}
	var nilConfig *Config
	if nilConfig.LazyInstallEnabled() {
		t.Fatal("nil LazyInstallEnabled() = true, want false")
	}
}

func TestPathHonorsXDGConfigHome(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	want := filepath.Join(configHome, "zenget", "config.json")
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
	want := filepath.Join(home, ".config", "zenget", "config.json")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
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

func TestInstallDirectoryDefaultsToLocalBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := &Config{}
	got, err := cfg.InstallDirectory()
	if err != nil {
		t.Fatalf("InstallDirectory() error = %v", err)
	}
	want := filepath.Join(home, ".local", "bin")
	if got != want {
		t.Errorf("InstallDirectory() = %q, want %q", got, want)
	}
}

func TestInstallDirectoryExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := &Config{InstallDir: "~/bin"}
	got, err := cfg.InstallDirectory()
	if err != nil {
		t.Fatalf("InstallDirectory() error = %v", err)
	}
	want := filepath.Join(home, "bin")
	if got != want {
		t.Errorf("InstallDirectory() = %q, want %q", got, want)
	}
}

func TestInstallDirectoryPreservesAbsolutePath(t *testing.T) {
	cfg := &Config{InstallDir: "/opt/bin"}
	got, err := cfg.InstallDirectory()
	if err != nil {
		t.Fatalf("InstallDirectory() error = %v", err)
	}
	if got != "/opt/bin" {
		t.Errorf("InstallDirectory() = %q, want %q", got, "/opt/bin")
	}
}

func TestExpandInstallDirRejectsMissingHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	if _, err := ExpandInstallDir("~/bin"); err == nil {
		t.Fatal("ExpandInstallDir() error = nil, want error")
	}
}

func TestValidateInstallDir(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{"empty means default", "", false},
		{"tilde prefix", "~/bin", false},
		{"absolute", "/usr/local/bin", false},
		{"relative", "bin", true},
		{"dot relative", "./bin", true},
		{"parent relative", "../bin", true},
		{"bare tilde", "~", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateInstallDir(tt.dir)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateInstallDir(%q) error = %v, wantErr %v", tt.dir, err, tt.wantErr)
			}
		})
	}
}

func TestConfigSaveLoadRoundTripPreservesInstallDir(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	original := &Config{InstallDir: "~/tools"}
	if err := original.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.InstallDir != "~/tools" {
		t.Errorf("InstallDir = %q, want %q", loaded.InstallDir, "~/tools")
	}
}

func ExampleValidateInstallDir() {
	fmt.Println(ValidateInstallDir("/usr/local/bin"))
	fmt.Println(ValidateInstallDir("bin"))
	// Output:
	// <nil>
	// install_dir must be absolute or start with ~/
}

func TestEffectiveLimitsUseDefaultsAndPersistNumericOverrides(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	cfg := &Config{}
	defaults := limits.Defaults()
	if got := cfg.EffectiveLimits(); got != defaults {
		t.Fatalf("EffectiveLimits() = %#v, want %#v", got, defaults)
	}
	if err := cfg.SetLimit(limits.DownloadBytesKey, 2*limits.MiB); err != nil {
		t.Fatalf("SetLimit() error = %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"max_download_bytes": 2097152`) {
		t.Fatalf("saved config = %s, want decimal byte value", data)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, _ := loaded.Limit(limits.DownloadBytesKey); got != 2*limits.MiB {
		t.Errorf("loaded download limit = %d, want %d", got, 2*limits.MiB)
	}
}

func TestLoadRejectsInvalidStoredLimit(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"max_download_bytes":0}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("Load() error = %v, want positive-limit validation", err)
	}
}

func TestConfigSetLimitRejectsNegativeValues(t *testing.T) {
	cfg := &Config{}
	if err := cfg.SetLimit(limits.BinaryBytesKey, -1); err == nil {
		t.Fatal("SetLimit() error = nil, want negative-value error")
	}
	if err := (&Config{MaxBinaryBytes: -1}).Save(); err == nil {
		t.Fatal("Save() error = nil, want negative-value error")
	}
}

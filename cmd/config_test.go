package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/config"
	"zenget/internal/limits"
)

func TestPrintConfigShowsDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var output bytes.Buffer
	if err := printConfig(&output); err != nil {
		t.Fatalf("printConfig: %v", err)
	}
	want := "usage_tracking=false\nusage_upload=false\ninstall_dir=" + installDirDefault(t) + "\nchecksum_policy=if-present\nlazy_install=false\n" + defaultLimitsOutput()
	if got := output.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestPrintConfigShowsEnabled(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	var output bytes.Buffer
	if err := printConfig(&output); err != nil {
		t.Fatalf("printConfig: %v", err)
	}
	want := "usage_tracking=true\nusage_upload=true\ninstall_dir=" + installDirDefault(t) + "\nchecksum_policy=if-present\nlazy_install=false\n" + defaultLimitsOutput()
	if got := output.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestSetConfigValueAcceptsTrueAndFalse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var output bytes.Buffer
	if err := setConfigValue(&output, "usage_tracking", "true"); err != nil {
		t.Fatalf("setConfigValue true: %v", err)
	}
	if got, want := output.String(), "usage_tracking=true\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !cfg.TrackingEnabled() {
		t.Fatal("usage_tracking was not saved as true")
	}

	output.Reset()
	if err := setConfigValue(&output, "usage_tracking", "false"); err != nil {
		t.Fatalf("setConfigValue false: %v", err)
	}
	if got, want := output.String(), "usage_tracking=false\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.TrackingEnabled() {
		t.Fatal("usage_tracking was not saved as false")
	}
}

func TestSetConfigValueRejectsUnknownKey(t *testing.T) {
	var output bytes.Buffer
	err := setConfigValue(&output, "unknown_key", "true")
	if err == nil {
		t.Fatal("setConfigValue unknown key: expected error")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Errorf("error = %q, want unknown config key", err)
	}
}

func TestSetConfigValueAcceptsUploadToggle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var output bytes.Buffer
	if err := setConfigValue(&output, "usage_upload", "true"); err != nil {
		t.Fatalf("setConfigValue usage_upload: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !cfg.UsageUpload {
		t.Fatal("usage_upload was not saved as true")
	}

	output.Reset()
	if err := setConfigValue(&output, "usage_upload", "false"); err != nil {
		t.Fatalf("setConfigValue usage_upload false: %v", err)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.UsageUpload {
		t.Fatal("usage_upload was not saved as false")
	}
}

func TestSetConfigValueAcceptsLazyInstallToggle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var output bytes.Buffer
	if err := setConfigValue(&output, "lazy_install", "true"); err != nil {
		t.Fatalf("setConfigValue lazy_install: %v", err)
	}
	if got, want := output.String(), "lazy_install=true\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !cfg.LazyInstall || !cfg.LazyInstallEnabled() {
		t.Fatal("lazy_install was not saved as true")
	}

	output.Reset()
	t.Setenv("ZENGET_LAZY_INSTALL", "0")
	if err := getConfigValue(&output, "lazy_install"); err != nil {
		t.Fatalf("getConfigValue lazy_install: %v", err)
	}
	if got, want := output.String(), "lazy_install=true\n"; got != want {
		t.Fatalf("get output = %q, want %q", got, want)
	}
	if cfg.LazyInstallEnabled() {
		t.Fatal("runtime opt-out did not disable configured lazy install")
	}
}

func TestUploadEnabledRequiresTracking(t *testing.T) {
	cfg := &config.Config{UsageTracking: false, UsageUpload: true}
	if cfg.UploadEnabled() {
		t.Error("UploadEnabled() = true when tracking disabled, want false")
	}
	cfg.UsageTracking = true
	if !cfg.UploadEnabled() {
		t.Error("UploadEnabled() = false when both enabled, want true")
	}
}

func TestSetConfigValueRejectsInvalidValue(t *testing.T) {
	var output bytes.Buffer
	err := setConfigValue(&output, "usage_tracking", "maybe")
	if err == nil {
		t.Fatal("setConfigValue invalid value: expected error")
	}
	if !strings.Contains(err.Error(), "invalid value") {
		t.Errorf("error = %q, want invalid value", err)
	}
}

func TestConfigChecksumPolicySetGetAndList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var output bytes.Buffer
	if err := setConfigValue(&output, "checksum_policy", " REQUIRED "); err != nil {
		t.Fatalf("setConfigValue() error = %v", err)
	}
	if got, want := output.String(), "checksum_policy=required\n"; got != want {
		t.Fatalf("set output = %q, want %q", got, want)
	}

	output.Reset()
	if err := getConfigValue(&output, "checksum_policy"); err != nil {
		t.Fatalf("getConfigValue() error = %v", err)
	}
	if got, want := output.String(), "checksum_policy=required\n"; got != want {
		t.Fatalf("get output = %q, want %q", got, want)
	}

	output.Reset()
	if err := printConfig(&output); err != nil {
		t.Fatalf("printConfig() error = %v", err)
	}
	if !strings.Contains(output.String(), "checksum_policy=required\n") {
		t.Fatalf("list output = %q, want configured policy", output.String())
	}
}

func TestSetConfigValueRejectsInvalidChecksumPolicyWithoutChangingConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var output bytes.Buffer
	if err := setConfigValue(&output, "checksum_policy", "required"); err != nil {
		t.Fatalf("initial setConfigValue() error = %v", err)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := setConfigValue(&output, "checksum_policy", "off"); err == nil {
		t.Fatal("setConfigValue() error = nil, want invalid policy error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("config changed after invalid policy: %q", got)
	}
}

func TestSetConfigValueAcceptsInstallDir(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var output bytes.Buffer
	if err := setConfigValue(&output, "install_dir", "~/bin"); err != nil {
		t.Fatalf("setConfigValue install_dir: %v", err)
	}
	want := "install_dir=" + filepath.Join(home, "bin") + "\n"
	if got := output.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.InstallDir != "~/bin" {
		t.Fatalf("InstallDir = %q, want %q", cfg.InstallDir, "~/bin")
	}
}

func TestConfigLimitSetGetAndList(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var output bytes.Buffer
	if err := setConfigValue(&output, limits.DownloadBytesKey, "2MiB"); err != nil {
		t.Fatalf("setConfigValue() error = %v", err)
	}
	if got, want := output.String(), limits.DownloadBytesKey+"=2097152\n"; got != want {
		t.Fatalf("set output = %q, want %q", got, want)
	}

	output.Reset()
	if err := getConfigValue(&output, limits.DownloadBytesKey); err != nil {
		t.Fatalf("getConfigValue() error = %v", err)
	}
	if got, want := output.String(), limits.DownloadBytesKey+"=2097152\n"; got != want {
		t.Fatalf("get output = %q, want %q", got, want)
	}

	output.Reset()
	if err := printConfig(&output); err != nil {
		t.Fatalf("printConfig() error = %v", err)
	}
	if !strings.Contains(output.String(), limits.DownloadBytesKey+"=2097152\n") {
		t.Fatalf("list output = %q, want configured limit", output.String())
	}
}

func TestSetConfigValueRejectsInvalidLimitWithoutChangingConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	var output bytes.Buffer
	if err := setConfigValue(&output, limits.DownloadBytesKey, "1MiB"); err != nil {
		t.Fatalf("initial setConfigValue() error = %v", err)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0", "-1", "1KB", "unlimited", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			if err := setConfigValue(&output, limits.DownloadBytesKey, value); err == nil {
				t.Fatal("setConfigValue() error = nil, want invalid-limit error")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("config changed after invalid value %q: %q", value, got)
			}
		})
	}
}

func TestSetConfigValueRejectsRelativeInstallDir(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var output bytes.Buffer
	err := setConfigValue(&output, "install_dir", "bin")
	if err == nil {
		t.Fatal("setConfigValue relative install_dir: expected error")
	}
	if !strings.Contains(err.Error(), "install_dir must be absolute") {
		t.Errorf("error = %q, want install_dir must be absolute", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.InstallDir != "" {
		t.Errorf("InstallDir = %q after failed set, want empty", cfg.InstallDir)
	}
}

func TestPrintConfigShowsCustomInstallDir(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	cfg := &config.Config{InstallDir: "~/tools"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	var output bytes.Buffer
	if err := printConfig(&output); err != nil {
		t.Fatalf("printConfig: %v", err)
	}
	want := "usage_tracking=false\nusage_upload=false\ninstall_dir=" + filepath.Join(home, "tools") + "\nchecksum_policy=if-present\nlazy_install=false\n" + defaultLimitsOutput()
	if got := output.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// installDirDefault returns the expanded default install directory (~/.local/bin).
func installDirDefault(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	return filepath.Join(home, ".local", "bin")
}

func defaultLimitsOutput() string {
	defaults := limits.Defaults()
	var output string
	for _, key := range limits.Keys() {
		value, _ := defaults.Value(key)
		output += key + "=" + fmt.Sprint(value) + "\n"
	}
	return output
}

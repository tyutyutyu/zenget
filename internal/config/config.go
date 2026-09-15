// Package config provides zenget's user configuration store.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"zenget/internal/limits"
)

// Config holds user-configurable settings for zenget.
type Config struct {
	UsageTracking      bool           `json:"usage_tracking"`
	UsageUpload        bool           `json:"usage_upload"`
	InstallDir         string         `json:"install_dir"`
	ChecksumPolicy     ChecksumPolicy `json:"checksum_policy,omitempty"`
	LazyInstall        bool           `json:"lazy_install"`
	MaxStructuredBytes int64          `json:"max_structured_bytes,omitempty"`
	MaxChecksumBytes   int64          `json:"max_checksum_bytes,omitempty"`
	MaxDownloadBytes   int64          `json:"max_download_bytes,omitempty"`
	MaxBinaryBytes     int64          `json:"max_binary_bytes,omitempty"`
	MaxArchiveEntries  int64          `json:"max_archive_entries,omitempty"`
	MaxArchiveBytes    int64          `json:"max_archive_bytes,omitempty"`
}

// ChecksumPolicy controls how release checksum evidence is handled during an
// installation.
type ChecksumPolicy string

const (
	// ChecksumPolicyIfPresent preserves the compatible behavior where a
	// release without checksum evidence may still be installed.
	ChecksumPolicyIfPresent ChecksumPolicy = "if-present"
	// ChecksumPolicyRequired requires valid SHA-256 evidence for every
	// downloaded release asset before it is unpacked or installed.
	ChecksumPolicyRequired ChecksumPolicy = "required"
)

// ParseChecksumPolicy validates and canonicalizes a user-provided policy.
func ParseChecksumPolicy(value string) (ChecksumPolicy, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case string(ChecksumPolicyIfPresent):
		return ChecksumPolicyIfPresent, nil
	case string(ChecksumPolicyRequired):
		return ChecksumPolicyRequired, nil
	default:
		return "", fmt.Errorf("expected %q or %q", ChecksumPolicyIfPresent, ChecksumPolicyRequired)
	}
}

// EffectiveChecksumPolicy returns the configured policy, defaulting to
// if-present for nil configurations and older files without this field.
func (c *Config) EffectiveChecksumPolicy() ChecksumPolicy {
	if c == nil || c.ChecksumPolicy == "" {
		return ChecksumPolicyIfPresent
	}
	parsed, err := ParseChecksumPolicy(string(c.ChecksumPolicy))
	if err != nil {
		return ChecksumPolicyIfPresent
	}
	return parsed
}

// SetChecksumPolicy validates and stores one checksum policy.
func (c *Config) SetChecksumPolicy(value string) error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	parsed, err := ParseChecksumPolicy(value)
	if err != nil {
		return err
	}
	c.ChecksumPolicy = parsed
	return nil
}

// Path returns the path of the persistent configuration file.
func Path() (string, error) {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "zenget", "config.json"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}

	return filepath.Join(home, ".config", "zenget", "config.json"), nil
}

// Load reads the persistent configuration. A missing configuration file is
// treated as an empty, default configuration.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	data, err := limits.ReadFile(path, limits.DefaultStructuredBytes, "structured JSON")
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	var cfg Config
	if err := limits.DecodeJSON(bytes.NewReader(data), &cfg, limits.DefaultStructuredBytes, "structured JSON", path); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}
	if err := validateStoredLimits(data, &cfg); err != nil {
		return nil, fmt.Errorf("validate config file %q: %w", path, err)
	}
	if err := validateStoredChecksumPolicy(data, &cfg); err != nil {
		return nil, fmt.Errorf("validate config file %q: %w", path, err)
	}

	return &cfg, nil
}

// Save writes the persistent configuration atomically.
func (c *Config) Save() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if err := c.normalizeLimits(); err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config directory %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	temp, err := os.CreateTemp(dir, "config-*.json")
	if err != nil {
		return fmt.Errorf("create temporary config file in %q: %w", dir, err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if n, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary config file %q: %w", tempPath, err)
	} else if n != len(data) {
		_ = temp.Close()
		return fmt.Errorf("write temporary config file %q: %w", tempPath, io.ErrShortWrite)
	}
	if err := temp.Chmod(0644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set config file permissions %q: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary config file %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace config file %q: %w", path, err)
	}

	removeTemp = false
	return nil
}

// EffectiveLimits returns the configured finite limits, using the documented
// defaults for fields omitted by older configuration files.
func (c *Config) EffectiveLimits() limits.Limits {
	effective := limits.Defaults()
	if c == nil {
		return effective
	}
	if c.MaxStructuredBytes > 0 {
		effective.StructuredBytes = c.MaxStructuredBytes
	}
	if c.MaxChecksumBytes > 0 {
		effective.ChecksumBytes = c.MaxChecksumBytes
	}
	if c.MaxDownloadBytes > 0 {
		effective.DownloadBytes = c.MaxDownloadBytes
	}
	if c.MaxBinaryBytes > 0 {
		effective.BinaryBytes = c.MaxBinaryBytes
	}
	if c.MaxArchiveEntries > 0 {
		effective.ArchiveEntries = c.MaxArchiveEntries
	}
	if c.MaxArchiveBytes > 0 {
		effective.ArchiveBytes = c.MaxArchiveBytes
	}
	return effective
}

// Limit returns one effective resource limit by its canonical configuration
// key.
func (c *Config) Limit(key string) (int64, bool) {
	return c.EffectiveLimits().Value(key)
}

// SetLimit validates and stores one resource limit by its canonical key.
func (c *Config) SetLimit(key string, value int64) error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	effective := c.EffectiveLimits()
	if err := effective.Set(key, value); err != nil {
		return err
	}
	switch key {
	case limits.StructuredBytesKey:
		c.MaxStructuredBytes = value
	case limits.ChecksumBytesKey:
		c.MaxChecksumBytes = value
	case limits.DownloadBytesKey:
		c.MaxDownloadBytes = value
	case limits.BinaryBytesKey:
		c.MaxBinaryBytes = value
	case limits.ArchiveEntriesKey:
		c.MaxArchiveEntries = value
	case limits.ArchiveBytesKey:
		c.MaxArchiveBytes = value
	default:
		return fmt.Errorf("unknown limit key %q", key)
	}
	return nil
}

func (c *Config) normalizeLimits() error {
	if c.ChecksumPolicy != "" {
		parsed, err := ParseChecksumPolicy(string(c.ChecksumPolicy))
		if err != nil {
			return fmt.Errorf("checksum_policy: %w", err)
		}
		c.ChecksumPolicy = parsed
	}
	for _, item := range []struct {
		key   string
		value int64
	}{
		{limits.StructuredBytesKey, c.MaxStructuredBytes},
		{limits.ChecksumBytesKey, c.MaxChecksumBytes},
		{limits.DownloadBytesKey, c.MaxDownloadBytes},
		{limits.BinaryBytesKey, c.MaxBinaryBytes},
		{limits.ArchiveEntriesKey, c.MaxArchiveEntries},
		{limits.ArchiveBytesKey, c.MaxArchiveBytes},
	} {
		if item.value < 0 {
			return fmt.Errorf("%s must be a positive int64", item.key)
		}
	}
	values := c.EffectiveLimits()
	if err := values.Validate(); err != nil {
		return err
	}
	c.MaxStructuredBytes = values.StructuredBytes
	c.MaxChecksumBytes = values.ChecksumBytes
	c.MaxDownloadBytes = values.DownloadBytes
	c.MaxBinaryBytes = values.BinaryBytes
	c.MaxArchiveEntries = values.ArchiveEntries
	c.MaxArchiveBytes = values.ArchiveBytes
	return nil
}

func validateStoredLimits(data []byte, cfg *Config) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range limits.Keys() {
		value, present := raw[key]
		if !present {
			continue
		}
		var parsed int64
		if err := json.Unmarshal(value, &parsed); err != nil {
			return fmt.Errorf("%s must be a JSON integer: %w", key, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("%s must be a positive int64", key)
		}
	}
	return cfg.normalizeLimits()
}

func validateStoredChecksumPolicy(data []byte, cfg *Config) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	value, present := raw["checksum_policy"]
	if !present {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("checksum_policy must be a string")
	}
	var stored string
	if err := json.Unmarshal(value, &stored); err != nil {
		return fmt.Errorf("checksum_policy must be a string: %w", err)
	}
	parsed, err := ParseChecksumPolicy(stored)
	if err != nil {
		return fmt.Errorf("checksum_policy %q: %w", stored, err)
	}
	cfg.ChecksumPolicy = parsed
	return nil
}

// TrackingEnabled reports whether usage tracking is enabled.
func (c *Config) TrackingEnabled() bool {
	if c == nil {
		return false
	}
	return c.UsageTracking
}

// UploadEnabled reports whether usage events should be uploaded to the
// collector. Uploading requires both the usage_upload opt-in and usage_tracking
// to be enabled.
func (c *Config) UploadEnabled() bool {
	if c == nil {
		return false
	}
	return c.UsageUpload && c.UsageTracking
}

// LazyInstallEnabled reports whether project-shim cache misses may download an
// exact manifest artifact. ZENGET_LAZY_INSTALL=0 is a runtime-only opt-out and
// never changes the persisted configuration.
func (c *Config) LazyInstallEnabled() bool {
	return c != nil && c.LazyInstall && os.Getenv("ZENGET_LAZY_INSTALL") != "0"
}

// InstallDirectory returns the effective install directory for wrappers.
// When unset it defaults to ~/.local/bin. The returned path is expanded (no
// leading ~/).
func (c *Config) InstallDirectory() (string, error) {
	raw := ""
	if c != nil {
		raw = c.InstallDir
	}
	if raw == "" {
		raw = "~/.local/bin"
	}
	return ExpandInstallDir(raw)
}

// ExpandInstallDir expands a leading "~" in dir to the user's home directory.
// The empty string is expanded to ~/.local/bin.
func ExpandInstallDir(dir string) (string, error) {
	if dir == "" {
		dir = "~/.local/bin"
	}
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}
		return filepath.Join(home, dir[2:]), nil
	}
	return dir, nil
}

// ValidateInstallDir validates a candidate install directory string.
// It accepts an absolute path or a path starting with "~/"; the empty string
// is valid and means the default. Relative paths and bare "~" are rejected.
func ValidateInstallDir(dir string) error {
	if dir == "" {
		return nil
	}
	if dir == "~" {
		return fmt.Errorf("install_dir must be absolute or start with ~/")
	}
	if strings.HasPrefix(dir, "~/") {
		return nil
	}
	if filepath.IsAbs(dir) {
		return nil
	}
	return fmt.Errorf("install_dir must be absolute or start with ~/")
}

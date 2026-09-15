// Package appcfg stores per-repository install choices.
package appcfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"zenget/internal/assetselector"
	"zenget/internal/limits"
)

// AppConfig contains the choices that zenget may reuse for one repository.
// Empty fields mean that the corresponding choice should be resolved using
// the normal install heuristics.
type AppConfig struct {
	Asset         string                  `json:"asset,omitempty"`
	AssetSelector *assetselector.Selector `json:"asset_selector,omitempty"`
	ArchiveBinary string                  `json:"archive_binary,omitempty"`
	TargetName    string                  `json:"target_name,omitempty"`
}

// AssetSelector is the public app-config name for a reusable asset rule.
type AssetSelector = assetselector.Selector

// AssetSelectorType is the public app-config name for a selector match type.
type AssetSelectorType = assetselector.Type

const (
	// AssetSelectorSubstring selects a case-insensitive literal substring.
	AssetSelectorSubstring = assetselector.TypeSubstring
	// AssetSelectorRegex selects a RE2 regular expression.
	AssetSelectorRegex = assetselector.TypeRegex
)

// Path returns the per-application configuration path for repository.
func Path(repository string) (string, error) {
	org, repo, ok := splitRepository(repository)
	if !ok {
		return "", fmt.Errorf("invalid repository %q: expected org/repo", repository)
	}

	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find user home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}

	return filepath.Join(configHome, "zenget", "apps", org+"__"+repo+".json"), nil
}

// Load reads repository's application configuration. A missing file is an
// empty configuration and is not an error.
func Load(repository string) (*AppConfig, error) {
	path, err := Path(repository)
	if err != nil {
		return nil, err
	}

	data, err := limits.ReadFile(path, limits.DefaultStructuredBytes, "structured JSON")
	if err != nil {
		if os.IsNotExist(err) {
			return &AppConfig{}, nil
		}
		return nil, fmt.Errorf("read app config %q: %w", path, err)
	}

	var cfg AppConfig
	if err := limits.DecodeJSON(bytes.NewReader(data), &cfg, limits.DefaultStructuredBytes, "structured JSON", path); err != nil {
		return nil, fmt.Errorf("parse app config %q: %w", path, err)
	}
	if cfg.AssetSelector != nil {
		if err := cfg.AssetSelector.Validate(); err != nil {
			return nil, fmt.Errorf("validate app config %q: %w", path, err)
		}
	}
	return &cfg, nil
}

// Save writes repository's application configuration atomically.
func (c *AppConfig) Save(repository string) error {
	if c == nil {
		return fmt.Errorf("app config is nil")
	}
	if c.AssetSelector != nil {
		if err := c.AssetSelector.Validate(); err != nil {
			return err
		}
	}

	path, err := Path(repository)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return fmt.Errorf("create app config directory %q: %w", directory, err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal app config: %w", err)
	}

	temporary, err := os.CreateTemp(directory, "app-*.json")
	if err != nil {
		return fmt.Errorf("create temporary app config in %q: %w", directory, err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if n, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary app config %q: %w", temporaryPath, err)
	} else if n != len(data) {
		_ = temporary.Close()
		return fmt.Errorf("write temporary app config %q: %w", temporaryPath, io.ErrShortWrite)
	}
	if err := temporary.Chmod(0644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set app config permissions: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary app config %q: %w", temporaryPath, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace app config %q: %w", path, err)
	}

	removeTemporary = false
	return nil
}

func splitRepository(repository string) (string, string, bool) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || !validPart(parts[0]) || !validPart(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func validPart(part string) bool {
	if part == "" || part == "." || part == ".." {
		return false
	}
	for _, r := range part {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// Package state provides persistent state for installed applications.
package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"zenget/internal/limits"
)

// App describes an installed application.
type App struct {
	Provider         string    `json:"provider,omitempty"`
	Version          string    `json:"version"`
	InstalledAt      time.Time `json:"installed_at"`
	DownloadURL      string    `json:"download_url"`
	InstallPath      string    `json:"install_path"`
	RealPath         string    `json:"real_path"`
	XXH3_64          string    `json:"xxh3_64"`
	Pinned           bool      `json:"pinned"`
	PlatformOS       string    `json:"platform_os,omitempty"`
	PlatformArch     string    `json:"platform_arch,omitempty"`
	ReleaseFilter    string    `json:"release_filter,omitempty"`
	ActiveArtifact   *Artifact `json:"active_artifact,omitempty"`
	PreviousArtifact *Artifact `json:"previous_artifact,omitempty"`
}

// ManifestSchemaVersion is the current portable tool-manifest schema version.
const ManifestSchemaVersion = 1

// Manifest describes a portable set of installation intentions. SchemaURL is
// optional for compatibility with hand-written version 1 files.
type Manifest struct {
	SchemaVersion int           `json:"schema_version"`
	SchemaURL     string        `json:"$schema,omitempty"`
	Apps          []ManifestApp `json:"apps"`
}

// ManifestApp contains only portable choices needed to reproduce one install.
// Runtime paths, download URLs, timestamps, hashes, and usage data are kept out
// of this type deliberately.
type ManifestApp struct {
	Repository    string `json:"repository"`
	Provider      string `json:"provider"`
	Tag           string `json:"tag"`
	Asset         string `json:"asset,omitempty"`
	PlatformOS    string `json:"platform_os,omitempty"`
	PlatformArch  string `json:"platform_arch,omitempty"`
	ArchiveBinary string `json:"archive_binary,omitempty"`
	TargetName    string `json:"target_name,omitempty"`
}

// Artifact identifies a versioned real binary retained by zenget.
type Artifact struct {
	Version  string `json:"version"`
	RealPath string `json:"real_path"`
	XXH3_64  string `json:"xxh3_64"`
}

// Active returns the active artifact metadata, falling back to the legacy
// top-level fields for state files written before versioned storage existed.
func (a App) Active() *Artifact {
	if a.ActiveArtifact != nil {
		return cloneArtifact(a.ActiveArtifact)
	}
	if a.RealPath == "" {
		return nil
	}
	return &Artifact{Version: a.Version, RealPath: a.RealPath, XXH3_64: a.XXH3_64}
}

// ArtifactPaths returns active and previous artifact metadata without
// exposing the pointers stored in the application value to callers.
func (a App) ArtifactPaths() []*Artifact {
	var artifacts []*Artifact
	if active := a.Active(); active != nil {
		artifacts = append(artifacts, active)
	}
	if a.PreviousArtifact != nil {
		artifacts = append(artifacts, cloneArtifact(a.PreviousArtifact))
	}
	return artifacts
}

func cloneArtifact(artifact *Artifact) *Artifact {
	if artifact == nil {
		return nil
	}
	copy := *artifact
	return &copy
}

// State is the registry of installed applications, keyed by repository.
type State struct {
	Apps map[string]App `json:"apps"`
}

// Path returns the path of the persistent state file.
func Path() (string, error) {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "zenget", "state.json"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}

	return filepath.Join(home, ".config", "zenget", "state.json"), nil
}

// Load reads the persistent state. A missing state file is treated as an
// empty registry.
func Load() (*State, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	data, err := limits.ReadFile(path, limits.DefaultStructuredBytes, "structured JSON")
	if err != nil {
		if os.IsNotExist(err) {
			return &State{Apps: make(map[string]App)}, nil
		}
		return nil, fmt.Errorf("read state file %q: %w", path, err)
	}

	var state State
	if err := limits.DecodeJSON(bytes.NewReader(data), &state, limits.DefaultStructuredBytes, "structured JSON", path); err != nil {
		return nil, fmt.Errorf("parse state file %q: %w", path, err)
	}
	if state.Apps == nil {
		state.Apps = make(map[string]App)
	}

	return &state, nil
}

// Save writes the persistent state atomically.
func (s *State) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create state directory %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	temp, err := os.CreateTemp(dir, "state-*.json")
	if err != nil {
		return fmt.Errorf("create temporary state file in %q: %w", dir, err)
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
		return fmt.Errorf("write temporary state file %q: %w", tempPath, err)
	} else if n != len(data) {
		_ = temp.Close()
		return fmt.Errorf("write temporary state file %q: %w", tempPath, io.ErrShortWrite)
	}
	if err := temp.Chmod(0644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set state file permissions %q: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary state file %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace state file %q: %w", path, err)
	}

	removeTemp = false
	return nil
}

// Get returns the application registered under key.
func (s *State) Get(key string) (App, bool) {
	if s == nil {
		return App{}, false
	}
	app, ok := s.Apps[key]
	return app, ok
}

// Set registers an application under key.
func (s *State) Set(key string, app App) {
	if s.Apps == nil {
		s.Apps = make(map[string]App)
	}
	s.Apps[key] = app
}

// Delete removes the application registered under key.
func (s *State) Delete(key string) {
	if s == nil {
		return
	}
	delete(s.Apps, key)
}

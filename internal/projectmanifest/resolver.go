// Package projectmanifest resolves the manifest belonging to the current
// project and loads it through the shared strict manifest parser.
package projectmanifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"zenget/internal/manifest"
	"zenget/internal/state"
)

const (
	// DefaultFileName is the only filename considered by automatic discovery.
	DefaultFileName = "zenget.json"
	noDiscoveryEnv  = "ZENGET_NO_MANIFEST_DISCOVERY"
	manifestEnv     = "ZENGET_MANIFEST"
)

// Source identifies how a manifest path was selected.
type Source string

const (
	// SourceFlag identifies an explicit command-line path.
	SourceFlag Source = "flag"
	// SourceEnvironment identifies the ZENGET_MANIFEST path.
	SourceEnvironment Source = "environment"
	// SourceDiscovered identifies an automatically discovered path.
	SourceDiscovered Source = "discovered"
)

// Options controls manifest resolution. WorkingDirectory is primarily useful
// to callers that need a stable starting directory for tests or wrappers; an
// empty value uses the process working directory.
type Options struct {
	ExplicitPath     string
	NoDiscovery      bool
	WorkingDirectory string
}

// Resolution contains the selected canonical path, its source, and the
// strictly validated manifest loaded from that path.
type Resolution struct {
	Path     string
	Source   Source
	Manifest state.Manifest
}

// ErrNotFound is wrapped by NoManifestError when discovery has no usable
// project manifest.
var ErrNotFound = errors.New("project manifest not found")

// NoManifestError reports that no manifest could be selected. Its exit code
// is intentionally distinct so scripts can distinguish absence from an
// invalid or unreadable manifest.
type NoManifestError struct {
	Directory string
	Disabled  bool
}

func (e *NoManifestError) Error() string {
	if e.Disabled {
		return fmt.Sprintf("no project manifest found: discovery is disabled; provide --manifest or %s", manifestEnv)
	}
	return fmt.Sprintf("no project manifest found from %s to the filesystem root; expected %s", e.Directory, DefaultFileName)
}

// Unwrap allows callers to use errors.Is with ErrNotFound.
func (e *NoManifestError) Unwrap() error { return ErrNotFound }

// ExitCode returns the documented command exit status for an absent manifest.
func (e *NoManifestError) ExitCode() int { return 2 }

// SelectionError reports a manifest path and source when path validation or
// strict manifest loading fails. Path is absolute and cleaned whenever the
// input was a local path.
type SelectionError struct {
	Source Source
	Path   string
	Err    error
}

func (e *SelectionError) Error() string {
	return fmt.Sprintf("manifest source=%s path=%s: %v", e.Source, e.Path, e.Err)
}

// Unwrap exposes the parser or path error to callers.
func (e *SelectionError) Unwrap() error { return e.Err }

// Resolve selects and strictly loads a project manifest. Precedence is an
// explicit path, ZENGET_MANIFEST, then the nearest regular non-symlink
// zenget.json discovered from the working directory toward the filesystem
// root. The schema is never fetched from the network.
func Resolve(options Options) (Resolution, error) {
	directory, err := startDirectory(options.WorkingDirectory)
	if err != nil {
		return Resolution{}, err
	}
	if options.ExplicitPath != "" {
		return loadSelected(directory, SourceFlag, options.ExplicitPath)
	}
	if environmentPath, ok := os.LookupEnv(manifestEnv); ok && environmentPath != "" {
		return loadSelected(directory, SourceEnvironment, environmentPath)
	}
	if options.NoDiscovery || os.Getenv(noDiscoveryEnv) == "1" {
		return Resolution{}, &NoManifestError{Directory: directory, Disabled: true}
	}

	discoveredPath := discover(directory)
	if discoveredPath == "" {
		return Resolution{}, &NoManifestError{Directory: directory}
	}
	return loadSelected(directory, SourceDiscovered, discoveredPath)
}

func startDirectory(workingDirectory string) (string, error) {
	if workingDirectory == "" {
		var err error
		workingDirectory, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("find working directory: %w", err)
		}
	}
	if !filepath.IsAbs(workingDirectory) {
		current, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("find working directory: %w", err)
		}
		workingDirectory = filepath.Join(current, workingDirectory)
	}
	return filepath.Clean(workingDirectory), nil
}

func loadSelected(directory string, source Source, rawPath string) (Resolution, error) {
	path, err := localPath(directory, rawPath)
	if err != nil {
		return Resolution{}, &SelectionError{Source: source, Path: rawPath, Err: err}
	}
	file, err := manifest.Load(path)
	if err != nil {
		return Resolution{}, &SelectionError{Source: source, Path: path, Err: err}
	}
	return Resolution{Path: path, Source: source, Manifest: file}, nil
}

func localPath(directory, rawPath string) (string, error) {
	if !manifest.IsLocalPath(rawPath) {
		return "", fmt.Errorf("manifest path must be a local file")
	}
	if !filepath.IsAbs(rawPath) {
		rawPath = filepath.Join(directory, rawPath)
	}
	return filepath.Clean(rawPath), nil
}

func discover(directory string) string {
	for {
		candidate := filepath.Join(directory, DefaultFileName)
		if isRegularNonSymlink(candidate) {
			return candidate
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
		directory = parent
	}
}

func isRegularNonSymlink(candidate string) bool {
	info, err := os.Lstat(candidate)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

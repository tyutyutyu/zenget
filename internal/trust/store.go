// Package trust stores explicit trust decisions for local project manifests.
package trust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"zenget/internal/fileowner"
	"zenget/internal/limits"
)

const (
	// SchemaVersion is the version of the project trust store format.
	SchemaVersion = 1
	storeFileName = "project-trust.json"
)

// Record grants path-based trust to one local manifest and its project root.
// Trust intentionally does not include a content hash: a trusted project may
// update exact release tags through a reviewed workflow such as Renovate.
type Record struct {
	ManifestPath string `json:"manifest_path"`
	ProjectRoot  string `json:"project_root"`
}

// Store is the atomically persisted collection of project trust records.
type Store struct {
	SchemaVersion int      `json:"schema_version"`
	Projects      []Record `json:"projects"`
}

// ErrNotTrusted reports that a manifest has no explicit trust record.
var ErrNotTrusted = errors.New("project manifest is not trusted")

// New returns an empty current-format trust store.
func New() Store {
	return Store{SchemaVersion: SchemaVersion, Projects: make([]Record, 0)}
}

// Path returns the persistent project trust store path.
func Path() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find user home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "zenget", storeFileName), nil
}

// ProjectRootFor returns the directory containing a manifest after converting
// its path to a cleaned absolute path.
func ProjectRootFor(manifestPath string) (string, error) {
	absolute, err := absolutePath(manifestPath, "manifest path")
	if err != nil {
		return "", err
	}
	return filepath.Dir(absolute), nil
}

// Load reads the trust store. A missing store is an empty current-format
// store. Existing stores are strictly decoded and structurally validated.
func Load() (Store, error) {
	path, err := Path()
	if err != nil {
		return Store{}, err
	}

	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return Store{}, fmt.Errorf("refusing to read trust store through symlink %q", path)
		}
		if !info.Mode().IsRegular() {
			return Store{}, fmt.Errorf("trust store %q is not a regular file", path)
		}
		if err := validateOwnerAndMode(info, path, "trust store"); err != nil {
			return Store{}, err
		}
	} else if !os.IsNotExist(statErr) {
		return Store{}, fmt.Errorf("inspect trust store %q: %w", path, statErr)
	}

	data, err := limits.ReadFile(path, limits.DefaultStructuredBytes, "project trust store")
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil {
		return Store{}, fmt.Errorf("read trust store %q: %w", path, err)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Store{}, fmt.Errorf("decode trust store %q: %w", path, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var store Store
	if err := decoder.Decode(&store); err != nil {
		return Store{}, fmt.Errorf("decode trust store %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Store{}, fmt.Errorf("decode trust store %q: multiple JSON values", path)
		}
		return Store{}, fmt.Errorf("decode trust store %q: trailing data: %w", path, err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return Store{}, fmt.Errorf("decode trust store %q: %w", path, err)
	}
	if object == nil {
		return Store{}, fmt.Errorf("trust store %q must be a JSON object", path)
	}
	if _, ok := object["schema_version"]; !ok {
		return Store{}, fmt.Errorf("trust store %q is missing schema_version", path)
	}
	projects, ok := object["projects"]
	if !ok || bytes.Equal(bytes.TrimSpace(projects), []byte("null")) {
		return Store{}, fmt.Errorf("trust store %q is missing projects", path)
	}
	if err := validateStore(store); err != nil {
		return Store{}, fmt.Errorf("validate trust store %q: %w", path, err)
	}
	return canonicalStore(store), nil
}

// Save atomically replaces the persistent trust store.
func Save(store Store) error {
	canonical, err := normalizeStore(store)
	if err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create trust store directory %q: %w", directory, err)
	}
	if err := fileowner.SecurePath(directory, true); err != nil {
		return fmt.Errorf("secure trust store directory %q: %w", directory, err)
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to replace trust store symlink %q", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("trust store %q is not a regular file", path)
		}
		if err := validateOwnerAndMode(info, path, "trust store"); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect trust store %q: %w", path, statErr)
	}

	data, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trust store: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".project-trust-*.json")
	if err != nil {
		return fmt.Errorf("create temporary trust store in %q: %w", directory, err)
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
		return fmt.Errorf("write temporary trust store %q: %w", temporaryPath, err)
	} else if n != len(data) {
		_ = temporary.Close()
		return fmt.Errorf("write temporary trust store %q: %w", temporaryPath, io.ErrShortWrite)
	}
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set trust store permissions: %w", err)
	}
	if err := fileowner.SecurePath(temporaryPath, false); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary trust store: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary trust store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary trust store %q: %w", temporaryPath, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace trust store %q: %w", path, err)
	}
	removeTemporary = false
	if err := fileowner.SecurePath(path, false); err != nil {
		return fmt.Errorf("secure trust store after replacement: %w", err)
	}
	return nil
}

// Add records a trusted manifest after checking its current path safety. The
// returned boolean is false when an identical record was already present.
func (s *Store) Add(manifestPath string) (Record, bool, error) {
	if s == nil {
		return Record{}, false, errors.New("trust store is nil")
	}
	record, err := NewRecord(manifestPath)
	if err != nil {
		return Record{}, false, err
	}
	if err := CheckPathSafety(record); err != nil {
		return Record{}, false, err
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = SchemaVersion
	}
	for index, existing := range s.Projects {
		if existing.ManifestPath != record.ManifestPath {
			continue
		}
		if existing == record {
			return record, false, nil
		}
		s.Projects[index] = record
		*s = canonicalStore(*s)
		return record, true, nil
	}
	s.Projects = append(s.Projects, record)
	*s = canonicalStore(*s)
	return record, true, nil
}

// Remove deletes the record for manifestPath. It intentionally does not check
// the manifest's current safety so an unsafe project can still be untrusted.
func (s *Store) Remove(manifestPath string) (bool, error) {
	if s == nil {
		return false, errors.New("trust store is nil")
	}
	absolute, err := absolutePath(manifestPath, "manifest path")
	if err != nil {
		return false, err
	}
	for index, record := range s.Projects {
		if record.ManifestPath != absolute {
			continue
		}
		s.Projects = append(s.Projects[:index], s.Projects[index+1:]...)
		return true, nil
	}
	return false, nil
}

// Find returns the record for manifestPath without checking current path
// safety. Call CheckPathSafety before using a record for activation or lazy
// execution.
func (s Store) Find(manifestPath string) (Record, bool, error) {
	absolute, err := absolutePath(manifestPath, "manifest path")
	if err != nil {
		return Record{}, false, err
	}
	for _, record := range s.Projects {
		if record.ManifestPath == absolute {
			return record, true, nil
		}
	}
	return Record{}, false, nil
}

// NewRecord creates a canonical path-based trust record for manifestPath.
func NewRecord(manifestPath string) (Record, error) {
	absolute, err := absolutePath(manifestPath, "manifest path")
	if err != nil {
		return Record{}, err
	}
	return Record{ManifestPath: absolute, ProjectRoot: filepath.Dir(absolute)}, nil
}

// CheckPathSafety verifies that the manifest and every directory from its
// parent through the recorded project root are user-owned, non-symlink paths,
// and not group/world writable.
func CheckPathSafety(record Record) error {
	normalized, err := normalizeRecord(record)
	if err != nil {
		return err
	}
	manifestDirectory := filepath.Dir(normalized.ManifestPath)
	relative, err := filepath.Rel(normalized.ProjectRoot, manifestDirectory)
	if err != nil {
		return fmt.Errorf("compare manifest and project root: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("project root %q is not an ancestor of manifest %q", normalized.ProjectRoot, normalized.ManifestPath)
	}

	info, err := os.Lstat(normalized.ManifestPath)
	if err != nil {
		return fmt.Errorf("inspect trusted manifest %q: %w", normalized.ManifestPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("trusted manifest %q is a symlink", normalized.ManifestPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("trusted manifest %q is not a regular file", normalized.ManifestPath)
	}
	if err := validateOwnerAndMode(info, normalized.ManifestPath, "trusted manifest"); err != nil {
		return err
	}

	for directory := manifestDirectory; ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil {
			return fmt.Errorf("inspect trusted project directory %q: %w", directory, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("trusted project directory %q is a symlink", directory)
		}
		if !info.IsDir() {
			return fmt.Errorf("trusted project path %q is not a directory", directory)
		}
		if err := validateOwnerAndMode(info, directory, "trusted project directory"); err != nil {
			return err
		}
		if directory == normalized.ProjectRoot {
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return fmt.Errorf("project root %q was not reached", normalized.ProjectRoot)
		}
	}
	return nil
}

func normalizeStore(store Store) (Store, error) {
	if store.SchemaVersion == 0 {
		store.SchemaVersion = SchemaVersion
	}
	if store.Projects == nil {
		store.Projects = make([]Record, 0)
	}
	if err := validateStore(store); err != nil {
		return Store{}, err
	}
	return canonicalStore(store), nil
}

func validateStore(store Store) error {
	if store.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d; want %d", store.SchemaVersion, SchemaVersion)
	}
	if store.Projects == nil {
		return errors.New("projects must be an array")
	}
	seen := make(map[string]struct{}, len(store.Projects))
	for index, record := range store.Projects {
		normalized, err := normalizeRecord(record)
		if err != nil {
			return fmt.Errorf("projects[%d]: %w", index, err)
		}
		if normalized != record {
			return fmt.Errorf("projects[%d]: paths must be canonical absolute paths", index)
		}
		if _, ok := seen[record.ManifestPath]; ok {
			return fmt.Errorf("projects[%d] duplicates manifest %q", index, record.ManifestPath)
		}
		seen[record.ManifestPath] = struct{}{}
	}
	return nil
}

func normalizeRecord(record Record) (Record, error) {
	manifestPath, err := absolutePath(record.ManifestPath, "manifest path")
	if err != nil {
		return Record{}, err
	}
	projectRoot, err := absolutePath(record.ProjectRoot, "project root")
	if err != nil {
		return Record{}, err
	}
	return Record{ManifestPath: manifestPath, ProjectRoot: projectRoot}, nil
}

func canonicalStore(store Store) Store {
	store.SchemaVersion = SchemaVersion
	if store.Projects == nil {
		store.Projects = make([]Record, 0)
	}
	sort.Slice(store.Projects, func(left, right int) bool {
		return store.Projects[left].ManifestPath < store.Projects[right].ManifestPath
	})
	return store
}

func absolutePath(value, label string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("%s contains NUL", label)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", label, value, err)
	}
	return filepath.Clean(absolute), nil
}

func validateOwnerAndMode(info os.FileInfo, path, label string) error {
	modeSafe := fileowner.PrivateDirectoryModeSafe(info)
	if !modeSafe {
		return fmt.Errorf("%s %q is group/world writable", label, path)
	}
	if !fileowner.CurrentUserOwnsPath(path, info) {
		return fmt.Errorf("inspect %s %q: ownership is unavailable", label, path)
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if delim == '[' {
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	return nil
}

// Package registry implements zenget's commit-pinned remote recipe registry.
//
// A registry is deliberately a small, immutable snapshot format. The config
// stores only a validated GitHub source identity; a snapshot stores the
// canonical registry index and the recipe bytes addressed by their SHA-256
// digests. No release metadata, URLs, credentials, or local recipe paths are
// persisted here.
package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"zenget/internal/fileowner"
	"zenget/internal/limits"
	"zenget/internal/recipe"
	"zenget/internal/recipepolicy"
)

const (
	// SchemaVersion is the schema version for registry config, indexes, and
	// snapshot metadata written by this package.
	SchemaVersion = 1
	// ConfigFileName is the global registry configuration basename.
	ConfigFileName = "registries.json"
	// IndexFileName is the remote registry index basename.
	IndexFileName = "registry.json"
	// SnapshotPointerFileName is the active-snapshot pointer basename.
	SnapshotPointerFileName = "current.json"
)

var (
	registryNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	repositoryPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	commitPattern       = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	digestPattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Source identifies one immutable registry root. An empty Root denotes the
// repository root and is rendered as a trailing colon in String.
type Source struct {
	Owner      string
	Repository string
	Commit     string
	Root       string
}

// ParseSource parses the only supported registry source grammar:
// github:owner/repository@<40-hex-commit>:<relative-root>.
func ParseSource(value string) (Source, error) {
	if value == "" || strings.ContainsAny(value, "?#%\\\x00\r\n\t ") {
		return Source{}, errors.New("registry source must be a clean pinned GitHub identity")
	}
	if !strings.HasPrefix(value, "github:") {
		return Source{}, errors.New("registry source must use github:owner/repository@<40-hex-commit>:<relative-root>")
	}
	remainder := strings.TrimPrefix(value, "github:")
	at := strings.IndexByte(remainder, '@')
	if at <= 0 || strings.Count(remainder, "@") != 1 {
		return Source{}, errors.New("registry source must contain one commit pin")
	}
	repository, pinned := remainder[:at], remainder[at+1:]
	colon := strings.IndexByte(pinned, ':')
	if colon < 0 || strings.Count(pinned, ":") != 1 {
		return Source{}, errors.New("registry source must contain one relative root after the commit")
	}
	commit, root := pinned[:colon], pinned[colon+1:]
	if !repositoryPattern.MatchString(repository) {
		return Source{}, errors.New("registry source must identify one owner and repository")
	}
	owner, repo, _ := strings.Cut(repository, "/")
	if !commitPattern.MatchString(commit) {
		return Source{}, errors.New("registry source commit must be exactly 40 hexadecimal characters")
	}
	cleanRoot, err := normalizeRelativePath(root, true)
	if err != nil {
		return Source{}, fmt.Errorf("registry source root: %w", err)
	}
	return Source{Owner: owner, Repository: repo, Commit: strings.ToLower(commit), Root: cleanRoot}, nil
}

// Normalize validates and canonicalizes a source value.
func (s Source) Normalize() (Source, error) {
	return ParseSource(s.String())
}

// String returns the canonical source grammar. Invalid values are rendered as
// a non-sensitive placeholder rather than leaking arbitrary input.
func (s Source) String() string {
	if s.Owner == "" || s.Repository == "" || s.Commit == "" {
		return "<invalid-registry-source>"
	}
	value := "github:" + s.Owner + "/" + s.Repository + "@" + strings.ToLower(s.Commit) + ":"
	return value + s.Root
}

// RepositoryName returns the source's owner/repository identity.
func (s Source) RepositoryName() string {
	return s.Owner + "/" + s.Repository
}

// Path resolves a clean path relative to the source root. The returned value
// is always repository-relative and never begins with a slash.
func (s Source) Path(relative string) (string, error) {
	normalized, err := s.Normalize()
	if err != nil {
		return "", err
	}
	clean, err := normalizeRelativePath(relative, false)
	if err != nil {
		return "", err
	}
	if normalized.Root == "" {
		return clean, nil
	}
	return normalized.Root + "/" + clean, nil
}

// IndexPath returns the repository-relative path of the registry index.
func (s Source) IndexPath() (string, error) {
	return s.Path(IndexFileName)
}

// PolicyIdentity returns the recipe-policy identity for one source-relative
// registry file.
func (s Source) PolicyIdentity(relative string) (recipepolicy.SourceIdentity, error) {
	pathValue, err := s.Path(relative)
	if err != nil {
		return recipepolicy.SourceIdentity{}, err
	}
	return recipepolicy.NewSourceIdentity(recipepolicy.GitHubHost, s.Owner, s.Repository, pathValue)
}

// Registry is one named registry configuration entry.
type Registry struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// Config is the global named-registry configuration.
type Config struct {
	SchemaVersion int        `json:"schema_version"`
	Registries    []Registry `json:"registries"`
}

// DefaultConfig returns an empty, schema-versioned configuration.
func DefaultConfig() Config {
	return Config{SchemaVersion: SchemaVersion, Registries: []Registry{}}
}

// Path returns the global registry configuration path.
func Path() (string, error) {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "zenget", ConfigFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}
	return filepath.Join(home, ".config", "zenget", ConfigFileName), nil
}

// LoadConfig loads the strict persistent registry configuration.
func LoadConfig() (Config, error) {
	configPath, err := Path()
	if err != nil {
		return Config{}, err
	}
	info, err := os.Lstat(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return Config{}, fmt.Errorf("inspect registry config: %w", err)
	}
	if err := validatePrivateFile(info, "registry config"); err != nil {
		return Config{}, err
	}
	data, err := limits.ReadFile(configPath, limits.DefaultStructuredBytes, "registry config")
	if err != nil {
		return Config{}, fmt.Errorf("read registry config: %w", err)
	}
	return DecodeConfig(data)
}

// SaveConfig validates and atomically writes the registry configuration.
func SaveConfig(config Config) error {
	normalized, err := config.Normalize()
	if err != nil {
		return err
	}
	configPath, err := Path()
	if err != nil {
		return err
	}
	directory := filepath.Dir(configPath)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create registry config directory: %w", err)
	}
	if info, statErr := os.Lstat(configPath); statErr == nil {
		if err := validatePrivateFile(info, "registry config"); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect registry config: %w", statErr)
	}
	data, err := MarshalConfig(normalized)
	if err != nil {
		return err
	}
	if err := atomicWrite(configPath, directory, ".zenget-registries-*", data, 0600, "registry config"); err != nil {
		return err
	}
	return nil
}

// DecodeConfig strictly validates config JSON supplied by a caller.
func DecodeConfig(data []byte) (Config, error) {
	var config Config
	object, err := decodeObject(data, &config, "registry config")
	if err != nil {
		return Config{}, err
	}
	if _, ok := object["schema_version"]; !ok {
		return Config{}, errors.New("registry config is missing schema_version")
	}
	registries, ok := object["registries"]
	if !ok || bytes.Equal(bytes.TrimSpace(registries), []byte("null")) {
		return Config{}, errors.New("registry config registries must be an array")
	}
	return config.Normalize()
}

// MarshalConfig returns deterministic config JSON with a trailing newline.
func MarshalConfig(config Config) ([]byte, error) {
	normalized, err := config.Normalize()
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal registry config: %w", err)
	}
	return append(data, '\n'), nil
}

// Normalize validates and sorts config entries by registry name.
func (c Config) Normalize() (Config, error) {
	if c.SchemaVersion != SchemaVersion {
		return Config{}, fmt.Errorf("unsupported registry config schema_version %d; want %d", c.SchemaVersion, SchemaVersion)
	}
	normalized := Config{SchemaVersion: SchemaVersion, Registries: make([]Registry, 0, len(c.Registries))}
	seen := make(map[string]struct{}, len(c.Registries))
	for _, entry := range c.Registries {
		canonical, err := entry.Normalize()
		if err != nil {
			return Config{}, err
		}
		if _, exists := seen[canonical.Name]; exists {
			return Config{}, fmt.Errorf("registry config contains duplicate name %q", canonical.Name)
		}
		seen[canonical.Name] = struct{}{}
		normalized.Registries = append(normalized.Registries, canonical)
	}
	sort.Slice(normalized.Registries, func(left, right int) bool {
		return normalized.Registries[left].Name < normalized.Registries[right].Name
	})
	return normalized, nil
}

// Validate checks config without filesystem or network access.
func (c Config) Validate() error {
	_, err := c.Normalize()
	return err
}

// Normalize validates and canonicalizes one named registry entry.
func (r Registry) Normalize() (Registry, error) {
	name := strings.TrimSpace(r.Name)
	if name == "" || name != r.Name || !registryNamePattern.MatchString(name) || name == "." || name == ".." {
		return Registry{}, fmt.Errorf("invalid registry name %q", r.Name)
	}
	source, err := ParseSource(r.Source)
	if err != nil {
		return Registry{}, fmt.Errorf("registry %q source: %w", name, err)
	}
	return Registry{Name: name, Source: source.String()}, nil
}

// Add inserts a named source. Re-adding the same source is idempotent; a
// different source under an existing name is rejected explicitly.
func (c *Config) Add(name, sourceValue string) (bool, error) {
	if c == nil {
		return false, errors.New("registry config is nil")
	}
	normalized, err := c.Normalize()
	if err != nil {
		return false, err
	}
	entry, err := (Registry{Name: name, Source: sourceValue}).Normalize()
	if err != nil {
		return false, err
	}
	for _, existing := range normalized.Registries {
		if existing.Name != entry.Name {
			continue
		}
		if existing.Source == entry.Source {
			*c = normalized
			return false, nil
		}
		return false, fmt.Errorf("registry %q already exists with a different source", entry.Name)
	}
	normalized.Registries = append(normalized.Registries, entry)
	normalized, err = normalized.Normalize()
	if err != nil {
		return false, err
	}
	*c = normalized
	return true, nil
}

// Remove deletes one named registry and reports whether the config changed.
func (c *Config) Remove(name string) (bool, error) {
	if c == nil {
		return false, errors.New("registry config is nil")
	}
	normalized, err := c.Normalize()
	if err != nil {
		return false, err
	}
	for index, entry := range normalized.Registries {
		if entry.Name != name {
			continue
		}
		normalized.Registries = append(normalized.Registries[:index], normalized.Registries[index+1:]...)
		*c = normalized
		return true, nil
	}
	*c = normalized
	return false, nil
}

// Find returns one named registry after canonical validation.
func (c Config) Find(name string) (Registry, bool, error) {
	normalized, err := c.Normalize()
	if err != nil {
		return Registry{}, false, err
	}
	for _, entry := range normalized.Registries {
		if entry.Name == name {
			return entry, true, nil
		}
	}
	return Registry{}, false, nil
}

// IndexEntry identifies one recipe file in a registry index. Path is relative
// to the configured source root.
type IndexEntry struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
}

// Index is the strict version 1 registry.json document.
type Index struct {
	SchemaVersion int          `json:"schema_version"`
	Entries       []IndexEntry `json:"entries"`
}

// Normalize validates an index and returns a canonical repository/path order.
func (i Index) Normalize() (Index, error) {
	if i.SchemaVersion != SchemaVersion {
		return Index{}, fmt.Errorf("unsupported registry index schema_version %d; want %d", i.SchemaVersion, SchemaVersion)
	}
	normalized := Index{SchemaVersion: SchemaVersion, Entries: make([]IndexEntry, 0, len(i.Entries))}
	seenRepositories := make(map[string]struct{}, len(i.Entries))
	seenPaths := make(map[string]struct{}, len(i.Entries))
	for position, entry := range i.Entries {
		canonical, err := entry.Normalize()
		if err != nil {
			return Index{}, fmt.Errorf("registry index entries[%d]: %w", position, err)
		}
		if _, exists := seenRepositories[canonical.Repository]; exists {
			return Index{}, fmt.Errorf("registry index contains duplicate repository %q", canonical.Repository)
		}
		if _, exists := seenPaths[canonical.Path]; exists {
			return Index{}, fmt.Errorf("registry index contains duplicate path %q", canonical.Path)
		}
		seenRepositories[canonical.Repository] = struct{}{}
		seenPaths[canonical.Path] = struct{}{}
		normalized.Entries = append(normalized.Entries, canonical)
	}
	sort.Slice(normalized.Entries, func(left, right int) bool {
		if normalized.Entries[left].Repository != normalized.Entries[right].Repository {
			return normalized.Entries[left].Repository < normalized.Entries[right].Repository
		}
		return normalized.Entries[left].Path < normalized.Entries[right].Path
	})
	return normalized, nil
}

// ValidateCanonical checks the index and requires its entries to already be
// in canonical order. Remote indexes are not silently reordered.
func (i Index) ValidateCanonical() error {
	normalized, err := i.Normalize()
	if err != nil {
		return err
	}
	if len(normalized.Entries) != len(i.Entries) {
		return errors.New("registry index entries are not canonically sorted")
	}
	for position := range normalized.Entries {
		if normalized.Entries[position] != i.Entries[position] {
			return errors.New("registry index entries are not canonically sorted")
		}
	}
	return nil
}

// Normalize validates one index entry.
func (e IndexEntry) Normalize() (IndexEntry, error) {
	if !repositoryPattern.MatchString(e.Repository) {
		return IndexEntry{}, fmt.Errorf("invalid repository %q", e.Repository)
	}
	cleanPath, err := normalizeRelativePath(e.Path, false)
	if err != nil || cleanPath != e.Path {
		if err == nil {
			err = errors.New("path is not canonical")
		}
		return IndexEntry{}, fmt.Errorf("invalid path %q: %w", e.Path, err)
	}
	if !digestPattern.MatchString(e.SHA256) {
		return IndexEntry{}, fmt.Errorf("invalid sha256 digest %q: expected lowercase 64 hexadecimal characters", e.SHA256)
	}
	return IndexEntry{Repository: e.Repository, Path: cleanPath, SHA256: e.SHA256}, nil
}

// DecodeIndex strictly validates a remote or cached registry index.
func DecodeIndex(data []byte) (Index, error) {
	var index Index
	object, err := decodeObject(data, &index, "registry index")
	if err != nil {
		return Index{}, err
	}
	if _, ok := object["schema_version"]; !ok {
		return Index{}, errors.New("registry index is missing schema_version")
	}
	entries, ok := object["entries"]
	if !ok || bytes.Equal(bytes.TrimSpace(entries), []byte("null")) {
		return Index{}, errors.New("registry index entries must be an array")
	}
	if err := index.ValidateCanonical(); err != nil {
		return Index{}, err
	}
	return index, nil
}

// MarshalIndex returns canonical index JSON with a trailing newline.
func MarshalIndex(index Index) ([]byte, error) {
	normalized, err := index.Normalize()
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal registry index: %w", err)
	}
	return append(data, '\n'), nil
}

// ErrSnapshotNotFound indicates that no active snapshot exists for a source.
var ErrSnapshotNotFound = errors.New("registry snapshot is not synchronized")

// ErrRecipeNotFound indicates that the active snapshot for a source contains
// no recipe for the requested repository.
var ErrRecipeNotFound = errors.New("registry snapshot has no recipe for repository")

// Snapshot contains one validated immutable registry snapshot in memory.
type Snapshot struct {
	Source      Source
	Index       Index
	IndexSHA256 string
	Recipes     map[string][]byte
}

// Recipe returns a defensive copy of the recipe content for repository.
func (s Snapshot) Recipe(repository string) (IndexEntry, []byte, bool) {
	for _, entry := range s.Index.Entries {
		if entry.Repository != repository {
			continue
		}
		data, ok := s.Recipes[entry.Path]
		if !ok {
			return IndexEntry{}, nil, false
		}
		return entry, append([]byte(nil), data...), true
	}
	return IndexEntry{}, nil, false
}

// Cache is a filesystem-backed immutable registry snapshot cache.
type Cache struct {
	root string
}

// CacheRoot returns the user's XDG cache namespace for registry snapshots.
func CacheRoot() (string, error) {
	if cacheHome := os.Getenv("XDG_CACHE_HOME"); cacheHome != "" {
		return filepath.Join(cacheHome, "zenget", "registries"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "zenget", "registries"), nil
}

// NewCache opens the user's registry snapshot cache.
func NewCache() (*Cache, error) {
	root, err := CacheRoot()
	if err != nil {
		return nil, err
	}
	return NewCacheAt(root)
}

// NewCacheAt opens an isolated registry snapshot cache.
func NewCacheAt(root string) (*Cache, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("registry cache root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve registry cache root: %w", err)
	}
	if info, statErr := os.Lstat(absolute); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("registry cache root is not a directory")
		}
		if err := os.Chmod(absolute, 0700); err != nil {
			return nil, fmt.Errorf("secure registry cache root: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect registry cache root: %w", statErr)
	}
	return &Cache{root: absolute}, nil
}

// Root returns the cache root.
func (c *Cache) Root() string {
	if c == nil {
		return ""
	}
	return c.root
}

// Save publishes a complete snapshot. Existing active snapshots remain
// untouched until every new file has been written and validated.
func (c *Cache) Save(source Source, index Index, recipes map[string][]byte) error {
	if c == nil {
		return errors.New("registry cache is nil")
	}
	normalizedSource, err := source.Normalize()
	if err != nil {
		return err
	}
	if err := index.ValidateCanonical(); err != nil {
		return err
	}
	indexData, err := MarshalIndex(index)
	if err != nil {
		return err
	}
	indexDigest := bytesSHA256(indexData)
	if err := validateRecipeSet(index, recipes); err != nil {
		return err
	}
	namespace := c.sourceDir(normalizedSource)
	if err := ensurePrivateDirectory(filepath.Dir(namespace), "registry cache root"); err != nil {
		return err
	}
	if info, statErr := os.Lstat(namespace); statErr == nil {
		if err := validatePrivateDirectory(info, "registry cache namespace"); err != nil {
			return err
		}
	} else if os.IsNotExist(statErr) {
		if err := os.Mkdir(namespace, 0700); err != nil {
			return fmt.Errorf("create registry cache namespace: %w", err)
		}
	} else {
		return fmt.Errorf("inspect registry cache namespace: %w", statErr)
	}
	if err := os.Chmod(namespace, 0700); err != nil {
		return fmt.Errorf("set registry cache namespace permissions: %w", err)
	}
	if err := os.MkdirAll(namespace, 0700); err != nil {
		return fmt.Errorf("create registry cache namespace: %w", err)
	}
	snapshotsDir := filepath.Join(namespace, "snapshots")
	if info, statErr := os.Lstat(snapshotsDir); statErr == nil {
		if err := validatePrivateDirectory(info, "registry snapshot directory"); err != nil {
			return err
		}
	} else if os.IsNotExist(statErr) {
		if err := os.Mkdir(snapshotsDir, 0700); err != nil {
			return fmt.Errorf("create registry snapshot directory: %w", err)
		}
	} else {
		return fmt.Errorf("inspect registry snapshot directory: %w", statErr)
	}
	staging, err := os.MkdirTemp(snapshotsDir, ".staging-")
	if err != nil {
		return fmt.Errorf("create registry snapshot staging directory: %w", err)
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := writeSnapshotFiles(staging, normalizedSource, indexData, indexDigest, index, recipes); err != nil {
		return err
	}
	final := filepath.Join(snapshotsDir, indexDigest)
	reuseExisting := false
	if info, statErr := os.Lstat(final); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("registry snapshot destination is unsafe")
		}
		if snapshotDirectoryMatches(final, normalizedSource, indexData, index, recipes) {
			if err := os.RemoveAll(staging); err != nil {
				return fmt.Errorf("remove duplicate registry snapshot staging: %w", err)
			}
			removeStaging = false
			reuseExisting = true
		} else if err := os.RemoveAll(final); err != nil {
			return fmt.Errorf("replace corrupt registry snapshot: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect registry snapshot destination: %w", statErr)
	}
	if !reuseExisting {
		if err := os.Rename(staging, final); err != nil {
			return fmt.Errorf("publish registry snapshot: %w", err)
		}
		removeStaging = false
	}
	pointer := snapshotPointer{SchemaVersion: SchemaVersion, Source: normalizedSource.String(), IndexSHA256: indexDigest}
	pointerData, err := marshalPointer(pointer)
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(namespace, SnapshotPointerFileName), namespace, ".zenget-registry-current-*", pointerData, 0600, "registry snapshot pointer"); err != nil {
		return err
	}
	return nil
}

// Load validates and loads only the active snapshot. It never scans older
// snapshots, so corruption or a policy revoke cannot silently fall back.
func (c *Cache) Load(source Source) (Snapshot, error) {
	if c == nil {
		return Snapshot{}, errors.New("registry cache is nil")
	}
	normalizedSource, err := source.Normalize()
	if err != nil {
		return Snapshot{}, err
	}
	if err := authorizeSource(normalizedSource, IndexFileName); err != nil {
		return Snapshot{}, err
	}
	namespace := c.sourceDir(normalizedSource)
	if err := ensurePrivateDirectory(filepath.Dir(namespace), "registry cache root"); err != nil {
		return Snapshot{}, err
	}
	if info, statErr := os.Lstat(namespace); statErr == nil {
		if err := validatePrivateDirectory(info, "registry cache namespace"); err != nil {
			return Snapshot{}, err
		}
	} else if os.IsNotExist(statErr) {
		return Snapshot{}, ErrSnapshotNotFound
	} else {
		return Snapshot{}, fmt.Errorf("inspect registry cache namespace: %w", statErr)
	}
	snapshotsDirectory := filepath.Join(namespace, "snapshots")
	if info, statErr := os.Lstat(snapshotsDirectory); statErr == nil {
		if err := validatePrivateDirectory(info, "registry snapshot directory"); err != nil {
			return Snapshot{}, err
		}
	} else if os.IsNotExist(statErr) {
		return Snapshot{}, ErrSnapshotNotFound
	} else {
		return Snapshot{}, fmt.Errorf("inspect registry snapshot directory: %w", statErr)
	}
	pointerPath := filepath.Join(namespace, SnapshotPointerFileName)
	info, err := os.Lstat(pointerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{}, ErrSnapshotNotFound
		}
		return Snapshot{}, fmt.Errorf("inspect registry snapshot pointer: %w", err)
	}
	if err := validatePrivateFile(info, "registry snapshot pointer"); err != nil {
		return Snapshot{}, err
	}
	pointerData, err := limits.ReadFile(pointerPath, limits.DefaultStructuredBytes, "registry snapshot pointer")
	if err != nil {
		return Snapshot{}, fmt.Errorf("read registry snapshot pointer: %w", err)
	}
	pointer, err := decodePointer(pointerData)
	if err != nil {
		return Snapshot{}, err
	}
	if pointer.Source != normalizedSource.String() {
		return Snapshot{}, errors.New("registry snapshot source does not match configuration")
	}
	if !digestPattern.MatchString(pointer.IndexSHA256) {
		return Snapshot{}, errors.New("registry snapshot pointer contains an invalid index digest")
	}
	snapshotDir := filepath.Join(namespace, "snapshots", pointer.IndexSHA256)
	return loadSnapshotDirectory(snapshotDir, normalizedSource, pointer.IndexSHA256)
}

// Lookup loads the authorized active snapshot and resolves exactly one recipe
// by repository. Missing or malformed snapshots are hard errors.
func (c *Cache) Lookup(source Source, repository string) (IndexEntry, []byte, error) {
	snapshot, err := c.Load(source)
	if err != nil {
		return IndexEntry{}, nil, err
	}
	entry, data, ok := snapshot.Recipe(repository)
	if !ok {
		return IndexEntry{}, nil, fmt.Errorf("%w: %q", ErrRecipeNotFound, repository)
	}
	return entry, data, nil
}

func (c *Cache) sourceDir(source Source) string {
	digest := sha256.Sum256([]byte(source.String()))
	return filepath.Join(c.root, hex.EncodeToString(digest[:]))
}

type snapshotPointer struct {
	SchemaVersion int    `json:"schema_version"`
	Source        string `json:"source"`
	IndexSHA256   string `json:"index_sha256"`
}

func writeSnapshotFiles(directory string, source Source, indexData []byte, indexDigest string, index Index, recipes map[string][]byte) error {
	metadata := snapshotPointer{SchemaVersion: SchemaVersion, Source: source.String(), IndexSHA256: indexDigest}
	metadataData, err := marshalPointer(metadata)
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(directory, "snapshot.json"), metadataData, 0600, "registry snapshot metadata"); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(directory, "index.json"), indexData, 0600, "registry snapshot index"); err != nil {
		return err
	}
	recipesDirectory := filepath.Join(directory, "recipes")
	if err := os.Mkdir(recipesDirectory, 0700); err != nil {
		return fmt.Errorf("create registry recipe directory: %w", err)
	}
	if err := os.Chmod(recipesDirectory, 0700); err != nil {
		return fmt.Errorf("set registry recipe directory permissions: %w", err)
	}
	for _, entry := range index.Entries {
		digest := entry.SHA256
		filePath := filepath.Join(recipesDirectory, digest+".json")
		if existing, readErr := os.ReadFile(filePath); readErr == nil {
			if !bytes.Equal(existing, recipes[entry.Path]) {
				return fmt.Errorf("registry recipe digest collision for %q", entry.Repository)
			}
			continue
		} else if !os.IsNotExist(readErr) {
			return fmt.Errorf("inspect registry recipe snapshot: %w", readErr)
		}
		if err := writeFileSync(filePath, recipes[entry.Path], 0600, "registry recipe snapshot"); err != nil {
			return err
		}
	}
	return nil
}

func snapshotDirectoryMatches(directory string, source Source, indexData []byte, index Index, recipes map[string][]byte) bool {
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false
	}
	if err := validatePrivateDirectory(info, "existing registry snapshot"); err != nil {
		return false
	}
	rootEntries, err := os.ReadDir(directory)
	if err != nil || len(rootEntries) != 3 {
		return false
	}
	for _, entry := range rootEntries {
		if entry.Name() != "snapshot.json" && entry.Name() != "index.json" && entry.Name() != "recipes" {
			return false
		}
		entryInfo, entryErr := os.Lstat(filepath.Join(directory, entry.Name()))
		if entryErr != nil || entryInfo.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	metadata, err := marshalPointer(snapshotPointer{SchemaVersion: SchemaVersion, Source: source.String(), IndexSHA256: bytesSHA256(indexData)})
	if err != nil {
		return false
	}
	if cached, err := readPrivateFile(filepath.Join(directory, "snapshot.json"), "existing registry snapshot metadata"); err != nil || !bytes.Equal(cached, metadata) {
		return false
	}
	if cached, err := readPrivateFile(filepath.Join(directory, "index.json"), "existing registry snapshot index"); err != nil || !bytes.Equal(cached, indexData) {
		return false
	}
	recipesDirectory := filepath.Join(directory, "recipes")
	recipeInfo, err := os.Lstat(recipesDirectory)
	if err != nil || recipeInfo.Mode()&os.ModeSymlink != 0 || !recipeInfo.IsDir() {
		return false
	}
	if err := validatePrivateDirectory(recipeInfo, "existing registry recipe directory"); err != nil {
		return false
	}
	entries, err := os.ReadDir(recipesDirectory)
	if err != nil || len(entries) != len(index.Entries) {
		return false
	}
	expectedFiles := make(map[string]struct{}, len(index.Entries))
	for _, entry := range index.Entries {
		fileName := entry.SHA256 + ".json"
		expectedFiles[fileName] = struct{}{}
		cached, err := readPrivateFile(filepath.Join(recipesDirectory, fileName), "existing registry recipe snapshot")
		if err != nil || !bytes.Equal(cached, recipes[entry.Path]) {
			return false
		}
	}
	for _, entry := range entries {
		if _, ok := expectedFiles[entry.Name()]; !ok {
			return false
		}
		info, err := os.Lstat(filepath.Join(recipesDirectory, entry.Name()))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func loadSnapshotDirectory(directory string, source Source, expectedDigest string) (Snapshot, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{}, errors.New("registry snapshot target is missing")
		}
		return Snapshot{}, fmt.Errorf("inspect registry snapshot: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Snapshot{}, errors.New("registry snapshot target is not a directory")
	}
	if err := validatePrivateDirectory(info, "registry snapshot target"); err != nil {
		return Snapshot{}, err
	}
	rootEntries, err := os.ReadDir(directory)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list registry snapshot: %w", err)
	}
	for _, entry := range rootEntries {
		if entry.Name() != "snapshot.json" && entry.Name() != "index.json" && entry.Name() != "recipes" {
			return Snapshot{}, errors.New("registry snapshot contains an unexpected file")
		}
		info, err := os.Lstat(filepath.Join(directory, entry.Name()))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return Snapshot{}, errors.New("registry snapshot contains an unsafe entry")
		}
	}
	metadataData, err := readPrivateFile(filepath.Join(directory, "snapshot.json"), "registry snapshot metadata")
	if err != nil {
		return Snapshot{}, err
	}
	metadata, err := decodePointer(metadataData)
	if err != nil {
		return Snapshot{}, err
	}
	if metadata.Source != source.String() || metadata.IndexSHA256 != expectedDigest {
		return Snapshot{}, errors.New("registry snapshot metadata does not match the active pointer")
	}
	indexData, err := readPrivateFile(filepath.Join(directory, "index.json"), "registry snapshot index")
	if err != nil {
		return Snapshot{}, err
	}
	index, err := DecodeIndex(indexData)
	if err != nil {
		return Snapshot{}, fmt.Errorf("validate registry snapshot index: %w", err)
	}
	canonicalIndex, err := MarshalIndex(index)
	if err != nil {
		return Snapshot{}, err
	}
	if bytesSHA256(canonicalIndex) != expectedDigest || !bytes.Equal(canonicalIndex, indexData) {
		return Snapshot{}, errors.New("registry snapshot index digest is invalid")
	}
	recipesDirectory := filepath.Join(directory, "recipes")
	recipeInfo, err := os.Lstat(recipesDirectory)
	if err != nil {
		return Snapshot{}, errors.New("registry snapshot recipe directory is missing")
	}
	if recipeInfo.Mode()&os.ModeSymlink != 0 || !recipeInfo.IsDir() {
		return Snapshot{}, errors.New("registry snapshot recipe directory is unsafe")
	}
	recipes := make(map[string][]byte, len(index.Entries))
	expectedFiles := make(map[string]struct{}, len(index.Entries))
	for _, entry := range index.Entries {
		if err := authorizeSource(source, entry.Path); err != nil {
			return Snapshot{}, err
		}
		fileName := entry.SHA256 + ".json"
		expectedFiles[fileName] = struct{}{}
		data, err := readPrivateFile(filepath.Join(recipesDirectory, fileName), "registry recipe snapshot")
		if err != nil {
			return Snapshot{}, err
		}
		if bytesSHA256(data) != entry.SHA256 {
			return Snapshot{}, fmt.Errorf("registry recipe %q has an invalid SHA-256", entry.Repository)
		}
		parsed, err := recipe.Decode(data, "registry recipe")
		if err != nil {
			return Snapshot{}, fmt.Errorf("validate cached recipe %q: %w", entry.Repository, err)
		}
		if parsed.Repository != entry.Repository {
			return Snapshot{}, fmt.Errorf("cached recipe repository %q does not match index repository %q", parsed.Repository, entry.Repository)
		}
		recipes[entry.Path] = append([]byte(nil), data...)
	}
	entries, err := os.ReadDir(recipesDirectory)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list registry snapshot recipes: %w", err)
	}
	for _, entry := range entries {
		if _, ok := expectedFiles[entry.Name()]; !ok {
			return Snapshot{}, errors.New("registry snapshot contains an unexpected recipe file")
		}
		info, err := os.Lstat(filepath.Join(recipesDirectory, entry.Name()))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return Snapshot{}, errors.New("registry snapshot contains an unsafe recipe file")
		}
	}
	return Snapshot{Source: source, Index: index, IndexSHA256: expectedDigest, Recipes: recipes}, nil
}

func validateRecipeSet(index Index, recipes map[string][]byte) error {
	if len(recipes) != len(index.Entries) {
		return errors.New("registry snapshot recipe set does not match index")
	}
	for _, entry := range index.Entries {
		data, ok := recipes[entry.Path]
		if !ok {
			return fmt.Errorf("registry snapshot is missing recipe %q", entry.Repository)
		}
		if bytesSHA256(data) != entry.SHA256 {
			return fmt.Errorf("recipe %q SHA-256 does not match registry index", entry.Repository)
		}
		parsed, err := recipe.Decode(data, "registry recipe")
		if err != nil {
			return fmt.Errorf("validate recipe %q: %w", entry.Repository, err)
		}
		if parsed.Repository != entry.Repository {
			return fmt.Errorf("recipe repository %q does not match index repository %q", parsed.Repository, entry.Repository)
		}
	}
	return nil
}

func authorizeSource(source Source, relative string) error {
	identity, err := source.PolicyIdentity(relative)
	if err != nil {
		return err
	}
	decision := recipepolicy.CheckSource(identity)
	if !decision.Allowed {
		return fmt.Errorf("registry source policy denied %s: %s", source.String(), decision.Reason)
	}
	return nil
}

func decodePointer(data []byte) (snapshotPointer, error) {
	var pointer snapshotPointer
	object, err := decodeObject(data, &pointer, "registry snapshot metadata")
	if err != nil {
		return snapshotPointer{}, err
	}
	for _, key := range []string{"schema_version", "source", "index_sha256"} {
		if _, ok := object[key]; !ok {
			return snapshotPointer{}, fmt.Errorf("registry snapshot metadata is missing %s", key)
		}
	}
	if pointer.SchemaVersion != SchemaVersion {
		return snapshotPointer{}, fmt.Errorf("unsupported registry snapshot schema_version %d; want %d", pointer.SchemaVersion, SchemaVersion)
	}
	if _, err := ParseSource(pointer.Source); err != nil {
		return snapshotPointer{}, fmt.Errorf("registry snapshot metadata source: %w", err)
	}
	if !digestPattern.MatchString(pointer.IndexSHA256) {
		return snapshotPointer{}, errors.New("registry snapshot metadata has an invalid index digest")
	}
	return pointer, nil
}

func decodeObject(data []byte, destination any, kind string) (map[string]json.RawMessage, error) {
	var checked json.RawMessage
	if err := limits.DecodeJSON(bytes.NewReader(data), &checked, limits.DefaultStructuredBytes, kind, kind); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return nil, fmt.Errorf("decode %s: %w", kind, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode %s: multiple JSON values", kind)
		}
		return nil, fmt.Errorf("decode %s: trailing data: %w", kind, err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("decode %s: %w", kind, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", kind)
	}
	return object, nil
}

func normalizeRelativePath(value string, emptyAllowed bool) (string, error) {
	if value == "" {
		if emptyAllowed {
			return "", nil
		}
		return "", errors.New("relative path must not be empty")
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") || strings.ContainsAny(value, "\\%?#:\x00") {
		return "", errors.New("path must be a clean relative path")
	}
	components := strings.Split(value, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("path dot-segments and empty components are not allowed")
		}
		for _, char := range component {
			if char < 0x20 || char == 0x7f || char > 0x7f || strings.ContainsRune("*?[](){}|^$+", char) {
				return "", errors.New("path contains an unsupported character")
			}
		}
	}
	return value, nil
}

func bytesSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// SHA256 returns the lowercase SHA-256 digest used by registry indexes and
// snapshots.
func SHA256(data []byte) string {
	return bytesSHA256(data)
}

func ensurePrivateDirectory(directory, kind string) error {
	if info, err := os.Lstat(directory); err == nil {
		return validatePrivateDirectory(info, kind)
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return fmt.Errorf("create %s: %w", kind, err)
		}
		info, statErr := os.Lstat(directory)
		if statErr != nil {
			return fmt.Errorf("inspect %s: %w", kind, statErr)
		}
		return validatePrivateDirectory(info, kind)
	} else {
		return fmt.Errorf("inspect %s: %w", kind, err)
	}
}

func validatePrivateDirectory(info os.FileInfo, kind string) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a safe directory", kind)
	}
	if info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%s must not be writable by group or other users", kind)
	}
	return nil
}

func validatePrivateFile(info os.FileInfo, kind string) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to use %s through symlink", kind)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", kind)
	}
	if info.Mode().Perm()&0077 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("%s permissions must be no more permissive than 0600", kind)
	}
	if !fileowner.CurrentUserOwns(info) {
		return fmt.Errorf("%s is not owned by the current user", kind)
	}
	return nil
}

func readPrivateFile(filePath, kind string) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s is missing", kind)
		}
		return nil, fmt.Errorf("inspect %s: %w", kind, err)
	}
	if err := validatePrivateFile(info, kind); err != nil {
		return nil, err
	}
	data, err := limits.ReadFile(filePath, limits.DefaultStructuredBytes, kind)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", kind, err)
	}
	return data, nil
}

func writeFileSync(filePath string, data []byte, mode os.FileMode, kind string) error {
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", kind, err)
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(filePath)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("set %s permissions: %w", kind, err)
	}
	if n, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", kind, err)
	} else if n != len(data) {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", kind, io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", kind, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", kind, err)
	}
	remove = false
	return nil
}

func atomicWrite(destination, directory, pattern string, data []byte, mode os.FileMode, kind string) error {
	temporary, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", kind, err)
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary %s permissions: %w", kind, err)
	}
	if n, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary %s: %w", kind, err)
	} else if n != len(data) {
		_ = temporary.Close()
		return fmt.Errorf("write temporary %s: %w", kind, io.ErrShortWrite)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary %s: %w", kind, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary %s: %w", kind, err)
	}
	if info, statErr := os.Lstat(destination); statErr == nil {
		if err := validatePrivateFile(info, kind); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect %s: %w", kind, statErr)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("replace %s: %w", kind, err)
	}
	remove = false
	return nil
}

func marshalPointer(pointer snapshotPointer) ([]byte, error) {
	data, err := json.MarshalIndent(pointer, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal registry snapshot metadata: %w", err)
	}
	return append(data, '\n'), nil
}

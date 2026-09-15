// Package artifactcache stores verified extracted release binaries for reuse.
// Cache entries are immutable files described by an atomically replaced index.
package artifactcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"zenget/internal/hashx"
	"zenget/internal/limits"
	"zenget/internal/wrapper"
)

const (
	// SchemaVersion is the cache index schema supported by this release.
	SchemaVersion = 1

	// DefaultLockTimeout bounds waiting for another process to acquire the
	// repository/release/platform cache lock.
	DefaultLockTimeout = 30 * time.Second

	// DefaultStagingMaxAge is the age after which abandoned staging files are
	// eligible for cleanup by cache gc.
	DefaultStagingMaxAge = 24 * time.Hour
)

// Key identifies one extracted artifact variant. Empty optional fields are
// wildcards when a key is used for Lookup, but published entries always carry
// the asset name and target name.
type Key struct {
	Provider      string `json:"provider"`
	Repository    string `json:"repository"`
	Tag           string `json:"tag"`
	PlatformOS    string `json:"platform_os"`
	PlatformArch  string `json:"platform_arch"`
	PlatformLibc  string `json:"platform_libc"`
	AssetID       int64  `json:"asset_id,omitempty"`
	AssetName     string `json:"asset_name"`
	ArchiveBinary string `json:"archive_binary,omitempty"`
	TargetName    string `json:"target_name"`
}

// Validate checks a key. When allowWildcards is true, optional selector
// fields may be empty for a lookup or protection rule.
func (k Key) Validate(allowWildcards bool) error {
	if strings.TrimSpace(k.Provider) == "" {
		return errors.New("cache key provider is empty")
	}
	if strings.TrimSpace(k.Repository) == "" {
		return errors.New("cache key repository is empty")
	}
	if strings.TrimSpace(k.Tag) == "" {
		return errors.New("cache key tag is empty")
	}
	if strings.TrimSpace(k.PlatformOS) == "" || strings.TrimSpace(k.PlatformArch) == "" {
		return errors.New("cache key platform os/arch must both be set")
	}
	if strings.TrimSpace(k.PlatformLibc) == "" {
		return errors.New("cache key platform libc is empty")
	}
	if !allowWildcards && strings.TrimSpace(k.AssetName) == "" {
		return errors.New("cache key asset name is empty")
	}
	if !allowWildcards && strings.TrimSpace(k.TargetName) == "" {
		return errors.New("cache key target name is empty")
	}
	if strings.ContainsRune(k.Provider, '\x00') || strings.ContainsRune(k.Repository, '\x00') || strings.ContainsRune(k.Tag, '\x00') ||
		strings.ContainsRune(k.PlatformOS, '\x00') || strings.ContainsRune(k.PlatformArch, '\x00') || strings.ContainsRune(k.PlatformLibc, '\x00') ||
		strings.ContainsRune(k.AssetName, '\x00') || strings.ContainsRune(k.ArchiveBinary, '\x00') || strings.ContainsRune(k.TargetName, '\x00') {
		return errors.New("cache key contains NUL")
	}
	return nil
}

// Identity returns a stable human-readable identity for output and tie
// breaking. It intentionally includes every key field.
func (k Key) Identity() string {
	return fmt.Sprintf("%s/%s@%s %s/%s/%s asset=%d:%s binary=%s target=%s",
		k.Provider, k.Repository, k.Tag, k.PlatformOS, k.PlatformArch, k.PlatformLibc,
		k.AssetID, k.AssetName, k.ArchiveBinary, k.TargetName)
}

func (k Key) canonical() string {
	data, _ := json.Marshal(k)
	return string(data)
}

func (k Key) variant() string {
	digest := sha256.Sum256([]byte(k.canonical()))
	return hex.EncodeToString(digest[:])
}

func (k Key) lockVariant() string {
	lockKey := struct {
		Provider     string `json:"provider"`
		Repository   string `json:"repository"`
		Tag          string `json:"tag"`
		PlatformOS   string `json:"platform_os"`
		PlatformArch string `json:"platform_arch"`
		PlatformLibc string `json:"platform_libc"`
	}{k.Provider, k.Repository, k.Tag, k.PlatformOS, k.PlatformArch, k.PlatformLibc}
	data, _ := json.Marshal(lockKey)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Entry is one indexed, verified extracted binary.
type Entry struct {
	Provider          string    `json:"provider"`
	Repository        string    `json:"repository"`
	Tag               string    `json:"tag"`
	PlatformOS        string    `json:"platform_os"`
	PlatformArch      string    `json:"platform_arch"`
	PlatformLibc      string    `json:"platform_libc"`
	AssetID           int64     `json:"asset_id,omitempty"`
	AssetName         string    `json:"asset_name"`
	ArchiveBinary     string    `json:"archive_binary,omitempty"`
	TargetName        string    `json:"target_name"`
	ArchiveSHA256     string    `json:"archive_sha256"`
	BinaryXXH3_64     string    `json:"binary_xxh3_64"`
	BinarySHA256      string    `json:"binary_sha256"`
	BinarySize        int64     `json:"binary_size"`
	RelativePath      string    `json:"relative_path"`
	LastSuccessfulUse time.Time `json:"last_successful_use"`
}

// Key returns the lookup identity represented by the entry.
func (e Entry) Key() Key {
	return Key{
		Provider: e.Provider, Repository: e.Repository, Tag: e.Tag,
		PlatformOS: e.PlatformOS, PlatformArch: e.PlatformArch, PlatformLibc: e.PlatformLibc,
		AssetID: e.AssetID, AssetName: e.AssetName, ArchiveBinary: e.ArchiveBinary, TargetName: e.TargetName,
	}
}

// Identity returns the stable identity of the entry.
func (e Entry) Identity() string { return e.Key().Identity() }

// Index is the atomically persisted cache registry.
type Index struct {
	SchemaVersion int     `json:"schema_version"`
	Entries       []Entry `json:"entries"`
}

// IntegrityStatus describes an indexed artifact's on-disk state.
type IntegrityStatus string

const (
	IntegrityValid   IntegrityStatus = "valid"
	IntegrityMissing IntegrityStatus = "missing"
	IntegrityCorrupt IntegrityStatus = "corrupt"
)

// Inspection is the deterministic cache-list representation of an entry.
type Inspection struct {
	Entry     Entry           `json:"entry"`
	Identity  string          `json:"identity"`
	Integrity IntegrityStatus `json:"integrity"`
	Size      int64           `json:"size"`
	Reason    string          `json:"reason,omitempty"`
	Protected bool            `json:"protected"`
	Active    bool            `json:"active"`
	Manifest  bool            `json:"manifest"`
}

// Protection identifies artifacts that GC must retain. Active and Manifest
// are separate so output can explain why an entry is protected.
type Protection struct {
	Active   []Key
	Manifest []Key
	Paths    []string
}

func (p Protection) flags(entry Entry, cache *Cache) (active, manifest bool, pathProtected bool) {
	key := entry.Key()
	for _, protected := range p.Active {
		if matches(protected, key) {
			active = true
		}
	}
	for _, protected := range p.Manifest {
		if matches(protected, key) {
			manifest = true
		}
	}
	for _, protectedPath := range p.Paths {
		if expected, err := cache.entryPath(key); err == nil && samePath(expected, protectedPath) {
			pathProtected = true
		}
	}
	return active, manifest, pathProtected
}

func matches(pattern, candidate Key) bool {
	if pattern.Provider != candidate.Provider || pattern.Repository != candidate.Repository || pattern.Tag != candidate.Tag ||
		pattern.PlatformOS != candidate.PlatformOS || pattern.PlatformArch != candidate.PlatformArch {
		return false
	}
	if pattern.PlatformLibc != "" && pattern.PlatformLibc != candidate.PlatformLibc {
		return false
	}
	if pattern.AssetID != 0 && pattern.AssetID != candidate.AssetID {
		return false
	}
	if pattern.AssetName != "" && pattern.AssetName != candidate.AssetName {
		return false
	}
	if pattern.ArchiveBinary != "" && pattern.ArchiveBinary != candidate.ArchiveBinary {
		return false
	}
	if pattern.TargetName != "" && pattern.TargetName != candidate.TargetName {
		return false
	}
	return true
}

// GCOptions controls planning and application of garbage collection.
type GCOptions struct {
	Protection    Protection
	MaxAge        time.Duration
	MaxAgeSet     bool
	MaxBytes      int64
	MaxBytesSet   bool
	StagingMaxAge time.Duration
	DryRun        bool
	Now           func() time.Time
}

// GCPlan is both the dry-run result and a summary of an applied collection.
type GCPlan struct {
	DryRun         bool         `json:"dry_run"`
	DeleteEntries  []Inspection `json:"delete_entries"`
	DeleteStaging  []string     `json:"delete_staging"`
	ReclaimedBytes int64        `json:"reclaimed_bytes"`
	RemainingBytes int64        `json:"remaining_bytes"`
	AgeEnabled     bool         `json:"age_enabled"`
	BytesEnabled   bool         `json:"bytes_enabled"`
}

// Options configures a cache instance.
type Options struct {
	LockTimeout time.Duration
	Now         func() time.Time
}

// Cache is a filesystem-backed artifact cache.
type Cache struct {
	root        string
	indexPath   string
	stagingPath string
	locksPath   string
	lockTimeout time.Duration
	now         func() time.Time
	mu          sync.Mutex
}

// New opens the user's default cache directory.
func New() (*Cache, error) {
	root, err := wrapper.CacheRoot()
	if err != nil {
		return nil, err
	}
	return NewAt(root)
}

// NewAt opens an isolated cache at root.
func NewAt(root string) (*Cache, error) {
	return NewAtWithOptions(root, Options{})
}

// NewAtWithOptions opens an isolated cache with testable timing and lock
// settings.
func NewAtWithOptions(root string, options Options) (*Cache, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("cache root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve cache root %q: %w", root, err)
	}
	if options.LockTimeout <= 0 {
		options.LockTimeout = DefaultLockTimeout
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Cache{
		root: absolute, indexPath: filepath.Join(absolute, "index.json"),
		stagingPath: filepath.Join(absolute, "staging"), locksPath: filepath.Join(absolute, "locks"),
		lockTimeout: options.LockTimeout, now: options.Now,
	}, nil
}

// Root returns the cache root path.
func (c *Cache) Root() string {
	if c == nil {
		return ""
	}
	return c.root
}

// Lookup returns a verified binary for key. Empty optional fields in key are
// wildcards; a wildcard lookup succeeds only when exactly one valid entry
// matches. expectedArchiveSHA256, when non-empty, is also required to match
// the indexed archive digest.
func (c *Cache) Lookup(key Key, expectedArchiveSHA256 string) (*Entry, []byte, bool, error) {
	return c.lookup(key, expectedArchiveSHA256, true)
}

// LookupReadOnly returns a verified binary for key without updating the cache
// usage index. Empty optional fields in key are wildcards; a wildcard lookup
// succeeds only when exactly one valid entry matches. This is intended for
// diagnostics such as `zenget which`, where inspecting state must not mutate
// it.
func (c *Cache) LookupReadOnly(key Key, expectedArchiveSHA256 string) (*Entry, []byte, bool, error) {
	return c.lookup(key, expectedArchiveSHA256, false)
}

func (c *Cache) lookup(key Key, expectedArchiveSHA256 string, touch bool) (*Entry, []byte, bool, error) {
	if c == nil {
		return nil, nil, false, errors.New("cache is nil")
	}
	if err := key.Validate(true); err != nil {
		return nil, nil, false, err
	}
	expectedArchiveSHA256, err := normalizeSHA256(expectedArchiveSHA256)
	if err != nil {
		return nil, nil, false, fmt.Errorf("validate expected archive SHA-256: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	index, err := c.loadIndex()
	if err != nil {
		return nil, nil, false, err
	}
	var matchesFound []struct {
		index int
		entry Entry
		data  []byte
	}
	for indexPosition, entry := range index.Entries {
		candidate := entry.Key()
		if !matches(key, candidate) || (expectedArchiveSHA256 != "" && !strings.EqualFold(entry.ArchiveSHA256, expectedArchiveSHA256)) {
			continue
		}
		data, status, reason := c.readVerified(entry)
		if status != IntegrityValid {
			_ = reason
			continue
		}
		matchesFound = append(matchesFound, struct {
			index int
			entry Entry
			data  []byte
		}{indexPosition, entry, data})
	}
	if len(matchesFound) != 1 {
		return nil, nil, false, nil
	}

	hit := matchesFound[0]
	if !touch {
		return &hit.entry, hit.data, true, nil
	}
	hit.entry.LastSuccessfulUse = c.now().UTC()
	index.Entries[hit.index] = hit.entry
	if err := c.saveIndex(index); err != nil {
		return nil, nil, false, fmt.Errorf("touch cache entry %q: %w", hit.entry.Identity(), err)
	}
	return &hit.entry, hit.data, true, nil
}

// Acquire obtains the repository/release/platform lock used while resolving
// and publishing one artifact. Call Release when the operation completes.
func (c *Cache) Acquire(ctx context.Context, key Key) (*Lock, error) {
	if c == nil {
		return nil, errors.New("cache is nil")
	}
	if err := key.Validate(true); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(c.locksPath, 0755); err != nil {
		return nil, fmt.Errorf("create cache lock directory: %w", err)
	}
	path := filepath.Join(c.locksPath, key.lockVariant()+".lock")
	deadline := time.NewTimer(c.lockTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, _ = io.WriteString(file, fmt.Sprintf("pid=%d\n", os.Getpid()))
			_ = file.Close()
			return &Lock{path: path}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create cache lock %q: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for cache lock %q: %w", key.Identity(), ctx.Err())
		case <-deadline.C:
			return nil, fmt.Errorf("cache lock timeout for %q", key.Identity())
		case <-ticker.C:
		}
	}
}

// Lock is an acquired cache lock.
type Lock struct {
	path string
	once sync.Once
}

// Release releases the lock. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	var err error
	l.once.Do(func() { err = os.Remove(l.path) })
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Publish stores a successfully extracted binary after obtaining the key
// lock. The artifact is written to staging, fsynced, renamed into its final
// immutable path, and then indexed atomically.
func (c *Cache) Publish(ctx context.Context, key Key, archiveSHA256 string, binary []byte) (Entry, error) {
	lock, err := c.Acquire(ctx, key)
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = lock.Release() }()
	return c.PublishLocked(key, archiveSHA256, binary)
}

// PublishLocked is Publish for callers that already hold Acquire's lock.
func (c *Cache) PublishLocked(key Key, archiveSHA256 string, binary []byte) (Entry, error) {
	if c == nil {
		return Entry{}, errors.New("cache is nil")
	}
	if err := key.Validate(false); err != nil {
		return Entry{}, err
	}
	archiveSHA256, err := normalizeSHA256(archiveSHA256)
	if err != nil {
		return Entry{}, fmt.Errorf("validate archive SHA-256: %w", err)
	}
	if len(binary) == 0 {
		return Entry{}, errors.New("cannot cache an empty binary")
	}

	entry := Entry{
		Provider: key.Provider, Repository: key.Repository, Tag: key.Tag,
		PlatformOS: key.PlatformOS, PlatformArch: key.PlatformArch, PlatformLibc: key.PlatformLibc,
		AssetID: key.AssetID, AssetName: key.AssetName, ArchiveBinary: key.ArchiveBinary, TargetName: key.TargetName,
		ArchiveSHA256: archiveSHA256, BinaryXXH3_64: hashx.Bytes(binary), BinarySHA256: bytesSHA256(binary),
		BinarySize: int64(len(binary)), LastSuccessfulUse: c.now().UTC(),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	index, err := c.loadIndex()
	if err != nil {
		return Entry{}, err
	}
	for position, existing := range index.Entries {
		if existing.Key() != key {
			continue
		}
		if existing.ArchiveSHA256 == entry.ArchiveSHA256 && existing.BinaryXXH3_64 == entry.BinaryXXH3_64 &&
			existing.BinarySHA256 == entry.BinarySHA256 && existing.BinarySize == entry.BinarySize {
			if _, status, _ := c.readVerified(existing); status == IntegrityValid {
				existing.LastSuccessfulUse = entry.LastSuccessfulUse
				index.Entries[position] = existing
				if err := c.saveIndex(index); err != nil {
					return Entry{}, fmt.Errorf("touch existing cache entry: %w", err)
				}
				return existing, nil
			}
		}
		break
	}

	finalPath, err := c.entryPath(key)
	if err != nil {
		return Entry{}, err
	}
	if err := c.publishArtifact(finalPath, binary); err != nil {
		return Entry{}, err
	}
	relative, err := filepath.Rel(c.root, finalPath)
	if err != nil {
		return Entry{}, fmt.Errorf("relativize cache artifact: %w", err)
	}
	entry.RelativePath = filepath.ToSlash(relative)

	replaced := false
	for position, existing := range index.Entries {
		if existing.Key() == key {
			index.Entries[position] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		index.Entries = append(index.Entries, entry)
	}
	canonicalSort(index.Entries)
	if err := c.saveIndex(index); err != nil {
		return Entry{}, fmt.Errorf("publish cache index: %w", err)
	}
	return entry, nil
}

// List inspects all indexed entries and marks state/manifest protections.
func (c *Cache) List(protection Protection) ([]Inspection, error) {
	if c == nil {
		return nil, errors.New("cache is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	index, err := c.loadIndex()
	if err != nil {
		return nil, err
	}
	return c.listIndex(index, protection), nil
}

// GC plans and, unless DryRun is set, applies safe cache collection.
func (c *Cache) GC(ctx context.Context, options GCOptions) (GCPlan, error) {
	if c == nil {
		return GCPlan{}, errors.New("cache is nil")
	}
	if options.MaxAgeSet && options.MaxAge < 0 {
		return GCPlan{}, errors.New("cache max age cannot be negative")
	}
	if options.MaxBytesSet && options.MaxBytes < 0 {
		return GCPlan{}, errors.New("cache max bytes cannot be negative")
	}
	if options.StagingMaxAge <= 0 {
		options.StagingMaxAge = DefaultStagingMaxAge
	}
	if options.Now == nil {
		options.Now = c.now
	}
	if ctx == nil {
		ctx = context.Background()
	}
	gcKey := Key{Provider: "gc", Repository: "zenget/cache", Tag: "gc", PlatformOS: "all", PlatformArch: "all", PlatformLibc: "unknown"}
	lock, err := c.Acquire(ctx, gcKey)
	if err != nil {
		return GCPlan{}, err
	}
	defer func() { _ = lock.Release() }()

	c.mu.Lock()
	defer c.mu.Unlock()
	index, err := c.loadIndex()
	if err != nil {
		return GCPlan{}, err
	}
	plan, selected, err := c.planLocked(index, options)
	if err != nil {
		return GCPlan{}, err
	}
	if options.DryRun {
		plan.DryRun = true
		return plan, nil
	}
	for _, inspection := range plan.DeleteEntries {
		if err := c.removeEntry(inspection.Entry); err != nil {
			return GCPlan{}, err
		}
	}
	for _, staging := range plan.DeleteStaging {
		if err := os.RemoveAll(filepath.Join(c.root, filepath.FromSlash(staging))); err != nil {
			return GCPlan{}, fmt.Errorf("remove staging artifact %q: %w", staging, err)
		}
	}
	if len(selected) > 0 {
		kept := make([]Entry, 0, len(index.Entries)-len(selected))
		for _, entry := range index.Entries {
			if _, ok := selected[entry.Identity()]; !ok {
				kept = append(kept, entry)
			}
		}
		index.Entries = kept
		canonicalSort(index.Entries)
		if err := c.saveIndex(index); err != nil {
			return GCPlan{}, fmt.Errorf("save cache index after gc: %w", err)
		}
	}
	return plan, nil
}

func (c *Cache) listIndex(index Index, protection Protection) []Inspection {
	inspections := make([]Inspection, 0, len(index.Entries))
	for _, entry := range index.Entries {
		_, status, reason := c.readVerified(entry)
		active, manifest, pathProtected := protection.flags(entry, c)
		inspections = append(inspections, Inspection{
			Entry: entry, Identity: entry.Identity(), Integrity: status, Size: entry.BinarySize,
			Reason: reason, Protected: active || manifest || pathProtected, Active: active, Manifest: manifest,
		})
	}
	sort.Slice(inspections, func(left, right int) bool { return inspections[left].Identity < inspections[right].Identity })
	return inspections
}

func (c *Cache) planLocked(index Index, options GCOptions) (GCPlan, map[string]struct{}, error) {
	inspections := c.listIndex(index, options.Protection)
	now := options.Now().UTC()
	selected := make(map[string]struct{})
	plan := GCPlan{AgeEnabled: options.MaxAgeSet, BytesEnabled: options.MaxBytesSet}
	for _, inspection := range inspections {
		if inspection.Protected {
			continue
		}
		if inspection.Integrity != IntegrityValid || (options.MaxAgeSet && !inspection.Entry.LastSuccessfulUse.After(now.Add(-options.MaxAge))) {
			selected[inspection.Identity] = struct{}{}
		}
	}
	if options.MaxBytesSet {
		remaining := int64(0)
		for _, inspection := range inspections {
			if _, ok := selected[inspection.Identity]; !ok {
				remaining += inspection.Size
			}
		}
		candidates := make([]Inspection, 0, len(inspections))
		for _, inspection := range inspections {
			if inspection.Protected {
				continue
			}
			if _, ok := selected[inspection.Identity]; ok {
				continue
			}
			candidates = append(candidates, inspection)
		}
		sort.Slice(candidates, func(left, right int) bool {
			if candidates[left].Entry.LastSuccessfulUse.Equal(candidates[right].Entry.LastSuccessfulUse) {
				return candidates[left].Identity < candidates[right].Identity
			}
			return candidates[left].Entry.LastSuccessfulUse.Before(candidates[right].Entry.LastSuccessfulUse)
		})
		for _, candidate := range candidates {
			if remaining <= options.MaxBytes {
				break
			}
			selected[candidate.Identity] = struct{}{}
			remaining -= candidate.Size
		}
	}
	for _, inspection := range inspections {
		if _, ok := selected[inspection.Identity]; ok {
			plan.DeleteEntries = append(plan.DeleteEntries, inspection)
			plan.ReclaimedBytes += inspection.Size
		} else {
			plan.RemainingBytes += inspection.Size
		}
	}
	staging, err := c.stagingCandidates(options.StagingMaxAge, now)
	if err != nil {
		return GCPlan{}, nil, err
	}
	plan.DeleteStaging = staging
	sort.Slice(plan.DeleteEntries, func(left, right int) bool {
		return plan.DeleteEntries[left].Identity < plan.DeleteEntries[right].Identity
	})
	return plan, selected, nil
}

func (c *Cache) stagingCandidates(maxAge time.Duration, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(c.stagingPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list cache staging: %w", err)
	}
	cutoff := now.Add(-maxAge)
	var paths []string
	for _, item := range entries {
		info, err := item.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect cache staging %q: %w", item.Name(), err)
		}
		if info.ModTime().Before(cutoff) {
			paths = append(paths, filepath.ToSlash(filepath.Join("staging", item.Name())))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (c *Cache) entryPath(key Key) (string, error) {
	return wrapper.CacheArtifactPathAt(c.root, key.Provider, key.Repository, key.Tag, key.variant(), key.TargetName)
}

func (c *Cache) readVerified(entry Entry) ([]byte, IntegrityStatus, string) {
	key := entry.Key()
	path, err := c.entryPath(key)
	if err != nil {
		return nil, IntegrityCorrupt, err.Error()
	}
	relative, err := filepath.Rel(c.root, path)
	if err != nil || filepath.ToSlash(relative) != filepath.ToSlash(filepath.FromSlash(entry.RelativePath)) {
		return nil, IntegrityCorrupt, "indexed path does not match cache identity"
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, IntegrityMissing, "artifact file is missing"
		}
		return nil, IntegrityCorrupt, err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, IntegrityCorrupt, "artifact is not a regular file"
	}
	if info.Size() != entry.BinarySize {
		return nil, IntegrityCorrupt, fmt.Sprintf("size is %d, indexed size is %d", info.Size(), entry.BinarySize)
	}
	data, err := limits.ReadFile(path, limits.DefaultBinaryBytes, "cached binary")
	if err != nil {
		return nil, IntegrityCorrupt, err.Error()
	}
	if !strings.EqualFold(bytesSHA256(data), entry.BinarySHA256) {
		return nil, IntegrityCorrupt, "binary SHA-256 does not match index"
	}
	if !strings.EqualFold(hashx.Bytes(data), entry.BinaryXXH3_64) {
		return nil, IntegrityCorrupt, "binary XXH3-64 does not match index"
	}
	return data, IntegrityValid, ""
}

func (c *Cache) publishArtifact(finalPath string, binary []byte) error {
	info, err := os.Lstat(finalPath)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to replace cache symlink %q", finalPath)
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect cache artifact %q: %w", finalPath, err)
	}
	if err := os.MkdirAll(c.stagingPath, 0755); err != nil {
		return fmt.Errorf("create cache staging directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		return fmt.Errorf("create cache artifact directory: %w", err)
	}
	temporary, err := os.CreateTemp(c.stagingPath, "artifact-*")
	if err != nil {
		return fmt.Errorf("create cache staging artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if n, err := temporary.Write(binary); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write cache staging artifact: %w", err)
	} else if n != len(binary) {
		_ = temporary.Close()
		return fmt.Errorf("write cache staging artifact: %w", io.ErrShortWrite)
	}
	if err := temporary.Chmod(0755); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set cache artifact permissions: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync cache staging artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close cache staging artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		if removeErr := os.Remove(finalPath); removeErr != nil {
			return fmt.Errorf("publish cache artifact: %w", err)
		}
		if renameErr := os.Rename(temporaryPath, finalPath); renameErr != nil {
			return fmt.Errorf("publish cache artifact: %w", renameErr)
		}
	}
	removeTemporary = false
	return nil
}

func (c *Cache) removeEntry(entry Entry) error {
	path, err := c.entryPath(entry.Key())
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect cache artifact for removal %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove cache symlink %q", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove non-regular cache artifact %q", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove cache artifact %q: %w", path, err)
	}
	return nil
}

func (c *Cache) loadIndex() (Index, error) {
	data, err := limits.ReadFile(c.indexPath, limits.DefaultStructuredBytes, "cache index")
	if os.IsNotExist(err) {
		return Index{SchemaVersion: SchemaVersion, Entries: make([]Entry, 0)}, nil
	}
	if err != nil {
		return Index{}, fmt.Errorf("read cache index %q: %w", c.indexPath, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var index Index
	if err := decoder.Decode(&index); err != nil {
		return Index{}, fmt.Errorf("decode cache index %q: %w", c.indexPath, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Index{}, fmt.Errorf("decode cache index %q: multiple JSON values", c.indexPath)
		}
		return Index{}, fmt.Errorf("decode cache index %q: trailing data: %w", c.indexPath, err)
	}
	if index.SchemaVersion != SchemaVersion {
		return Index{}, fmt.Errorf("unsupported cache index schema_version %d; want %d", index.SchemaVersion, SchemaVersion)
	}
	if index.Entries == nil {
		index.Entries = make([]Entry, 0)
	}
	seen := make(map[string]struct{}, len(index.Entries))
	for _, entry := range index.Entries {
		if err := entry.Key().Validate(false); err != nil {
			return Index{}, fmt.Errorf("invalid cache index entry %q: %w", entry.Identity(), err)
		}
		if _, ok := seen[entry.Identity()]; ok {
			return Index{}, fmt.Errorf("duplicate cache index entry %q", entry.Identity())
		}
		seen[entry.Identity()] = struct{}{}
		if _, err := normalizeSHA256(entry.ArchiveSHA256); err != nil {
			return Index{}, fmt.Errorf("invalid archive digest for %q: %w", entry.Identity(), err)
		}
		if _, err := normalizeSHA256(entry.BinarySHA256); err != nil {
			return Index{}, fmt.Errorf("invalid binary digest for %q: %w", entry.Identity(), err)
		}
		if entry.BinaryXXH3_64 == "" || entry.BinarySize < 0 || entry.RelativePath == "" {
			return Index{}, fmt.Errorf("incomplete cache index entry %q", entry.Identity())
		}
	}
	canonicalSort(index.Entries)
	return index, nil
}

func (c *Cache) saveIndex(index Index) error {
	index.SchemaVersion = SchemaVersion
	if index.Entries == nil {
		index.Entries = make([]Entry, 0)
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache index: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(c.root, 0755); err != nil {
		return fmt.Errorf("create cache root: %w", err)
	}
	temporary, err := os.CreateTemp(c.root, ".index-*")
	if err != nil {
		return fmt.Errorf("create temporary cache index: %w", err)
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
		return fmt.Errorf("write temporary cache index: %w", err)
	} else if n != len(data) {
		_ = temporary.Close()
		return fmt.Errorf("write temporary cache index: %w", io.ErrShortWrite)
	}
	if err := temporary.Chmod(0644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set cache index permissions: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary cache index: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary cache index: %w", err)
	}
	if err := os.Rename(temporaryPath, c.indexPath); err != nil {
		return fmt.Errorf("replace cache index: %w", err)
	}
	removeTemporary = false
	return nil
}

func canonicalSort(entries []Entry) {
	sort.Slice(entries, func(left, right int) bool { return entries[left].Identity() < entries[right].Identity() })
}

func normalizeSHA256(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(strings.ToLower(value), "sha256:") {
		value = value[len("sha256:"):]
	}
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("want 64 hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("invalid hexadecimal digest: %w", err)
	}
	return hex.EncodeToString(decoded), nil
}

func bytesSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func samePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

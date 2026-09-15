package artifactcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKey(name string) Key {
	return Key{
		Provider: "github", Repository: "acme/tool", Tag: "v1.0.0",
		PlatformOS: "linux", PlatformArch: "amd64", PlatformLibc: "glibc",
		AssetID: 42, AssetName: name, ArchiveBinary: "tool", TargetName: "tool",
	}
}

func digest(data []byte) string {
	value := sha256.Sum256(data)
	return hex.EncodeToString(value[:])
}

func newTestCache(t *testing.T) *Cache {
	t.Helper()
	cache, err := NewAtWithOptions(filepath.Join(t.TempDir(), "cache"), Options{
		Now: func() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func TestNewUsesDefaultRootAndNilRootIsEmpty(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	cache, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataHome, "zenget", "cache"); cache.Root() != want {
		t.Fatalf("New().Root() = %q, want %q", cache.Root(), want)
	}
	var nilCache *Cache
	if got := nilCache.Root(); got != "" {
		t.Fatalf("nil Cache.Root() = %q, want empty", got)
	}
}

func TestPublishLookupAndReuse(t *testing.T) {
	cache := newTestCache(t)
	key := testKey("tool.tar.gz")
	binary := []byte("#!/bin/sh\necho ok\n")
	entry, err := cache.Publish(context.Background(), key, digest([]byte("archive")), binary)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if entry.BinarySize != int64(len(binary)) || entry.RelativePath == "" {
		t.Fatalf("Publish() entry = %+v, want size and relative path", entry)
	}
	got, gotBinary, hit, err := cache.Lookup(key, entry.ArchiveSHA256)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if !hit || got == nil || string(gotBinary) != string(binary) {
		t.Fatalf("Lookup() = (%+v, %q, %t), want cache hit", got, gotBinary, hit)
	}
	if got.LastSuccessfulUse.IsZero() {
		t.Error("Lookup() did not retain last successful use timestamp")
	}

	entries, err := cache.List(Protection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Integrity != IntegrityValid {
		t.Fatalf("List() = %+v, want one valid entry", entries)
	}
	if _, err := os.Stat(filepath.Join(cache.Root(), entry.RelativePath)); err != nil {
		t.Fatalf("published artifact missing: %v", err)
	}

	if _, err := cache.Publish(context.Background(), key, entry.ArchiveSHA256, binary); err != nil {
		t.Fatalf("re-Publish() error = %v", err)
	}
	entries, err = cache.List(Protection{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("List() after reuse = (%+v, %v)", entries, err)
	}
}

func TestLookupReadOnlyDoesNotTouchUsageIndex(t *testing.T) {
	cache := newTestCache(t)
	key := testKey("tool.tar.gz")
	entry, err := cache.Publish(context.Background(), key, digest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	before := entry.LastSuccessfulUse

	got, gotBinary, hit, err := cache.LookupReadOnly(key, entry.ArchiveSHA256)
	if err != nil {
		t.Fatalf("LookupReadOnly() error = %v", err)
	}
	if !hit || got == nil || string(gotBinary) != "binary" {
		t.Fatalf("LookupReadOnly() = (%+v, %q, %t), want cache hit", got, gotBinary, hit)
	}
	if !got.LastSuccessfulUse.Equal(before) {
		t.Fatalf("LookupReadOnly() timestamp = %v, want unchanged %v", got.LastSuccessfulUse, before)
	}

	entries, err := cache.List(Protection{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 1 || !entries[0].Entry.LastSuccessfulUse.Equal(before) {
		t.Fatalf("cache usage index changed after read-only lookup: %+v", entries)
	}
}

func TestLookupRejectsCorruptedArtifact(t *testing.T) {
	cache := newTestCache(t)
	key := testKey("tool.tar.gz")
	entry, err := cache.Publish(context.Background(), key, digest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root(), entry.RelativePath), []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, hit, err := cache.Lookup(key, entry.ArchiveSHA256); err != nil {
		t.Fatalf("Lookup() error = %v", err)
	} else if hit {
		t.Fatal("Lookup() hit corrupted artifact")
	}
	entries, err := cache.List(Protection{})
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Integrity != IntegrityCorrupt {
		t.Fatalf("Integrity = %q, want corrupt", entries[0].Integrity)
	}
	if _, err := cache.Publish(context.Background(), key, entry.ArchiveSHA256, []byte("repaired")); err != nil {
		t.Fatalf("Publish() repair error = %v", err)
	}
}

func TestLookupWildcardRequiresUniqueEntry(t *testing.T) {
	cache := newTestCache(t)
	first := testKey("one.tar.gz")
	second := testKey("two.tar.gz")
	second.ArchiveBinary = "other"
	if _, err := cache.Publish(context.Background(), first, digest([]byte("one")), []byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Publish(context.Background(), second, digest([]byte("two")), []byte("two")); err != nil {
		t.Fatal(err)
	}
	wildcard := first
	wildcard.AssetName = ""
	wildcard.ArchiveBinary = ""
	wildcard.TargetName = ""
	if _, _, hit, err := cache.Lookup(wildcard, ""); err != nil {
		t.Fatal(err)
	} else if hit {
		t.Fatal("ambiguous wildcard lookup returned a hit")
	}
}

func TestLoadIndexRejectsUnknownFields(t *testing.T) {
	cache := newTestCache(t)
	data := []byte(`{"schema_version":1,"entries":[],"unknown":true}`)
	if err := os.MkdirAll(cache.Root(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root(), "index.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(Protection{}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("List() error = %v, want unknown-field error", err)
	}
}

func TestAcquireTimesOutAndReleaseUnblocks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	first, err := NewAtWithOptions(root, Options{LockTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAtWithOptions(root, Options{LockTimeout: 35 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	key := testKey("tool.tar.gz")
	lock, err := first.Acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Acquire(context.Background(), key); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Acquire() error = %v, want timeout", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	lock, err = second.Acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("Acquire() after release error = %v", err)
	}
	_ = lock.Release()
}

func TestGCProtectsEntriesAndHonorsLRU(t *testing.T) {
	cache := newTestCache(t)
	old := testKey("old.tar.gz")
	newer := testKey("new.tar.gz")
	newer.ArchiveBinary = "new"
	protected := testKey("protected.tar.gz")
	protected.ArchiveBinary = "protected"
	for _, item := range []struct {
		key  Key
		data []byte
	}{
		{old, []byte("old")}, {newer, []byte("newer")}, {protected, []byte("protected")},
	} {
		if _, err := cache.Publish(context.Background(), item.key, digest(item.data), item.data); err != nil {
			t.Fatal(err)
		}
	}
	protection := Protection{Active: []Key{protected}}
	plan, err := cache.GC(context.Background(), GCOptions{DryRun: true, MaxBytesSet: true, MaxBytes: 7, Protection: protection})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || len(plan.DeleteEntries) != 2 || plan.DeleteEntries[0].Identity > plan.DeleteEntries[1].Identity {
		t.Fatalf("GC dry-run plan = %+v, want two deterministic deletions", plan)
	}
	if _, err := cache.GC(context.Background(), GCOptions{MaxBytesSet: true, MaxBytes: 7, Protection: protection}); err != nil {
		t.Fatal(err)
	}
	entries, err := cache.List(protection)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Protected {
		t.Fatalf("entries after GC = %+v, want only protected entry", entries)
	}
}

func TestGCDefaultRemovesCorruptionAndOldStagingOnly(t *testing.T) {
	cache := newTestCache(t)
	key := testKey("tool.tar.gz")
	entry, err := cache.Publish(context.Background(), key, digest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root(), entry.RelativePath), []byte("bad"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache.Root(), "staging"), 0755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(cache.Root(), "staging", "orphan")
	if err := os.WriteFile(orphan, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	// The test cache injects a fixture clock pinned to 2026-09-07, so the
	// orphan must be aged relative to that fixed now, not the wall clock.
	old := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	plan, err := cache.GC(context.Background(), GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.DeleteEntries) != 1 || len(plan.DeleteStaging) != 1 {
		t.Fatalf("default GC plan = %+v", plan)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan staging stat error = %v, want removed", err)
	}
	entries, err := cache.List(Protection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries after default GC = %+v, want empty", entries)
	}
}

func TestEntryJSONRoundTrip(t *testing.T) {
	entry := Entry{Provider: "github", Repository: "acme/tool", Tag: "v1", PlatformOS: "linux", PlatformArch: "amd64", PlatformLibc: "glibc", AssetName: "tool.tar.gz", TargetName: "tool", ArchiveSHA256: strings.Repeat("a", 64), BinarySHA256: strings.Repeat("b", 64), BinaryXXH3_64: "c", BinarySize: 1, RelativePath: "github/acme/tool/v1/x/tool"}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Entry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Identity() != entry.Identity() {
		t.Fatalf("decoded identity = %q, want %q", decoded.Identity(), entry.Identity())
	}
}

func TestCacheRejectsInvalidInputs(t *testing.T) {
	valid := testKey("tool.tar.gz")
	tests := []struct {
		name string
		key  Key
		wild bool
	}{
		{name: "provider", key: func() Key { key := valid; key.Provider = ""; return key }()},
		{name: "repository", key: func() Key { key := valid; key.Repository = ""; return key }()},
		{name: "tag", key: func() Key { key := valid; key.Tag = ""; return key }()},
		{name: "platform os", key: func() Key { key := valid; key.PlatformOS = ""; return key }()},
		{name: "platform arch", key: func() Key { key := valid; key.PlatformArch = ""; return key }()},
		{name: "libc", key: func() Key { key := valid; key.PlatformLibc = ""; return key }()},
		{name: "asset", key: func() Key { key := valid; key.AssetName = ""; return key }()},
		{name: "target", key: func() Key { key := valid; key.TargetName = ""; return key }()},
		{name: "nul", key: func() Key { key := valid; key.AssetName = "bad\x00name"; return key }(), wild: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.key.Validate(test.wild); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
	if _, err := NewAt(""); err == nil {
		t.Fatal("NewAt() error = nil for empty root")
	}
	var nilCache *Cache
	if _, _, _, err := nilCache.Lookup(valid, ""); err == nil {
		t.Fatal("nil Lookup() error = nil")
	}
	if _, err := nilCache.Acquire(context.Background(), valid); err == nil {
		t.Fatal("nil Acquire() error = nil")
	}
	if _, err := nilCache.Publish(context.Background(), valid, digest([]byte("archive")), []byte("binary")); err == nil {
		t.Fatal("nil Publish() error = nil")
	}
	if _, err := nilCache.List(Protection{}); err == nil {
		t.Fatal("nil List() error = nil")
	}
	if _, err := nilCache.GC(context.Background(), GCOptions{}); err == nil {
		t.Fatal("nil GC() error = nil")
	}
	if err := (&Lock{}).Release(); err != nil {
		t.Fatalf("empty Lock.Release() error = %v", err)
	}
}

func TestLookupRejectsDigestAndIndexPathMismatches(t *testing.T) {
	cache := newTestCache(t)
	key := testKey("tool.tar.gz")
	entry, err := cache.Publish(context.Background(), key, digest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, hit, err := cache.Lookup(key, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	} else if hit {
		t.Fatal("Lookup() hit with mismatched archive digest")
	}
	entry.RelativePath = "outside/tool"
	if err := cache.saveIndex(Index{SchemaVersion: SchemaVersion, Entries: []Entry{entry}}); err != nil {
		t.Fatal(err)
	}
	entries, err := cache.List(Protection{})
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Integrity != IntegrityCorrupt || !strings.Contains(entries[0].Reason, "indexed path") {
		t.Fatalf("path mismatch inspection = %+v", entries[0])
	}
}

func TestPublishRejectsBadDigestEmptyBinaryAndSymlink(t *testing.T) {
	cache := newTestCache(t)
	key := testKey("tool.tar.gz")
	if _, err := cache.Publish(context.Background(), key, "bad", []byte("binary")); err == nil {
		t.Fatal("Publish() error = nil for bad archive digest")
	}
	if _, err := cache.Publish(context.Background(), key, digest([]byte("archive")), nil); err == nil {
		t.Fatal("Publish() error = nil for empty binary")
	}
	path, err := cache.entryPath(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cache.Root(), "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Publish(context.Background(), key, digest([]byte("archive")), []byte("binary")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Publish() error = %v, want symlink refusal", err)
	}
}

func TestGCRejectsNegativeLimitsAndProtectsByPath(t *testing.T) {
	cache := newTestCache(t)
	key := testKey("tool.tar.gz")
	entry, err := cache.Publish(context.Background(), key, digest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache.Root(), entry.RelativePath)
	entries, err := cache.List(Protection{Paths: []string{path}})
	if err != nil || len(entries) != 1 || !entries[0].Protected {
		t.Fatalf("path-protected list = (%+v, %v)", entries, err)
	}
	if _, err := cache.GC(context.Background(), GCOptions{MaxAgeSet: true, MaxAge: -time.Second}); err == nil {
		t.Fatal("GC() error = nil for negative max age")
	}
	if _, err := cache.GC(context.Background(), GCOptions{MaxBytesSet: true, MaxBytes: -1}); err == nil {
		t.Fatal("GC() error = nil for negative max bytes")
	}
}

func TestContextCancellationStopsAcquire(t *testing.T) {
	cache, err := NewAtWithOptions(filepath.Join(t.TempDir(), "cache"), Options{LockTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	key := testKey("tool.tar.gz")
	lock, err := cache.Acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Acquire(ctx, key); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("Acquire() error = %v, want cancellation", err)
	}
}

func TestLoadIndexRejectsMalformedVariants(t *testing.T) {
	validDigest := strings.Repeat("a", 64)
	validEntry := Entry{
		Provider: "github", Repository: "acme/tool", Tag: "v1", PlatformOS: "linux", PlatformArch: "amd64", PlatformLibc: "glibc",
		AssetName: "tool.tar.gz", TargetName: "tool", ArchiveSHA256: validDigest, BinarySHA256: validDigest,
		BinaryXXH3_64: "xxh3", BinarySize: 1, RelativePath: "github/acme/tool/v1/variant/tool",
	}
	tests := []struct {
		name string
		data []byte
	}{
		{name: "schema", data: []byte(`{"schema_version":2,"entries":[]}`)},
		{name: "multiple values", data: []byte(`{"schema_version":1,"entries":[]} {}`)},
		{name: "trailing data", data: []byte(`{"schema_version":1,"entries":[]} {`)},
		{name: "bad archive digest", data: []byte(`{"schema_version":1,"entries":[{"provider":"github","repository":"acme/tool","tag":"v1","platform_os":"linux","platform_arch":"amd64","platform_libc":"glibc","asset_name":"tool.tar.gz","target_name":"tool","archive_sha256":"bad","binary_sha256":"` + validDigest + `","binary_xxh3_64":"xxh3","binary_size":1,"relative_path":"x"}]}`)},
		{name: "bad binary digest", data: []byte(`{"schema_version":1,"entries":[{"provider":"github","repository":"acme/tool","tag":"v1","platform_os":"linux","platform_arch":"amd64","platform_libc":"glibc","asset_name":"tool.tar.gz","target_name":"tool","archive_sha256":"` + validDigest + `","binary_sha256":"bad","binary_xxh3_64":"xxh3","binary_size":1,"relative_path":"x"}]}`)},
		{name: "incomplete entry", data: []byte(`{"schema_version":1,"entries":[{"provider":"github","repository":"acme/tool","tag":"v1","platform_os":"linux","platform_arch":"amd64","platform_libc":"glibc","asset_name":"tool.tar.gz","target_name":"tool","archive_sha256":"` + validDigest + `","binary_sha256":"` + validDigest + `","binary_size":1,"relative_path":"x"}]}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache := newTestCache(t)
			if err := os.MkdirAll(cache.Root(), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cache.Root(), "index.json"), test.data, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := cache.List(Protection{}); err == nil {
				t.Fatal("List() error = nil")
			}
		})
	}
	cache := newTestCache(t)
	duplicateData, err := json.Marshal(Index{SchemaVersion: SchemaVersion, Entries: []Entry{validEntry, validEntry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache.Root(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root(), "index.json"), duplicateData, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(Protection{}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate index error = %v", err)
	}
}

func TestMissingArtifactIsReportedAndCollected(t *testing.T) {
	cache := newTestCache(t)
	key := testKey("tool.tar.gz")
	entry, err := cache.Publish(context.Background(), key, digest([]byte("archive")), []byte("binary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cache.Root(), entry.RelativePath)); err != nil {
		t.Fatal(err)
	}
	entries, err := cache.List(Protection{})
	if err != nil || len(entries) != 1 || entries[0].Integrity != IntegrityMissing {
		t.Fatalf("missing artifact list = (%+v, %v)", entries, err)
	}
	if _, err := cache.GC(context.Background(), GCOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, err = cache.List(Protection{})
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing artifact after GC = (%+v, %v)", entries, err)
	}
}

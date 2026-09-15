// Package lockfile defines the portable artifact lock used by zenget's
// manifest workflow.
package lockfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"zenget/internal/install"
	"zenget/internal/limits"
	"zenget/internal/state"
)

const (
	// SchemaVersion is the current artifact-lock schema version.
	SchemaVersion = 1
	// ProviderGitHub is the provider currently supported by lock/apply.
	ProviderGitHub = "github"
	// UnknownLibc records that the manifest did not carry a portable libc
	// choice. The lock still records the field so platform resolution is
	// explicit and future schema versions can refine it.
	UnknownLibc = "unknown"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Lockfile binds manifest choices to provider release metadata and the
// content hash of the downloaded artifact. It intentionally contains no
// download URL, token, local path, timestamp, or usage information.
type Lockfile struct {
	SchemaVersion int     `json:"schema_version"`
	Apps          []Entry `json:"apps"`
}

// File is an alias kept for callers that prefer a generic file name.
type File = Lockfile

// Entry is one locked manifest application.
type Entry struct {
	Repository           string `json:"repository"`
	Provider             string `json:"provider"`
	SelectionFingerprint string `json:"selection_fingerprint"`
	Tag                  string `json:"tag"`
	PlatformOS           string `json:"platform_os"`
	PlatformArch         string `json:"platform_arch"`
	PlatformLibc         string `json:"platform_libc"`
	AssetID              int64  `json:"asset_id"`
	AssetName            string `json:"asset_name"`
	AssetSize            int64  `json:"asset_size"`
	ProviderDigest       string `json:"provider_digest,omitempty"`
	ArchiveSHA256        string `json:"archive_sha256"`
}

// Fingerprint returns a stable SHA-256 fingerprint of the manifest choices
// that determine release and asset selection. Runtime data is not included.
func Fingerprint(entry state.ManifestApp) (string, error) {
	selection := struct {
		Repository    string `json:"repository"`
		Provider      string `json:"provider"`
		Tag           string `json:"tag"`
		Asset         string `json:"asset,omitempty"`
		PlatformOS    string `json:"platform_os,omitempty"`
		PlatformArch  string `json:"platform_arch,omitempty"`
		ArchiveBinary string `json:"archive_binary,omitempty"`
		TargetName    string `json:"target_name,omitempty"`
	}{
		Repository:    entry.Repository,
		Provider:      entry.Provider,
		Tag:           entry.Tag,
		Asset:         entry.Asset,
		PlatformOS:    entry.PlatformOS,
		PlatformArch:  entry.PlatformArch,
		ArchiveBinary: entry.ArchiveBinary,
		TargetName:    entry.TargetName,
	}
	data, err := json.Marshal(selection)
	if err != nil {
		return "", fmt.Errorf("marshal manifest selection: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// Canonicalize returns a copy with entries ordered by repository. The input
// slice is not modified.
func Canonicalize(lock Lockfile) Lockfile {
	canonical := lock
	canonical.Apps = append([]Entry(nil), lock.Apps...)
	sort.Slice(canonical.Apps, func(left, right int) bool {
		return canonical.Apps[left].Repository < canonical.Apps[right].Repository
	})
	return canonical
}

// Validate checks schema, required values, canonical ordering, and all
// provider/content identifiers. Validation is fail-closed for future schema
// versions and unknown fields are rejected by LoadFile before this function.
func Validate(lock Lockfile) error {
	if lock.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported lock schema_version %d; want %d", lock.SchemaVersion, SchemaVersion)
	}
	if lock.Apps == nil {
		return fmt.Errorf("lock apps must be an array")
	}
	previousRepository := ""
	for index, entry := range lock.Apps {
		if !repositoryPattern.MatchString(entry.Repository) {
			return fmt.Errorf("apps[%d] has invalid repository %q", index, entry.Repository)
		}
		if index > 0 && entry.Repository <= previousRepository {
			return fmt.Errorf("apps[%d] is not in strict repository order", index)
		}
		previousRepository = entry.Repository
		if entry.Provider != ProviderGitHub {
			return fmt.Errorf("apps[%d] has unsupported provider %q", index, entry.Provider)
		}
		if !validTag(entry.Tag) {
			return fmt.Errorf("apps[%d] has invalid tag %q", index, entry.Tag)
		}
		if !install.ValidPlatform(entry.PlatformOS, entry.PlatformArch) {
			return fmt.Errorf("apps[%d] has unsupported platform %s/%s", index, entry.PlatformOS, entry.PlatformArch)
		}
		if entry.PlatformLibc != UnknownLibc && entry.PlatformLibc != "glibc" && entry.PlatformLibc != "musl" {
			return fmt.Errorf("apps[%d] has invalid platform_libc %q", index, entry.PlatformLibc)
		}
		if !validHexDigest(entry.SelectionFingerprint) {
			return fmt.Errorf("apps[%d] has invalid selection_fingerprint", index)
		}
		if entry.AssetID <= 0 {
			return fmt.Errorf("apps[%d] has invalid asset_id %d", index, entry.AssetID)
		}
		if entry.AssetName == "" || strings.ContainsRune(entry.AssetName, '\x00') || strings.Contains(entry.AssetName, "://") {
			return fmt.Errorf("apps[%d] has invalid asset_name %q", index, entry.AssetName)
		}
		if entry.AssetSize < 0 {
			return fmt.Errorf("apps[%d] has invalid asset_size %d", index, entry.AssetSize)
		}
		if entry.ProviderDigest != "" && !validProviderDigest(entry.ProviderDigest) {
			return fmt.Errorf("apps[%d] has invalid provider_digest", index)
		}
		if !validHexDigest(entry.ArchiveSHA256) {
			return fmt.Errorf("apps[%d] has invalid archive_sha256", index)
		}
	}
	return nil
}

// Marshal returns canonical, indented JSON with a trailing newline.
func Marshal(lock Lockfile) ([]byte, error) {
	canonical := Canonicalize(lock)
	if err := Validate(canonical); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal lockfile: %w", err)
	}
	return append(data, '\n'), nil
}

// Load reads and strictly validates a local lockfile.
func Load(path string) (Lockfile, error) {
	return LoadFile(path)
}

// LoadFile reads and strictly validates a local lockfile.
func LoadFile(path string) (Lockfile, error) {
	data, err := limits.ReadFile(path, limits.DefaultStructuredBytes, "lockfile")
	if err != nil {
		return Lockfile{}, fmt.Errorf("read lockfile %q: %w", path, err)
	}
	var checked json.RawMessage
	if err := limits.DecodeJSON(bytes.NewReader(data), &checked, limits.DefaultStructuredBytes, "lockfile", path); err != nil {
		return Lockfile{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var lock Lockfile
	if err := decoder.Decode(&lock); err != nil {
		return Lockfile{}, fmt.Errorf("decode lockfile %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Lockfile{}, fmt.Errorf("decode lockfile %q: multiple JSON values", path)
		}
		return Lockfile{}, fmt.Errorf("decode lockfile %q: trailing data: %w", path, err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return Lockfile{}, fmt.Errorf("decode lockfile %q: %w", path, err)
	}
	if object == nil {
		return Lockfile{}, fmt.Errorf("lockfile %q must be a JSON object", path)
	}
	if _, ok := object["schema_version"]; !ok {
		return Lockfile{}, fmt.Errorf("lockfile %q is missing schema_version", path)
	}
	apps, ok := object["apps"]
	if !ok || bytes.Equal(bytes.TrimSpace(apps), []byte("null")) {
		return Lockfile{}, fmt.Errorf("lockfile %q is missing apps", path)
	}
	if err := Validate(lock); err != nil {
		return Lockfile{}, fmt.Errorf("validate lockfile %q: %w", path, err)
	}
	return lock, nil
}

// WriteFile writes data atomically. Existing output is preserved unless
// overwrite is explicitly true.
func WriteFile(output string, data []byte, overwrite bool) error {
	if !overwrite {
		if _, err := os.Lstat(output); err == nil {
			return fmt.Errorf("refusing to overwrite existing lockfile %q; use --force", output)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check lockfile output %q: %w", output, err)
		}
	}

	temporary, err := os.CreateTemp(filepath.Dir(output), ".zenget-lock-*")
	if err != nil {
		return fmt.Errorf("create temporary lockfile in %q: %w", filepath.Dir(output), err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	temporaryClosed := false
	defer func() {
		if !temporaryClosed {
			_ = temporary.Close()
		}
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if n, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write temporary lockfile %q: %w", temporaryPath, err)
	} else if n != len(data) {
		return fmt.Errorf("write temporary lockfile %q: %w", temporaryPath, io.ErrShortWrite)
	}
	if err := temporary.Chmod(0644); err != nil {
		return fmt.Errorf("set lockfile permissions %q: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary lockfile %q: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		temporaryClosed = true
		return fmt.Errorf("close temporary lockfile %q: %w", temporaryPath, err)
	}
	temporaryClosed = true

	if overwrite {
		if err := os.Rename(temporaryPath, output); err != nil {
			return fmt.Errorf("replace lockfile %q: %w", output, err)
		}
	} else {
		if err := os.Link(temporaryPath, output); err != nil {
			return fmt.Errorf("create lockfile %q without overwrite: %w", output, err)
		}
		if err := os.Remove(temporaryPath); err != nil {
			return fmt.Errorf("remove temporary lockfile %q: %w", temporaryPath, err)
		}
	}
	removeTemporary = false
	return nil
}

func validTag(tag string) bool {
	return tag != "" && strings.TrimSpace(tag) == tag &&
		!strings.ContainsAny(tag, "\x00\r\n ~^:?*[\\") &&
		!strings.Contains(tag, "..") && !strings.Contains(tag, "@{") &&
		!strings.HasPrefix(tag, ".") && !strings.HasSuffix(tag, ".") && !strings.HasSuffix(tag, "/")
}

func validProviderDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.EqualFold(value[:len("sha256:")], "sha256:") {
		return false
	}
	return validHexDigest(value[len("sha256:"):])
}

func validHexDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

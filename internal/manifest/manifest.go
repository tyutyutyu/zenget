// Package manifest defines the schema-versioned portable manifest contract.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"zenget/internal/install"
	"zenget/internal/limits"
	"zenget/internal/state"
)

const (
	// SchemaVersion is the current portable manifest schema version.
	SchemaVersion = state.ManifestSchemaVersion
	// SchemaURL is the stable public URL for the version 1 JSON Schema.
	SchemaURL = "https://raw.githubusercontent.com/tyutyutyu/zenget/main/schemas/manifest-v1.schema.json"
	// ProviderGitHub is the provider supported by manifest schema version 1.
	ProviderGitHub = "github"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// New returns a deterministic empty version 1 manifest with the public schema
// reference included for editor integrations.
func New() state.Manifest {
	return state.Manifest{
		SchemaVersion: SchemaVersion,
		SchemaURL:     SchemaURL,
		Apps:          make([]state.ManifestApp, 0),
	}
}

// Canonicalize returns a copy with applications sorted by repository. The
// input slice is not modified.
func Canonicalize(file state.Manifest) state.Manifest {
	canonical := file
	if file.Apps != nil {
		canonical.Apps = append(make([]state.ManifestApp, 0, len(file.Apps)), file.Apps...)
	}
	sort.Slice(canonical.Apps, func(left, right int) bool {
		return canonical.Apps[left].Repository < canonical.Apps[right].Repository
	})
	return canonical
}

// Validate checks the manifest schema contract without accessing the network.
// The optional $schema field, when present, must reference SchemaURL.
func Validate(file state.Manifest) error {
	if file.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d; want %d", file.SchemaVersion, SchemaVersion)
	}
	if file.SchemaURL != "" && file.SchemaURL != SchemaURL {
		return fmt.Errorf("unsupported $schema %q; want %q", file.SchemaURL, SchemaURL)
	}
	if file.Apps == nil {
		return fmt.Errorf("apps must be an array")
	}
	seen := make(map[string]struct{}, len(file.Apps))
	for index, entry := range file.Apps {
		if err := ValidateEntry(entry); err != nil {
			return fmt.Errorf("apps[%d]: %w", index, err)
		}
		if _, ok := seen[entry.Repository]; ok {
			return fmt.Errorf("apps[%d] duplicates repository %q", index, entry.Repository)
		}
		seen[entry.Repository] = struct{}{}
	}
	return nil
}

// ValidateEntry checks one manifest application independently of its array
// position. It is useful to validate add-command choices before writing.
func ValidateEntry(entry state.ManifestApp) error {
	if !repositoryPattern.MatchString(entry.Repository) {
		return fmt.Errorf("invalid repository %q: expected org/repo", entry.Repository)
	}
	if entry.Provider != ProviderGitHub {
		return fmt.Errorf("unsupported provider %q; manifest v1 supports github", entry.Provider)
	}
	if !validTag(entry.Tag) {
		return fmt.Errorf("invalid tag %q", entry.Tag)
	}
	if (entry.PlatformOS == "") != (entry.PlatformArch == "") {
		return fmt.Errorf("must specify both platform_os and platform_arch")
	}
	if entry.PlatformOS != "" && !install.ValidPlatform(entry.PlatformOS, entry.PlatformArch) {
		return fmt.Errorf("unsupported platform %s/%s", entry.PlatformOS, entry.PlatformArch)
	}
	if entry.Asset != "" && (strings.ContainsRune(entry.Asset, '\x00') || strings.Contains(entry.Asset, "://")) {
		return fmt.Errorf("invalid asset choice %q", entry.Asset)
	}
	if entry.ArchiveBinary != "" {
		clean := path.Clean(entry.ArchiveBinary)
		if strings.ContainsRune(entry.ArchiveBinary, '\x00') || strings.ContainsRune(entry.ArchiveBinary, '\\') || path.IsAbs(entry.ArchiveBinary) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || hasParentPathComponent(entry.ArchiveBinary) {
			return fmt.Errorf("invalid archive_binary %q", entry.ArchiveBinary)
		}
	}
	if entry.TargetName != "" && !validTargetName(entry.TargetName) {
		return fmt.Errorf("invalid target_name %q", entry.TargetName)
	}
	return nil
}

// Marshal returns canonical, indented JSON with a trailing newline.
func Marshal(file state.Manifest) ([]byte, error) {
	canonical := Canonicalize(file)
	if err := Validate(canonical); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	return append(data, '\n'), nil
}

// Load reads and strictly validates a local manifest. URLs and stdin are not
// accepted, and the returned applications are canonicalized by repository.
func Load(filePath string) (state.Manifest, error) {
	if !IsLocalPath(filePath) {
		return state.Manifest{}, fmt.Errorf("apply accepts one local manifest file; URLs and stdin are not supported")
	}
	data, err := limits.ReadFile(filePath, limits.DefaultStructuredBytes, "manifest")
	if err != nil {
		return state.Manifest{}, fmt.Errorf("read manifest %q: %w", filePath, err)
	}

	var checked json.RawMessage
	if err := limits.DecodeJSON(bytes.NewReader(data), &checked, limits.DefaultStructuredBytes, "manifest", filePath); err != nil {
		return state.Manifest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file state.Manifest
	if err := decoder.Decode(&file); err != nil {
		return state.Manifest{}, fmt.Errorf("decode manifest %q: %w", filePath, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return state.Manifest{}, fmt.Errorf("decode manifest %q: multiple JSON values", filePath)
		}
		return state.Manifest{}, fmt.Errorf("decode manifest %q: trailing data: %w", filePath, err)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return state.Manifest{}, fmt.Errorf("decode manifest %q: %w", filePath, err)
	}
	if object == nil {
		return state.Manifest{}, fmt.Errorf("manifest %q must be a JSON object", filePath)
	}
	if _, ok := object["schema_version"]; !ok {
		return state.Manifest{}, fmt.Errorf("manifest %q is missing schema_version", filePath)
	}
	appsData, ok := object["apps"]
	if !ok || bytes.Equal(bytes.TrimSpace(appsData), []byte("null")) {
		return state.Manifest{}, fmt.Errorf("manifest %q is missing apps", filePath)
	}
	if err := Validate(file); err != nil {
		return state.Manifest{}, fmt.Errorf("validate manifest %q: %w", filePath, err)
	}
	return Canonicalize(file), nil
}

// IsLocalPath reports whether value names a local filesystem path rather than
// stdin or a URL.
func IsLocalPath(value string) bool {
	if value == "" || value == "-" {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	return parsed.Scheme == "" && parsed.Host == "" && !strings.Contains(value, "://")
}

// WriteFile atomically writes a manifest. Existing regular files require
// overwrite; symlink targets are always rejected, including with overwrite.
func WriteFile(output string, data []byte, overwrite bool) error {
	if output == "" || !IsLocalPath(output) {
		return fmt.Errorf("manifest output must be a local path")
	}
	if info, err := os.Lstat(output); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write manifest through symlink %q", output)
		}
		if !overwrite {
			return fmt.Errorf("refusing to overwrite existing manifest %q; use --force", output)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check manifest output %q: %w", output, err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(output), ".zenget-manifest-*")
	if err != nil {
		return fmt.Errorf("create temporary manifest in %q: %w", filepath.Dir(output), err)
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
		return fmt.Errorf("write temporary manifest %q: %w", temporaryPath, err)
	} else if n != len(data) {
		return fmt.Errorf("write temporary manifest %q: %w", temporaryPath, io.ErrShortWrite)
	}
	if err := temporary.Chmod(0644); err != nil {
		return fmt.Errorf("set manifest permissions %q: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary manifest %q: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		temporaryClosed = true
		return fmt.Errorf("close temporary manifest %q: %w", temporaryPath, err)
	}
	temporaryClosed = true
	if err := os.Rename(temporaryPath, output); err != nil {
		return fmt.Errorf("replace manifest %q: %w", output, err)
	}
	removeTemporary = false
	return nil
}

func validTag(tag string) bool {
	return tag != "" && strings.TrimSpace(tag) == tag &&
		!strings.ContainsAny(tag, "\x00\r\n ~^:?*[\\") &&
		!strings.Contains(tag, "..") && !strings.HasSuffix(tag, ".") && !strings.HasSuffix(tag, "/")
}

func validTargetName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\") && !strings.ContainsRune(name, '\x00')
}

func hasParentPathComponent(value string) bool {
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

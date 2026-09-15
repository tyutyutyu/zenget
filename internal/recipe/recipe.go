// Package recipe defines the local, versioned install-recipe contract.
package recipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"zenget/internal/assetselector"
	"zenget/internal/install"
	"zenget/internal/limits"
)

const (
	// SchemaVersion is the current local install-recipe schema version.
	SchemaVersion = 1

	// ChecksumTypeExact selects a checksum asset by its exact release name.
	ChecksumTypeExact ChecksumType = "exact"
	// ChecksumTypeRegex selects a checksum asset with a RE2 expression.
	ChecksumTypeRegex ChecksumType = "regex"

	// ChecksumFormatRaw expects one bare SHA-256 digest in the checksum asset.
	ChecksumFormatRaw ChecksumFormat = "raw"
	// ChecksumFormatSHA256Sum expects sha256sum lines naming the release asset.
	ChecksumFormatSHA256Sum ChecksumFormat = "sha256sum"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ChecksumType identifies how a recipe locates its checksum release asset.
type ChecksumType string

// ChecksumFormat identifies the syntax contained in a checksum release asset.
type ChecksumFormat string

// Checksum describes an optional SHA-256 checksum asset in a recipe. The
// selector applies only to the assets returned for the current release.
type Checksum struct {
	Type    ChecksumType   `json:"type"`
	Pattern string         `json:"pattern"`
	Format  ChecksumFormat `json:"format"`
}

// ChecksumRule is an alternate descriptive name for Checksum.
type ChecksumRule = Checksum

// Matches reports whether a release asset name satisfies the checksum
// selector. Callers that accept external recipes should validate the recipe
// before calling this method.
func (c Checksum) Matches(name string) bool {
	switch c.Type {
	case ChecksumTypeExact:
		return name == c.Pattern
	case ChecksumTypeRegex:
		compiled, err := regexp.Compile(c.Pattern)
		return err == nil && compiled.MatchString(name)
	default:
		return false
	}
}

// Recipe is a schema-versioned local install recipe. It deliberately stores
// rules only; resolved release metadata and the local recipe path never enter
// application state.
type Recipe struct {
	SchemaVersion    int                     `json:"schema_version"`
	Repository       string                  `json:"repository"`
	SupportedSystems []string                `json:"supported_systems,omitempty"`
	AssetSelector    *assetselector.Selector `json:"asset_selector,omitempty"`
	ArchiveBinary    string                  `json:"archive_binary,omitempty"`
	TargetName       string                  `json:"target_name,omitempty"`
	Checksum         *Checksum               `json:"checksum,omitempty"`
}

// Load reads and strictly validates one explicit local regular file. URLs,
// stdin, directories, and symlinks are rejected before the file is parsed.
func Load(filePath string) (Recipe, error) {
	if !IsLocalPath(filePath) {
		return Recipe{}, fmt.Errorf("recipe must be one local file; URLs and stdin are not supported")
	}

	info, err := os.Lstat(filePath)
	if err != nil {
		return Recipe{}, fmt.Errorf("stat recipe %q: %w", filePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Recipe{}, fmt.Errorf("refusing to read recipe through symlink %q", filePath)
	}
	if !info.Mode().IsRegular() {
		return Recipe{}, fmt.Errorf("recipe %q is not a regular file", filePath)
	}

	data, err := limits.ReadFile(filePath, limits.DefaultStructuredBytes, "recipe")
	if err != nil {
		return Recipe{}, fmt.Errorf("read recipe %q: %w", filePath, err)
	}
	return decode(data, filePath)
}

// Decode validates recipe JSON supplied by a caller that has already read a
// local file. It is useful for deterministic tests and embedded tooling.
func Decode(data []byte, input string) (Recipe, error) {
	return decode(data, input)
}

func decode(data []byte, input string) (Recipe, error) {
	var checked json.RawMessage
	if err := limits.DecodeJSON(bytes.NewReader(data), &checked, limits.DefaultStructuredBytes, "recipe", input); err != nil {
		return Recipe{}, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result Recipe
	if err := decoder.Decode(&result); err != nil {
		return Recipe{}, fmt.Errorf("decode recipe %q: %w", input, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Recipe{}, fmt.Errorf("decode recipe %q: multiple JSON values", input)
		}
		return Recipe{}, fmt.Errorf("decode recipe %q: trailing data: %w", input, err)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return Recipe{}, fmt.Errorf("decode recipe %q: %w", input, err)
	}
	if object == nil {
		return Recipe{}, fmt.Errorf("recipe %q must be a JSON object", input)
	}
	for _, key := range []string{"supported_systems", "asset_selector", "archive_binary", "target_name", "checksum"} {
		if value, ok := object[key]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Recipe{}, fmt.Errorf("recipe %q field %q must not be null", input, key)
		}
	}
	if _, ok := object["schema_version"]; !ok {
		return Recipe{}, fmt.Errorf("recipe %q is missing schema_version", input)
	}
	if _, ok := object["repository"]; !ok {
		return Recipe{}, fmt.Errorf("recipe %q is missing repository", input)
	}

	normalized, err := result.Normalize()
	if err != nil {
		return Recipe{}, fmt.Errorf("validate recipe %q: %w", input, err)
	}
	return normalized, nil
}

// Validate checks the schema and all recipe fields without accessing the
// network or filesystem.
func (r Recipe) Validate() error {
	_, err := r.Normalize()
	return err
}

// Normalize validates a recipe and returns its canonical representation.
// Supported platforms are lower-case, trimmed, unique, and sorted; relative
// archive paths are cleaned with slash semantics.
func (r Recipe) Normalize() (Recipe, error) {
	if r.SchemaVersion != SchemaVersion {
		return Recipe{}, fmt.Errorf("unsupported recipe schema_version %d; want %d", r.SchemaVersion, SchemaVersion)
	}
	if !repositoryPattern.MatchString(r.Repository) {
		return Recipe{}, fmt.Errorf("invalid recipe repository %q: expected org/repo", r.Repository)
	}

	normalized := r
	if r.SupportedSystems != nil {
		normalized.SupportedSystems = make([]string, 0, len(r.SupportedSystems))
		seen := make(map[string]struct{}, len(r.SupportedSystems))
		for _, system := range r.SupportedSystems {
			canonical := strings.ToLower(strings.TrimSpace(system))
			if canonical == "" {
				return Recipe{}, fmt.Errorf("supported_systems contains an empty platform")
			}
			if _, ok := seen[canonical]; ok {
				return Recipe{}, fmt.Errorf("supported_systems contains duplicate platform %q", canonical)
			}
			seen[canonical] = struct{}{}
			goos, goarch, ok := strings.Cut(canonical, "/")
			if !ok || goos == "" || goarch == "" || strings.Contains(goarch, "/") || !install.ValidPlatform(goos, goarch) {
				return Recipe{}, fmt.Errorf("supported_systems contains unsupported platform %q", system)
			}
			normalized.SupportedSystems = append(normalized.SupportedSystems, canonical)
		}
		sort.Strings(normalized.SupportedSystems)
	}

	if normalized.AssetSelector != nil {
		if err := normalized.AssetSelector.Validate(); err != nil {
			return Recipe{}, fmt.Errorf("invalid asset_selector: %w", err)
		}
	}
	if normalized.ArchiveBinary != "" {
		clean, ok := cleanRelativeArchivePath(normalized.ArchiveBinary)
		if !ok {
			return Recipe{}, fmt.Errorf("invalid archive_binary %q", normalized.ArchiveBinary)
		}
		normalized.ArchiveBinary = clean
	}
	if normalized.TargetName != "" && !validTargetName(normalized.TargetName) {
		return Recipe{}, fmt.Errorf("invalid target_name %q", normalized.TargetName)
	}
	if normalized.Checksum != nil {
		if err := normalized.Checksum.Validate(); err != nil {
			return Recipe{}, fmt.Errorf("invalid checksum: %w", err)
		}
	}
	return normalized, nil
}

// Marshal returns canonical, indented JSON with a trailing newline.
func Marshal(r Recipe) ([]byte, error) {
	normalized, err := r.Normalize()
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal recipe: %w", err)
	}
	return append(data, '\n'), nil
}

// IsLocalPath reports whether value is a local path, not stdin or a URL.
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

// Validate checks the checksum selector and SHA-256 file format.
func (c Checksum) Validate() error {
	if c.Pattern == "" || strings.TrimSpace(c.Pattern) == "" || strings.ContainsRune(c.Pattern, '\x00') {
		return fmt.Errorf("checksum selector pattern must not be empty")
	}
	switch c.Type {
	case ChecksumTypeExact:
	case ChecksumTypeRegex:
		if _, err := regexp.Compile(c.Pattern); err != nil {
			return fmt.Errorf("invalid checksum selector regex %q: %w", c.Pattern, err)
		}
	default:
		return fmt.Errorf("unsupported checksum selector type %q; expected %q or %q", c.Type, ChecksumTypeExact, ChecksumTypeRegex)
	}
	switch c.Format {
	case ChecksumFormatRaw, ChecksumFormatSHA256Sum:
		return nil
	default:
		return fmt.Errorf("unsupported checksum format %q; expected %q or %q", c.Format, ChecksumFormatRaw, ChecksumFormatSHA256Sum)
	}
}

func (c Checksum) validate() error {
	return c.Validate()
}

func cleanRelativeArchivePath(value string) (string, bool) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.ContainsRune(value, '\\') || path.IsAbs(value) {
		return "", false
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	for _, component := range strings.Split(clean, "/") {
		if component == ".." {
			return "", false
		}
	}
	return clean, true
}

func validTargetName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\") && !strings.ContainsRune(name, '\x00')
}

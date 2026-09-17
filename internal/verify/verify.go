// Package verify provides SHA-256 checksum verification for downloaded
// release assets against checksums published in the same GitHub release.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/limits"
)

// AggregateNames lists the aggregate checksum file names that zenget
// recognises. Matching is case-insensitive.
var aggregateNames = []string{
	"sha256.sum",
	"checksums.txt",
	"SHA256SUMS",
	"checksums.sha256",
}

// Verify compares the SHA-256 digest of the file at assetPath against every
// checksum published for asset in release. When no checksum is published for
// the asset, it returns nil. When all available checksums match, it writes a
// single informational line to out. Any malformed or mismatching checksum
// stops verification and names its source in the returned error.
type assetDownloader interface {
	DownloadAsset(ctx context.Context, asset github.Asset, destPath string, maxBytes ...int64) error
}

type diagnosticAssetDownloader interface {
	DownloadAssetWithLimit(ctx context.Context, asset github.Asset, destPath string, maxBytes int64, kind string) error
}

// ChecksumFormat identifies the syntax of a recipe-selected checksum asset.
type ChecksumFormat string

const (
	// ChecksumFormatRaw is one bare 64-character SHA-256 digest.
	ChecksumFormatRaw ChecksumFormat = "raw"
	// ChecksumFormatSHA256Sum is a sha256sum file containing the target name.
	ChecksumFormatSHA256Sum ChecksumFormat = "sha256sum"
)

func Verify(ctx context.Context, client assetDownloader, out io.Writer, asset github.Asset, release *github.Release, assetPath string) error {
	return VerifyWithPolicy(ctx, client, out, asset, release, assetPath, "", config.ChecksumPolicyIfPresent)
}

// VerifyWithPolicy applies the shared checksum policy to a downloaded release
// asset. If the policy is if-present, an entirely absent checksum is allowed;
// any discovered malformed, wrong-target, or mismatching checksum remains an
// error. Required additionally rejects an asset when no valid SHA-256 evidence
// is available.
func VerifyWithPolicy(ctx context.Context, client assetDownloader, out io.Writer, asset github.Asset, release *github.Release, assetPath, repository string, policy config.ChecksumPolicy) error {
	effectivePolicy, err := config.ParseChecksumPolicy(string(policy))
	if err != nil {
		return fmt.Errorf("invalid checksum policy %q: %w", policy, err)
	}
	if release == nil {
		return wrapPolicyError(repository, release, asset, effectivePolicy, fmt.Errorf("release is nil"))
	}

	expected, err := findExpectedDigests(ctx, client, asset, release)
	if err != nil {
		return wrapPolicyError(repository, release, asset, effectivePolicy, err)
	}
	if len(expected) == 0 {
		if effectivePolicy == config.ChecksumPolicyRequired {
			return wrapPolicyError(repository, release, asset, effectivePolicy, fmt.Errorf("no valid SHA-256 checksum was found"))
		}
		return nil
	}

	actual, err := fileSHA256(assetPath)
	if err != nil {
		return wrapPolicyError(repository, release, asset, effectivePolicy, fmt.Errorf("compute SHA-256 for %q: %w", asset.Name, err))
	}

	for _, checksum := range expected {
		if !strings.EqualFold(actual, checksum.digest) {
			return wrapPolicyError(repository, release, asset, effectivePolicy, fmt.Errorf(
				"checksum mismatch for %q: expected %s (from %s), got %s",
				asset.Name,
				strings.ToLower(checksum.digest),
				checksum.source,
				strings.ToLower(actual),
			))
		}
	}

	if out != nil {
		if repository == "" {
			_, _ = fmt.Fprintf(out, "Verified SHA-256 checksum for %s\n", asset.Name)
		} else {
			_, _ = fmt.Fprintf(out, "Verified SHA-256 checksum for %s (policy=%s repository=%s release=%s)\n", asset.Name, effectivePolicy, repository, release.TagName)
		}
	}
	return nil
}

func wrapPolicyError(repository string, release *github.Release, asset github.Asset, policy config.ChecksumPolicy, err error) error {
	releaseTag := "<nil>"
	if release != nil && release.TagName != "" {
		releaseTag = release.TagName
	}
	if repository == "" {
		return fmt.Errorf("checksum verification failed (policy=%s release=%s asset=%q): %w", policy, releaseTag, asset.Name, err)
	}
	return fmt.Errorf("checksum verification failed (policy=%s repository=%s release=%s asset=%q): %w", policy, repository, releaseTag, asset.Name, err)
}

// VerifyWithChecksumAsset downloads and verifies one recipe-selected checksum
// asset against assetPath. Unlike Verify, it does not inspect the release's
// built-in checksum conventions; the caller has already resolved the one
// checksum asset named by the recipe.
func VerifyWithChecksumAsset(ctx context.Context, client assetDownloader, out io.Writer, asset, checksumAsset github.Asset, format ChecksumFormat, assetPath string) error {
	if client == nil {
		return fmt.Errorf("GitHub client is nil")
	}
	data, err := downloadAssetBytes(ctx, client, checksumAsset)
	if err != nil {
		return err
	}

	digest, ok := parseRecipeChecksum(data, asset.Name, format)
	if !ok {
		return fmt.Errorf("checksum asset %q does not contain a valid SHA-256 digest for %q", checksumAsset.Name, asset.Name)
	}

	actual, err := fileSHA256(assetPath)
	if err != nil {
		return fmt.Errorf("compute SHA-256 for %q: %w", asset.Name, err)
	}
	if !strings.EqualFold(actual, digest) {
		return fmt.Errorf(
			"checksum mismatch for %q: expected %s (from %s), got %s",
			asset.Name,
			strings.ToLower(digest),
			checksumAsset.Name,
			strings.ToLower(actual),
		)
	}

	if out != nil {
		_, _ = fmt.Fprintf(out, "Verified SHA-256 checksum for %s from %s\n", asset.Name, checksumAsset.Name)
	}
	return nil
}

func parseRecipeChecksum(data []byte, targetName string, format ChecksumFormat) (string, bool) {
	switch format {
	case ChecksumFormatRaw:
		digest := strings.TrimSpace(string(data))
		if isHexDigest(digest) {
			return strings.ToLower(digest), true
		}
		return "", false
	case ChecksumFormatSHA256Sum:
		return parseAggregateChecksumFile(data, targetName)
	default:
		return "", false
	}
}

type expectedDigest struct {
	digest string
	source string
}

func findExpectedDigests(ctx context.Context, client assetDownloader, asset github.Asset, release *github.Release) ([]expectedDigest, error) {
	if client == nil {
		return nil, fmt.Errorf("GitHub client is nil")
	}

	var expected []expectedDigest
	if asset.Digest != "" {
		digest, err := parseGitHubDigest(asset.Digest)
		if err != nil {
			return nil, fmt.Errorf("invalid digest for %q: %w", asset.Name, err)
		}
		expected = append(expected, expectedDigest{digest: digest, source: "GitHub digest"})
	}

	// Sidecar checksums are still checked first for compatibility, but they no
	// longer suppress an available GitHub digest or aggregate checksum.
	sidecarName := asset.Name + ".sha256"
	for _, a := range release.Assets {
		if strings.EqualFold(a.Name, sidecarName) {
			digest, err := downloadDigest(ctx, client, a, asset.Name)
			if err != nil {
				return nil, err
			}
			expected = append(expected, expectedDigest{digest: digest, source: a.Name})
			break
		}
	}

	for _, aggregate := range release.Assets {
		if !isAggregateFile(aggregate.Name) {
			continue
		}
		digest, err := downloadAndLookupDigest(ctx, client, aggregate, asset.Name)
		if err != nil {
			return nil, err
		}
		if digest != "" {
			expected = append(expected, expectedDigest{digest: digest, source: aggregate.Name})
		}
	}

	return expected, nil
}

// findExpectedDigest returns the expected SHA-256 digest for asset, the source
// it came from, and whether a checksum was found at all.
func findExpectedDigest(ctx context.Context, client assetDownloader, asset github.Asset, release *github.Release) (string, string, bool, error) {
	expected, err := findExpectedDigests(ctx, client, asset, release)
	if err != nil {
		return "", "", false, err
	}
	if len(expected) == 0 {
		return "", "", false, nil
	}
	return expected[0].digest, expected[0].source, true, nil
}

func parseGitHubDigest(value string) (string, error) {
	return ParseSHA256Digest(value)
}

// ParseSHA256Digest validates a provider digest in sha256:<64 hex> form and
// returns the lower-case hexadecimal digest.
func ParseSHA256Digest(value string) (string, error) {
	const prefix = "sha256:"
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", fmt.Errorf("expected sha256:<64 hex>")
	}
	digest := value[len(prefix):]
	if !isHexDigest(digest) {
		return "", fmt.Errorf("expected sha256:<64 hex>")
	}
	return strings.ToLower(digest), nil
}

// isAggregateFile reports whether name is one of the recognised aggregate
// checksum file names.
func isAggregateFile(name string) bool {
	lower := strings.ToLower(name)
	for _, candidate := range aggregateNames {
		if lower == strings.ToLower(candidate) {
			return true
		}
	}
	return false
}

// downloadDigest downloads a single checksum asset and extracts the digest for
// the named target file. Sidecar files usually contain only the digest, but the
// target name is checked when the file follows the sha256sum format.
func downloadDigest(ctx context.Context, client assetDownloader, asset github.Asset, targetName string) (string, error) {
	data, err := downloadAssetBytes(ctx, client, asset)
	if err != nil {
		return "", err
	}

	digest, ok := parseSidecarChecksumFile(data, targetName)
	if !ok {
		return "", fmt.Errorf("checksum file %q does not contain a valid digest for %q", asset.Name, targetName)
	}
	return digest, nil
}

// downloadAndLookupDigest downloads an aggregate checksum file and returns the
// digest for targetName. A recognized aggregate file that is malformed or does
// not contain the target is an error.
func downloadAndLookupDigest(ctx context.Context, client assetDownloader, asset github.Asset, targetName string) (string, error) {
	data, err := downloadAssetBytes(ctx, client, asset)
	if err != nil {
		return "", err
	}

	digest, ok := parseAggregateChecksumFile(data, targetName)
	if !ok {
		return "", fmt.Errorf("checksum file %q does not contain a valid digest for %q", asset.Name, targetName)
	}
	return digest, nil
}

// downloadAssetBytes downloads asset to a temporary file and returns its
// contents. It reuses github.Client.DownloadAsset so authentication and error
// handling stay consistent.
func downloadAssetBytes(ctx context.Context, client assetDownloader, asset github.Asset) ([]byte, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config limits: %w", err)
	}
	checksumLimit := cfg.EffectiveLimits().ChecksumBytes
	tmp, err := os.CreateTemp("", "zenget-checksum-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary checksum file: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	var downloadErr error
	if diagnosticClient, ok := client.(diagnosticAssetDownloader); ok {
		downloadErr = diagnosticClient.DownloadAssetWithLimit(ctx, asset, tmpPath, checksumLimit, "checksum asset")
	} else {
		downloadErr = client.DownloadAsset(ctx, asset, tmpPath, checksumLimit)
	}
	if downloadErr != nil {
		return nil, fmt.Errorf("download checksum asset %q: %w", asset.Name, downloadErr)
	}

	data, err := limits.ReadFile(tmpPath, checksumLimit, "checksum asset")
	if err != nil {
		return nil, fmt.Errorf("read downloaded checksum file %q: %w", tmpPath, err)
	}
	return data, nil
}

// parseSidecarChecksumFile parses a per-asset sidecar checksum file. It
// returns the first valid SHA-256 digest found, either as a bare digest or in
// sha256sum format. When targetName is supplied, named entries must identify
// that target; bare sidecar digests remain valid.
func parseSidecarChecksumFile(data []byte, targetNames ...string) (string, bool) {
	targetName := ""
	if len(targetNames) > 0 {
		targetName = targetNames[0]
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		digest, name := splitChecksumLine(line)
		if name != "" && targetName != "" && !strings.EqualFold(name, targetName) && !strings.EqualFold(filepathBase(name), targetName) {
			continue
		}
		if isHexDigest(digest) {
			return digest, true
		}
	}
	return "", false
}

// parseAggregateChecksumFile parses a sha256sum-style aggregate file and
// returns the digest for the entry whose file name matches targetName. It
// ignores bare digests because they do not identify which asset they belong to.
func parseAggregateChecksumFile(data []byte, targetName string) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		digest, name := splitChecksumLine(line)
		if !isHexDigest(digest) || name == "" {
			continue
		}

		if strings.EqualFold(name, targetName) || strings.EqualFold(filepathBase(name), targetName) {
			return digest, true
		}
	}
	return "", false
}

// splitChecksumLine splits a single sha256sum line into digest and file name.
// It handles both the standard "<digest>  <name>" form and a bare digest.
func splitChecksumLine(line string) (digest, name string) {
	line = strings.TrimSpace(line)
	if idx := strings.IndexFunc(line, unicodeIsSpace); idx > 0 {
		digest = strings.ToLower(strings.TrimSpace(line[:idx]))
		name = strings.TrimSpace(line[idx+1:])
		if strings.HasPrefix(name, "*") {
			name = strings.TrimSpace(name[1:])
		}
		return digest, name
	}

	// Bare digest: the entire line must be 64 hex characters.
	candidate := strings.ToLower(strings.TrimSpace(line))
	if isHexDigest(candidate) {
		return candidate, ""
	}
	return "", ""
}

// unicodeIsSpace reports whether r is a Unicode space character.
func unicodeIsSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
}

// isHexDigest reports whether s is a 64-character hex string.
func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// filepathBase returns the last path element of name, falling back to the
// whole name when it is empty or contains only separators.
func filepathBase(name string) string {
	base := name
	for {
		idx := strings.LastIndexAny(base, "/\\")
		if idx < 0 {
			break
		}
		if idx == len(base)-1 {
			base = base[:idx]
			continue
		}
		base = base[idx+1:]
		break
	}
	if base == "" {
		return name
	}
	return base
}

// fileSHA256 returns the lower-case hex SHA-256 digest of path.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	sum := hasher.Sum(nil)
	return strings.ToLower(hex.EncodeToString(sum[:])), nil
}

// SHA256File returns the lower-case SHA-256 digest of a local file.
func SHA256File(path string) (string, error) {
	return fileSHA256(path)
}

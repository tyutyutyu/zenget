// Package install provides release asset extraction and binary installation.
package install

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/limits"
	"zenget/internal/shim"
	"zenget/internal/wrapper"
)

// SelectAsset picks the best downloadable asset from a release.
//
// When assetOverride is non-empty, it selects by exact or unique substring
// match against all asset names. Otherwise it filters out checksums,
// signatures, installer scripts, unsupported package formats and source
// archives, then prefers an asset whose name contains OS/arch tokens for the
// requested platform. On a detected musl Linux host, explicitly gnu/glibc
// assets are excluded and musl assets are preferred over libc-neutral assets.
// Format preference is tar.gz/tgz > tar.xz > tar.zst > tar.bz2 > zip > tar >
// compressed raw > raw, with ties broken by asset name.
func SelectAsset(rel *github.Release, goos, goarch, assetOverride string) (github.Asset, error) {
	return selectAssetFromAnalysis(rel, goos, goarch, assetOverride)
}

// FilterAssets removes non-installable release assets and, when possible,
// narrows the result to assets matching goos/goarch. The returned slice keeps
// release order so callers can present a stable list for user selection.
func FilterAssets(assets []github.Asset, goos, goarch string) []github.Asset {
	candidates := filterNoise(assets)
	if len(candidates) <= 1 {
		return candidates
	}
	platformCandidates := filterPlatform(candidates, goos, goarch)
	if len(platformCandidates) == 0 {
		return candidates
	}
	platformCandidates = preferArchitectureSpecific(filterBitness(platformCandidates, goarch), goos, goarch)
	if len(platformCandidates) == 0 {
		return candidates
	}
	libcCandidates := filterLibc(platformCandidates, hostLibcFor(goos))
	if len(libcCandidates) == 0 {
		return platformCandidates
	}
	return libcCandidates
}

func selectAssetWithLibc(rel *github.Release, goos, goarch, assetOverride string, host libcKind) (github.Asset, error) {
	if rel == nil {
		return github.Asset{}, fmt.Errorf("cannot select an asset from a nil release")
	}
	if goos != "linux" {
		host = libcUnknown
	}

	if len(rel.Assets) == 0 {
		return github.Asset{}, fmt.Errorf("release %q has no downloadable assets", rel.TagName)
	}

	if assetOverride != "" {
		return selectAssetOverride(rel, assetOverride)
	}

	candidates := filterNoise(rel.Assets)
	if len(candidates) == 1 {
		if len(filterLibc(candidates, host)) == 0 {
			return github.Asset{}, noCompatibleLibcAssetError(rel, goos, goarch, candidates)
		}
		return candidates[0], nil
	}

	platformCandidates := filterPlatform(candidates, goos, goarch)
	if len(platformCandidates) > 0 {
		platformCandidates = preferArchitectureSpecific(filterBitness(platformCandidates, goarch), goos, goarch)
		if len(platformCandidates) == 0 {
			return github.Asset{}, fmt.Errorf(
				"release %q has multiple downloadable assets and none match %s/%s; candidates: %s; use --asset to select one explicitly",
				rel.TagName,
				goos,
				goarch,
				strings.Join(assetNames(candidates), ", "),
			)
		}
		libcCandidates := filterLibc(platformCandidates, host)
		if len(libcCandidates) > 0 {
			return bestAsset(libcCandidates), nil
		}
		return github.Asset{}, noCompatibleLibcAssetError(rel, goos, goarch, platformCandidates)
	}

	names := assetNames(candidates)
	if len(candidates) == 0 {
		names = assetNames(rel.Assets)
		return github.Asset{}, fmt.Errorf(
			"release %q has no downloadable assets after filtering; available assets: %s; use --asset to select one explicitly",
			rel.TagName,
			strings.Join(names, ", "),
		)
	}

	return github.Asset{}, fmt.Errorf(
		"release %q has multiple downloadable assets and none match %s/%s; candidates: %s; use --asset to select one explicitly",
		rel.TagName,
		goos,
		goarch,
		strings.Join(names, ", "),
	)
}

func noCompatibleLibcAssetError(rel *github.Release, goos, goarch string, candidates []github.Asset) error {
	return fmt.Errorf(
		"release %q has no asset compatible with the host libc for %s/%s; candidates: %s; use --asset to select one explicitly",
		rel.TagName,
		goos,
		goarch,
		strings.Join(assetNames(candidates), ", "),
	)
}

// selectAssetOverride selects an asset by exact name or unique substring.
func selectAssetOverride(rel *github.Release, override string) (github.Asset, error) {
	lowerOverride := strings.ToLower(override)

	var exact []github.Asset
	for _, asset := range rel.Assets {
		if strings.EqualFold(asset.Name, override) {
			exact = append(exact, asset)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}

	var substring []github.Asset
	for _, asset := range rel.Assets {
		if strings.Contains(strings.ToLower(asset.Name), lowerOverride) {
			substring = append(substring, asset)
		}
	}
	switch len(substring) {
	case 1:
		return substring[0], nil
	case 0:
		return github.Asset{}, fmt.Errorf(
			"release %q has no asset matching %q; available assets: %s",
			rel.TagName,
			override,
			strings.Join(assetNames(rel.Assets), ", "),
		)
	default:
		return github.Asset{}, fmt.Errorf(
			"release %q has ambiguous assets matching %q: %s",
			rel.TagName,
			override,
			strings.Join(assetNames(substring), ", "),
		)
	}
}

// filterNoise removes assets that are clearly not installable binaries.
func filterNoise(assets []github.Asset) []github.Asset {
	var out []github.Asset
	for _, asset := range assets {
		if !isNoise(asset.Name) {
			out = append(out, asset)
		}
	}
	return out
}

// isNoise reports whether a file name is a checksum, signature, installer
// script, unsupported package format or source archive.
func isNoise(name string) bool {
	lower := strings.ToLower(name)

	noiseExts := []string{
		".sha256", ".sha512", ".md5", ".sig", ".asc", ".pem", ".sbom",
		".sh", ".ps1", ".bat",
		".deb", ".rpm", ".msi", ".dmg", ".apk", ".pkg",
	}
	for _, ext := range noiseExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}

	noiseSuffixes := []string{
		"source.tar.gz", "source.zip",
		"checksums.txt", "sha256.sum", "sha256sums", "checksums.sha256", "dist-manifest.json",
	}
	for _, suffix := range noiseSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}

	return false
}

// osAliases maps a Go runtime GOOS value to the tokens that may indicate it.
var osAliases = map[string][]string{
	"linux":   {"linux"},
	"darwin":  {"darwin", "macos", "osx"},
	"windows": {"windows", "win"},
}

// archAliases maps a Go runtime GOARCH value to the tokens that may indicate it.
var archAliases = map[string][]string{
	"amd64": {"x86_64", "amd64", "x64"},
	"arm64": {"aarch64", "arm64"},
	"386":   {"i386", "i686", "386", "x86"},
	"arm":   {"armv6", "armv7", "arm"},
}

type libcKind string

const (
	libcUnknown libcKind = "unknown"
	libcGlibc   libcKind = "glibc"
	libcMusl    libcKind = "musl"
)

var nonMuslLibcTokens = []string{"gnu", "glibc"}

// hostLibcFor returns the host libc only when the requested OS is the local
// Linux host. A different target OS has no known libc for asset selection.
func hostLibcFor(goos string) libcKind {
	if goos != "linux" || runtime.GOOS != "linux" {
		return libcUnknown
	}
	return hostLibc()
}

// hostLibc detects musl through its dynamic loader name. Failure to find a
// musl loader is intentionally treated as unknown: unknown has the same
// neutral asset-selection behavior as glibc.
func hostLibc() libcKind {
	if runtime.GOOS != "linux" {
		return libcUnknown
	}

	for _, pattern := range []string{
		"/lib/ld-musl-*.so.1",
		"/lib64/ld-musl-*.so.1",
		"/usr/lib/ld-musl-*.so.1",
	} {
		matches, err := filepath.Glob(pattern)
		if err == nil && len(matches) > 0 {
			return libcMusl
		}
	}
	return libcUnknown
}

// ValidPlatform reports whether goos and goarch are supported by the asset
// selection alias tables.
func ValidPlatform(goos, goarch string) bool {
	_, osOK := osAliases[goos]
	_, archOK := archAliases[goarch]
	return osOK && archOK
}

// LibcIdentity returns the libc identity used in cache keys for a target
// platform. Non-Linux targets, and Linux targets selected from a non-Linux
// host, are intentionally represented as unknown because their runtime libc
// cannot be inspected locally.
func LibcIdentity(goos string) string {
	if goos != "linux" || runtime.GOOS != "linux" {
		return string(libcUnknown)
	}
	if hostLibc() == libcMusl {
		return string(libcMusl)
	}
	return string(libcGlibc)
}

// TargetLibc reports the locally known libc for a target operating system.
// Unlike LibcIdentity, it preserves unknown detection results instead of
// mapping them to the cache-compatible glibc identity.
func TargetLibc(goos string) string {
	return string(hostLibcFor(goos))
}

// SupportedPlatforms returns the sorted lists of supported GOOS and GOARCH
// values used by SelectAsset.
func SupportedPlatforms() (oses, arches []string) {
	for goos := range osAliases {
		oses = append(oses, goos)
	}
	for goarch := range archAliases {
		arches = append(arches, goarch)
	}
	sort.Strings(oses)
	sort.Strings(arches)
	return oses, arches
}

// filterPlatform returns candidates whose names contain OS and arch tokens.
func filterPlatform(candidates []github.Asset, goos, goarch string) []github.Asset {
	osTokens, osOK := osAliases[goos]
	archTokens, archOK := archAliases[goarch]
	if !osOK || !archOK {
		return nil
	}

	var out []github.Asset
	for _, asset := range candidates {
		tokens := tokenSet(asset.Name)
		osMatch := containsAny(tokens, osTokens)
		archMatch := containsAny(tokens, archTokens)
		universalMatch := goos == "darwin" && containsAny(tokens, []string{"universal", "all"})
		if osMatch && (archMatch || universalMatch) {
			out = append(out, asset)
		}
	}
	return out
}

var narrow32Tokens = []string{"i386", "i686", "386", "x86"}

func filterBitness(candidates []github.Asset, goarch string) []github.Asset {
	if goarch != "amd64" && goarch != "arm64" {
		return candidates
	}

	var non32 []github.Asset
	for _, asset := range candidates {
		if !containsAny(tokenSet(asset.Name), narrow32Tokens) {
			non32 = append(non32, asset)
		}
	}
	if len(non32) == 0 {
		return nil
	}
	return non32
}

func preferArchitectureSpecific(candidates []github.Asset, goos, goarch string) []github.Asset {
	if goos != "darwin" || len(candidates) < 2 {
		return candidates
	}
	archTokens := archAliases[goarch]
	var specific []github.Asset
	for _, asset := range candidates {
		tokens := tokenSet(asset.Name)
		if containsAny(tokens, archTokens) && !containsAny(tokens, []string{"universal", "all"}) {
			specific = append(specific, asset)
		}
	}
	if len(specific) > 0 {
		return specific
	}
	return candidates
}

// filterLibc applies libc preferences to platform-matched candidates. Musl
// hosts reject explicitly gnu/glibc assets and prefer musl assets over
// candidates without a libc token. Glibc and unknown hosts retain the input
// unchanged so their selection behavior stays compatible.
func filterLibc(candidates []github.Asset, host libcKind) []github.Asset {
	if host != libcMusl {
		return candidates
	}

	var muslCandidates []github.Asset
	var neutralCandidates []github.Asset
	for _, asset := range candidates {
		tokens := tokenSet(asset.Name)
		if containsAny(tokens, nonMuslLibcTokens) {
			continue
		}
		if _, ok := tokens["musl"]; ok {
			muslCandidates = append(muslCandidates, asset)
			continue
		}
		neutralCandidates = append(neutralCandidates, asset)
	}

	if len(muslCandidates) > 0 {
		return muslCandidates
	}
	return neutralCandidates
}

// tokenize splits a name on non-alphanumeric characters and then glues
// adjacent tokens back together when they form a known multi-part arch token
// such as "x86_64" or "armv7".
func tokenize(name string) []string {
	raw := splitOnNonAlnum(strings.ToLower(name))
	return mergeGluedTokens(raw)
}

// splitOnNonAlnum splits s at every non-alphanumeric rune.
func splitOnNonAlnum(s string) []string {
	var tokens []string
	var buf strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			buf.WriteRune(r)
			continue
		}
		if buf.Len() > 0 {
			tokens = append(tokens, buf.String())
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		tokens = append(tokens, buf.String())
	}
	return tokens
}

// gluedTokens maps a known multi-part arch token to the raw tokens that would
// appear after splitting on non-alphanumeric characters.
var gluedTokens = map[string][2]string{
	"x86_64":  {"x86", "64"},
	"aarch64": {"aarch", "64"},
	"armv6":   {"arm", "v6"},
	"armv7":   {"arm", "v7"},
	"armv8":   {"arm", "v8"},
	"i386":    {"i", "386"},
	"i686":    {"i", "686"},
}

// mergeGluedTokens recombines adjacent raw tokens that form known arch tokens.
func mergeGluedTokens(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	var out []string
	for i := 0; i < len(tokens); i++ {
		merged := false
		if i+1 < len(tokens) {
			for glued, parts := range gluedTokens {
				if tokens[i] == parts[0] && tokens[i+1] == parts[1] {
					out = append(out, glued)
					i++
					merged = true
					break
				}
			}
		}
		if !merged {
			out = append(out, tokens[i])
		}
	}
	return out
}

// tokenSet returns the tokens in a name as a set.
func tokenSet(name string) map[string]struct{} {
	tokens := tokenize(name)
	set := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		set[t] = struct{}{}
	}
	return set
}

// containsAny reports whether set contains any of the values.
func containsAny(set map[string]struct{}, values []string) bool {
	for _, v := range values {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

// bestAsset returns the preferred asset using format rank and name order.
func bestAsset(assets []github.Asset) github.Asset {
	if len(assets) == 1 {
		return assets[0]
	}
	sorted := make([]github.Asset, len(assets))
	copy(sorted, assets)
	sort.Slice(sorted, func(i, j int) bool {
		rankI, rankJ := formatRank(sorted[i].Name), formatRank(sorted[j].Name)
		if rankI != rankJ {
			return rankI < rankJ
		}
		return sorted[i].Name < sorted[j].Name
	})
	return sorted[0]
}

// formatRank assigns a preference order to supported formats.
func formatRank(name string) int {
	switch DetectFormat(name) {
	case FormatTarGz:
		return 0
	case FormatTarXz:
		return 1
	case FormatTarZst:
		return 2
	case FormatTarBz2:
		return 3
	case FormatZip:
		return 4
	case FormatTar:
		return 5
	case FormatGz, FormatBz2, FormatXz, FormatZst:
		return 6
	case FormatRaw:
		return 7
	default:
		return 8
	}
}

// assetNames extracts names from a slice of assets.
func assetNames(assets []github.Asset) []string {
	names := make([]string, len(assets))
	for i, asset := range assets {
		names[i] = asset.Name
	}
	return names
}

// Format identifies the supported release asset formats.
type Format int

const (
	// FormatRaw is an uncompressed binary.
	FormatRaw Format = iota
	// FormatTarGz is a gzip-compressed tar archive.
	FormatTarGz
	// FormatZip is a ZIP archive.
	FormatZip
	// FormatTarXz is an xz-compressed tar archive.
	FormatTarXz
	// FormatTarBz2 is a bzip2-compressed tar archive.
	FormatTarBz2
	// FormatTar is an uncompressed tar archive.
	FormatTar
	// FormatGz is a gzip-compressed raw binary.
	FormatGz
	// FormatBz2 is a bzip2-compressed raw binary.
	FormatBz2
	// FormatXz is an xz-compressed raw binary.
	FormatXz
	// FormatTarZst is a zstd-compressed tar archive.
	FormatTarZst
	// FormatZst is a zstd-compressed raw binary.
	FormatZst
)

// DetectFormat determines an asset format from its file name.
//
// Tar-suffixed names are matched before bare compression suffixes so that
// "x.tar.gz" is classified as a tar.gz archive rather than a compressed raw
// binary.
func DetectFormat(name string) Format {
	lowerName := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lowerName, ".tar.gz"), strings.HasSuffix(lowerName, ".tgz"):
		return FormatTarGz
	case strings.HasSuffix(lowerName, ".tar.xz"):
		return FormatTarXz
	case strings.HasSuffix(lowerName, ".tar.bz2"):
		return FormatTarBz2
	case strings.HasSuffix(lowerName, ".tar.zst"), strings.HasSuffix(lowerName, ".tzst"):
		return FormatTarZst
	case strings.HasSuffix(lowerName, ".tar"):
		return FormatTar
	case strings.HasSuffix(lowerName, ".zip"):
		return FormatZip
	case strings.HasSuffix(lowerName, ".gz"):
		return FormatGz
	case strings.HasSuffix(lowerName, ".bz2"):
		return FormatBz2
	case strings.HasSuffix(lowerName, ".xz"):
		return FormatXz
	case strings.HasSuffix(lowerName, ".zst"):
		return FormatZst
	default:
		return FormatRaw
	}
}

const (
	tarMagicOffset  = 257
	tarMagic        = "ustar"
	sniffHeaderSize = tarMagicOffset + len(tarMagic)
)

type contentFormatRule struct {
	magic         []byte
	format        Format
	archiveFormat Format
}

// contentFormatRules maps magic bytes to the corresponding format. Compressed
// formats also carry an archive format so their decompressed stream can be
// checked for a tar header. Add future signatures here without changing the
// detection flow.
var contentFormatRules = []contentFormatRule{
	{magic: []byte{0x1f, 0x8b}, format: FormatGz, archiveFormat: FormatTarGz},
	{magic: []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, format: FormatXz, archiveFormat: FormatTarXz},
	{magic: []byte{'B', 'Z', 'h'}, format: FormatBz2, archiveFormat: FormatTarBz2},
	{magic: []byte{'P', 'K', 0x03, 0x04}, format: FormatZip},
}

// detectFormatFromContent identifies an asset from bounded magic-byte reads,
// falling back to the filename-derived format when the content is unknown.
func detectFormatFromContent(assetPath string, fallback Format) Format {
	prefix, err := readPrefix(assetPath, sniffHeaderSize)
	if err != nil {
		return fallback
	}
	if hasTarMagic(prefix) {
		return FormatTar
	}

	for _, rule := range contentFormatRules {
		if !bytes.HasPrefix(prefix, rule.magic) {
			continue
		}
		if rule.archiveFormat != FormatRaw && compressedStreamHasTarMagic(assetPath, rule.format) {
			return rule.archiveFormat
		}
		return rule.format
	}
	return fallback
}

func readPrefix(assetPath string, size int) ([]byte, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return io.ReadAll(io.LimitReader(file, int64(size)))
}

func hasTarMagic(contents []byte) bool {
	return len(contents) >= sniffHeaderSize && string(contents[tarMagicOffset:sniffHeaderSize]) == tarMagic
}

func compressedStreamHasTarMagic(assetPath string, format Format) bool {
	file, err := os.Open(assetPath)
	if err != nil {
		return false
	}
	defer file.Close()

	var reader io.Reader
	var closeReader io.Closer
	switch format {
	case FormatGz:
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return false
		}
		reader = gzipReader
		closeReader = gzipReader
	case FormatXz:
		xzReader, err := xz.NewReader(file)
		if err != nil {
			return false
		}
		reader = xzReader
	case FormatBz2:
		reader = bzip2.NewReader(file)
	default:
		return false
	}
	if closeReader != nil {
		defer closeReader.Close()
	}

	prefix, err := io.ReadAll(io.LimitReader(reader, int64(sniffHeaderSize)))
	return err == nil && hasTarMagic(prefix)
}

// archiveCandidate is an executable regular file found in an archive.
type archiveCandidate struct {
	name     string
	contents []byte
}

// ExtractBinary reads a raw binary, decompresses a compressed raw binary, or
// extracts an executable regular file selected by exeName from an archive. For
// archives, an exact archive path wins when one is supplied, followed by an
// exact basename match, a unique basename prefix match, and then the
// single-executable fallback; exeName is ignored for raw formats.
func ExtractBinary(assetPath string, format Format, exeName string) ([]byte, error) {
	resourceLimits, err := configuredLimits()
	if err != nil {
		return nil, err
	}
	return extractBinary(assetPath, format, exeName, resourceLimits)
}

func extractBinary(assetPath string, format Format, exeName string, resourceLimits limits.Limits) ([]byte, error) {
	if err := resourceLimits.Validate(); err != nil {
		return nil, err
	}
	format = detectFormatFromContent(assetPath, format)
	switch format {
	case FormatRaw:
		return limits.ReadFile(assetPath, resourceLimits.BinaryBytes, "binary candidate bytes")
	case FormatGz:
		return decompressGz(assetPath, resourceLimits.BinaryBytes)
	case FormatBz2:
		return decompressBz2(assetPath, resourceLimits.BinaryBytes)
	case FormatXz:
		return decompressXz(assetPath, resourceLimits.BinaryBytes)
	case FormatZst:
		return decompressZst(assetPath, resourceLimits.BinaryBytes)
	}

	var candidates []archiveCandidate
	var err error
	switch format {
	case FormatTarGz:
		candidates, err = extractTarGz(assetPath, resourceLimits)
	case FormatTarXz:
		candidates, err = extractTarXz(assetPath, resourceLimits)
	case FormatTarBz2:
		candidates, err = extractTarBz2(assetPath, resourceLimits)
	case FormatTarZst:
		candidates, err = extractTarZst(assetPath, resourceLimits)
	case FormatTar:
		candidates, err = extractTarFile(assetPath, resourceLimits)
	case FormatZip:
		candidates, err = extractZip(assetPath, resourceLimits)
	default:
		return nil, fmt.Errorf("unsupported asset format %d", format)
	}
	if err != nil {
		return nil, err
	}

	return selectArchiveCandidate(candidates, exeName)
}

// ExtractBinaryByName extracts a binary from an asset using the supplied
// archive member name. It is an explicit-name spelling of ExtractBinary for
// callers that have already resolved an archive choice.
func ExtractBinaryByName(assetPath string, format Format, exeName string) ([]byte, error) {
	return ExtractBinary(assetPath, format, exeName)
}

// ListExecutables returns executable regular-file names in an archive. Raw
// and compressed-raw assets have no archive member list and return nil.
func ListExecutables(assetPath string, format Format) ([]string, error) {
	resourceLimits, err := configuredLimits()
	if err != nil {
		return nil, err
	}
	return listExecutables(assetPath, format, resourceLimits)
}

func listExecutables(assetPath string, format Format, resourceLimits limits.Limits) ([]string, error) {
	if err := resourceLimits.Validate(); err != nil {
		return nil, err
	}
	format = detectFormatFromContent(assetPath, format)
	if format == FormatRaw || format == FormatGz || format == FormatBz2 ||
		format == FormatXz || format == FormatZst {
		return nil, nil
	}

	var candidates []archiveCandidate
	var err error
	switch format {
	case FormatTarGz:
		candidates, err = extractTarGz(assetPath, resourceLimits)
	case FormatTarXz:
		candidates, err = extractTarXz(assetPath, resourceLimits)
	case FormatTarBz2:
		candidates, err = extractTarBz2(assetPath, resourceLimits)
	case FormatTarZst:
		candidates, err = extractTarZst(assetPath, resourceLimits)
	case FormatTar:
		candidates, err = extractTarFile(assetPath, resourceLimits)
	case FormatZip:
		candidates, err = extractZip(assetPath, resourceLimits)
	default:
		return nil, fmt.Errorf("unsupported asset format %d", format)
	}
	if err != nil {
		return nil, err
	}

	names := make([]string, len(candidates))
	for i, candidate := range candidates {
		names[i] = candidate.name
	}
	sort.Strings(names)
	return names, nil
}

func configuredLimits() (limits.Limits, error) {
	cfg, err := config.Load()
	if err != nil {
		return limits.Limits{}, fmt.Errorf("load config limits: %w", err)
	}
	resourceLimits := cfg.EffectiveLimits()
	if err := resourceLimits.Validate(); err != nil {
		return limits.Limits{}, err
	}
	return resourceLimits, nil
}

func selectArchiveCandidate(candidates []archiveCandidate, exeName string) ([]byte, error) {
	if strings.Contains(exeName, "/") {
		wantedPath := path.Clean(exeName)
		var exactPathMatches []archiveCandidate
		for _, candidate := range candidates {
			if path.Clean(candidate.name) == wantedPath {
				exactPathMatches = append(exactPathMatches, candidate)
			}
		}
		if len(exactPathMatches) == 1 {
			return exactPathMatches[0].contents, nil
		}
		if len(exactPathMatches) > 1 {
			return nil, archiveCandidateError(candidates, exeName)
		}
	}
	baseName := path.Base(exeName)
	var exactMatches []archiveCandidate
	for _, candidate := range candidates {
		if path.Base(candidate.name) == baseName {
			exactMatches = append(exactMatches, candidate)
		}
	}
	if len(exactMatches) == 1 {
		return exactMatches[0].contents, nil
	}
	if len(exactMatches) > 1 {
		return nil, archiveCandidateError(candidates, exeName)
	}

	var prefixMatches []archiveCandidate
	for _, candidate := range candidates {
		if strings.HasPrefix(path.Base(candidate.name), baseName) {
			prefixMatches = append(prefixMatches, candidate)
		}
	}
	if len(prefixMatches) == 1 {
		return prefixMatches[0].contents, nil
	}
	if len(candidates) == 1 {
		return candidates[0].contents, nil
	}

	return nil, archiveCandidateError(candidates, exeName)
}

func archiveCandidateError(candidates []archiveCandidate, exeName string) error {
	if len(candidates) == 0 {
		return fmt.Errorf("archive contains 0 executable files; no executable candidates for %q", exeName)
	}

	names := make([]string, len(candidates))
	for i, candidate := range candidates {
		names[i] = candidate.name
	}
	return fmt.Errorf(
		"archive contains %d executable files; no unique executable matching %q; candidates: %s",
		len(candidates),
		exeName,
		strings.Join(names, ", "),
	)
}

func extractTarGz(assetPath string, resourceLimits limits.Limits) ([]archiveCandidate, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open tar.gz asset %q: %w", assetPath, err)
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open gzip asset %q: %w", assetPath, err)
	}
	defer reader.Close()

	return extractTar(reader, assetPath, "tar.gz", resourceLimits)
}

func extractTarXz(assetPath string, resourceLimits limits.Limits) ([]archiveCandidate, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open tar.xz asset %q: %w", assetPath, err)
	}
	defer file.Close()

	reader, err := xz.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open xz asset %q: %w", assetPath, err)
	}

	return extractTar(reader, assetPath, "tar.xz", resourceLimits)
}

func extractTarBz2(assetPath string, resourceLimits limits.Limits) ([]archiveCandidate, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open tar.bz2 asset %q: %w", assetPath, err)
	}
	defer file.Close()

	return extractTar(bzip2.NewReader(file), assetPath, "tar.bz2", resourceLimits)
}

func extractTarZst(assetPath string, resourceLimits limits.Limits) ([]archiveCandidate, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open tar.zst asset %q: %w", assetPath, err)
	}
	defer file.Close()

	reader, err := zstd.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open zstd asset %q: %w", assetPath, err)
	}
	defer reader.Close()

	return extractTar(reader, assetPath, "tar.zst", resourceLimits)
}

func extractTarFile(assetPath string, resourceLimits limits.Limits) ([]archiveCandidate, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open tar asset %q: %w", assetPath, err)
	}
	defer file.Close()

	return extractTar(file, assetPath, "tar", resourceLimits)
}

// extractTar reads executable regular files from a tar archive stream.
func extractTar(r io.Reader, assetPath, label string, resourceLimits limits.Limits) ([]archiveCandidate, error) {
	archiveBudget, err := limits.NewBudget(resourceLimits.ArchiveBytes, "archive decompressed bytes", assetPath)
	if err != nil {
		return nil, err
	}
	tarReader := tar.NewReader(archiveBudget.Reader(r))
	var candidates []archiveCandidate
	var entryCount int64
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s asset %q: %w", label, assetPath, err)
		}
		if err := limits.CheckCount(entryCount, resourceLimits.ArchiveEntries, "archive entry count", assetPath); err != nil {
			return nil, err
		}
		entryCount++
		if header.Size > archiveBudget.Remaining() {
			return nil, &limits.LimitError{
				Kind:  "archive decompressed bytes",
				Limit: resourceLimits.ArchiveBytes,
				Input: assetPath,
			}
		}

		if !header.FileInfo().Mode().IsRegular() || header.Mode&0111 == 0 {
			continue
		}
		if header.Size > resourceLimits.BinaryBytes {
			return nil, &limits.LimitError{
				Kind:  "binary candidate bytes",
				Limit: resourceLimits.BinaryBytes,
				Input: assetPath,
			}
		}
		contents, err := limits.ReadAll(tarReader, resourceLimits.BinaryBytes, "binary candidate bytes", assetPath)
		if err != nil {
			return nil, fmt.Errorf("read executable %q from %s asset: %w", header.Name, label, err)
		}
		candidates = append(candidates, archiveCandidate{name: header.Name, contents: contents})
	}

	return candidates, nil
}

func decompressGz(assetPath string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open gzip asset %q: %w", assetPath, err)
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open gzip asset %q: %w", assetPath, err)
	}
	defer reader.Close()

	contents, err := limits.ReadAll(reader, maxBytes, "binary candidate bytes", assetPath)
	if err != nil {
		return nil, fmt.Errorf("read gzip asset %q: %w", assetPath, err)
	}
	return contents, nil
}

func decompressBz2(assetPath string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open bzip2 asset %q: %w", assetPath, err)
	}
	defer file.Close()

	contents, err := limits.ReadAll(bzip2.NewReader(bufio.NewReader(file)), maxBytes, "binary candidate bytes", assetPath)
	if err != nil {
		return nil, fmt.Errorf("read bzip2 asset %q: %w", assetPath, err)
	}
	return contents, nil
}

func decompressXz(assetPath string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open xz asset %q: %w", assetPath, err)
	}
	defer file.Close()

	reader, err := xz.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open xz asset %q: %w", assetPath, err)
	}

	contents, err := limits.ReadAll(reader, maxBytes, "binary candidate bytes", assetPath)
	if err != nil {
		return nil, fmt.Errorf("read xz asset %q: %w", assetPath, err)
	}
	return contents, nil
}

func decompressZst(assetPath string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open zstd asset %q: %w", assetPath, err)
	}
	defer file.Close()

	reader, err := zstd.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open zstd asset %q: %w", assetPath, err)
	}
	defer reader.Close()

	contents, err := limits.ReadAll(reader, maxBytes, "binary candidate bytes", assetPath)
	if err != nil {
		return nil, fmt.Errorf("read zstd asset %q: %w", assetPath, err)
	}
	return contents, nil
}

func extractZip(assetPath string, resourceLimits limits.Limits) ([]archiveCandidate, error) {
	archive, err := zip.OpenReader(assetPath)
	if err != nil {
		return nil, fmt.Errorf("open zip asset %q: %w", assetPath, err)
	}
	defer archive.Close()
	if int64(len(archive.File)) > resourceLimits.ArchiveEntries {
		return nil, &limits.LimitError{
			Kind:  "archive entry count",
			Limit: resourceLimits.ArchiveEntries,
			Input: assetPath,
		}
	}

	var candidates []archiveCandidate
	archiveBudget, err := limits.NewBudget(resourceLimits.ArchiveBytes, "archive decompressed bytes", assetPath)
	if err != nil {
		return nil, err
	}
	var entryCount int64
	for _, file := range archive.File {
		if err := limits.CheckCount(entryCount, resourceLimits.ArchiveEntries, "archive entry count", assetPath); err != nil {
			return nil, err
		}
		entryCount++
		if !file.Mode().IsRegular() {
			continue
		}
		if file.UncompressedSize64 > uint64(archiveBudget.Remaining()) {
			return nil, &limits.LimitError{
				Kind:  "archive decompressed bytes",
				Limit: resourceLimits.ArchiveBytes,
				Input: assetPath,
			}
		}
		isExecutable := file.Mode()&0111 != 0
		if isExecutable && file.UncompressedSize64 > uint64(resourceLimits.BinaryBytes) {
			return nil, &limits.LimitError{
				Kind:  "binary candidate bytes",
				Limit: resourceLimits.BinaryBytes,
				Input: assetPath,
			}
		}

		reader, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open executable %q from zip asset: %w", file.Name, err)
		}
		boundedReader := archiveBudget.Reader(reader)
		var contents []byte
		var readErr error
		if isExecutable {
			contents, readErr = io.ReadAll(limits.NewReader(boundedReader, resourceLimits.BinaryBytes, "binary candidate bytes", assetPath))
		} else {
			_, readErr = io.Copy(io.Discard, boundedReader)
		}
		closeErr := reader.Close()
		if readErr != nil {
			if isExecutable {
				return nil, fmt.Errorf("read executable %q from zip asset: %w", file.Name, readErr)
			}
			return nil, fmt.Errorf("read archive entry %q from zip asset: %w", file.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close executable %q from zip asset: %w", file.Name, closeErr)
		}
		if isExecutable {
			candidates = append(candidates, archiveCandidate{name: file.Name, contents: contents})
		}
	}

	return candidates, nil
}

// TargetPath returns the effective wrapper path for a repository, honouring the
// install_dir configuration value.
func TargetPath(repo string) (string, error) {
	return TargetPathFor(repo, path.Base(repo))
}

// TargetPathFor returns the configured wrapper path for a custom executable
// name.
func TargetPathFor(repo, targetName string) (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	installDir, err := cfg.InstallDirectory()
	if err != nil {
		return "", err
	}
	return wrapper.TargetPathFor(repo, targetName, installDir)
}

// RealPath returns the default real binary path for a repository.
func RealPath(repo string) (string, error) {
	return wrapper.RealPath(repo)
}

// VersionedRealPath returns the real binary path for a provider, repository,
// release version, and target name.
func VersionedRealPath(provider, repo, version, targetName string) (string, error) {
	return wrapper.VersionedRealPath(provider, repo, version, targetName)
}

func managedWrapperScript(repo, targetName, realPath, usageLog string) ([]byte, error) {
	// A Go test binary contains the testing harness instead of zenget's main
	// command, so it cannot serve as an executable shim runtime. Keep fixture
	// wrappers executable in that isolated environment; production binaries
	// always use the managed-runtime path below.
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".test") {
		return wrapper.Generate(repo, realPath, usageLog), nil
	}
	runtimePath, err := shim.EnsureCurrent()
	if err != nil {
		return nil, err
	}
	return wrapper.GenerateShim(repo, runtimePath, targetName), nil
}

// InstallVersioned writes a new versioned artifact and a stable user-facing
// wrapper. The wrapper is not overwritten when foreignWrapper is true.
func InstallVersioned(binary []byte, provider, repo, version string, foreignWrapper bool, targetName string) (string, string, error) {
	if targetName == "" {
		targetName = path.Base(repo)
	}
	wrapperPath, err := TargetPathFor(repo, targetName)
	if err != nil {
		return "", "", err
	}
	if foreignWrapper {
		return "", "", fmt.Errorf("refusing to overwrite foreign file %q", wrapperPath)
	}
	realPath, err := VersionedRealPath(provider, repo, version, targetName)
	if err != nil {
		return "", "", err
	}
	if err := validateVersionedDestination(realPath, "artifact"); err != nil {
		return "", "", err
	}
	if err := validateVersionedDestination(wrapperPath, "wrapper"); err != nil {
		return "", "", err
	}
	usageLog, err := wrapper.UsageLogPathFor(provider, repo)
	if err != nil {
		return "", "", err
	}
	wrapperScript, err := managedWrapperScript(repo, targetName, realPath, usageLog)
	if err != nil {
		return "", "", err
	}

	if err := writeExecutable(binary, realPath); err != nil {
		return "", "", err
	}
	if err := writeExecutable(wrapperScript, wrapperPath); err != nil {
		_ = os.Remove(realPath)
		return "", "", err
	}
	return wrapperPath, realPath, nil
}

// Install writes the real binary under the XDG data directory and installs a
// POSIX shell wrapper at the user-facing target path. When foreignWrapper is
// true, the target wrapper path is known to belong to something other than
// zenget and must not be overwritten.
func Install(binary []byte, repo string, foreignWrapper bool, targetNames ...string) (string, string, error) {
	targetName := path.Base(repo)
	if len(targetNames) > 0 && targetNames[0] != "" {
		targetName = targetNames[0]
	}
	wrapperPath, err := TargetPathFor(repo, targetName)
	if err != nil {
		return "", "", err
	}
	realPath, err := RealPath(repo)
	if err != nil {
		return "", "", err
	}

	if foreignWrapper {
		return "", "", fmt.Errorf("refusing to overwrite foreign file %q", wrapperPath)
	}
	usageLog, err := wrapper.UsageLogPath(repo)
	if err != nil {
		return "", "", err
	}
	wrapperScript, err := managedWrapperScript(repo, targetName, realPath, usageLog)
	if err != nil {
		return "", "", err
	}

	if err := writeExecutable(binary, realPath); err != nil {
		return "", "", err
	}

	if err := writeExecutable(wrapperScript, wrapperPath); err != nil {
		return "", "", err
	}

	return wrapperPath, realPath, nil
}

type transactionBackup struct {
	target string
	backup string
}

// InstallTransaction describes an upgrade whose filesystem changes can still
// be rolled back until Commit is called.
type InstallTransaction struct {
	wrapperPath      string
	realPath         string
	temporaryPaths   []string
	backups          []transactionBackup
	cleanupWarnings  []error
	realCommitted    bool
	wrapperCommitted bool
	finalized        bool
}

// WrapperPath returns the user-facing wrapper path affected by the upgrade.
func (t *InstallTransaction) WrapperPath() string {
	if t == nil {
		return ""
	}
	return t.wrapperPath
}

// RealPath returns the stored real binary path affected by the upgrade.
func (t *InstallTransaction) RealPath() string {
	if t == nil {
		return ""
	}
	return t.realPath
}

var (
	installRename = os.Rename
	installRemove = os.Remove
)

// InstallUpgrade stages and activates an upgrade for an already registered
// application. Existing real and wrapper files are retained as .old files
// until the returned transaction is committed.
func InstallUpgrade(binary []byte, repo, targetName string) (*InstallTransaction, error) {
	wrapperPath, err := TargetPathFor(repo, targetName)
	if err != nil {
		return nil, err
	}
	realPath, err := RealPath(repo)
	if err != nil {
		return nil, err
	}

	if data, err := os.ReadFile(wrapperPath); err == nil && !wrapper.IsMarkerLine(string(data)) {
		return nil, fmt.Errorf("refusing to overwrite foreign file %q", wrapperPath)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read existing wrapper %q: %w", wrapperPath, err)
	}

	usageLog, err := wrapper.UsageLogPath(repo)
	if err != nil {
		return nil, err
	}
	wrapperScript, err := managedWrapperScript(repo, targetName, realPath, usageLog)
	if err != nil {
		return nil, err
	}
	transaction := &InstallTransaction{wrapperPath: wrapperPath, realPath: realPath}
	for _, item := range []struct {
		data   []byte
		target string
	}{
		{data: binary, target: realPath},
		{data: wrapperScript, target: wrapperPath},
	} {
		temporary, err := prepareExecutable(item.data, item.target)
		if err != nil {
			if cleanupErr := transaction.cleanup(); cleanupErr != nil {
				return nil, fmt.Errorf("%w (cleanup failed: %v)", err, cleanupErr)
			}
			return nil, err
		}
		transaction.temporaryPaths = append(transaction.temporaryPaths, temporary)
	}

	for _, target := range []string{realPath, wrapperPath} {
		backup, existed, err := backupExisting(target)
		if err != nil {
			return nil, abortUpgrade(transaction, err)
		}
		if existed {
			transaction.backups = append(transaction.backups, transactionBackup{target: target, backup: backup})
		}
	}

	if err := installRename(transaction.temporaryPaths[0], realPath); err != nil {
		return nil, abortUpgrade(transaction, fmt.Errorf("activate real binary %q: %w", realPath, err))
	}
	transaction.realCommitted = true
	if err := installRename(transaction.temporaryPaths[1], wrapperPath); err != nil {
		return nil, abortUpgrade(transaction, fmt.Errorf("activate wrapper %q: %w", wrapperPath, err))
	}
	transaction.wrapperCommitted = true
	transaction.temporaryPaths = nil
	return transaction, nil
}

// VersionedInstallTransaction describes a versioned upgrade whose stable
// wrapper and new artifact can still be rolled back until Commit is called.
type VersionedInstallTransaction struct {
	wrapperPath      string
	realPath         string
	temporaryPaths   []string
	backups          []transactionBackup
	cleanupPaths     []string
	cleanupWarnings  []error
	realCommitted    bool
	wrapperCommitted bool
	finalized        bool
}

// WrapperPath returns the stable user-facing wrapper path affected by the
// versioned upgrade.
func (t *VersionedInstallTransaction) WrapperPath() string {
	if t == nil {
		return ""
	}
	return t.wrapperPath
}

// RealPath returns the new versioned artifact path affected by the upgrade.
func (t *VersionedInstallTransaction) RealPath() string {
	if t == nil {
		return ""
	}
	return t.realPath
}

// InstallUpgradeVersioned stages and activates a versioned upgrade. Cleanup
// paths are removed only after Commit, so state persistence can fail without
// losing the previous artifact.
func InstallUpgradeVersioned(binary []byte, provider, repo, version, targetName string, cleanupPaths ...string) (*VersionedInstallTransaction, error) {
	if targetName == "" {
		targetName = path.Base(repo)
	}
	wrapperPath, err := TargetPathFor(repo, targetName)
	if err != nil {
		return nil, err
	}
	realPath, err := VersionedRealPath(provider, repo, version, targetName)
	if err != nil {
		return nil, err
	}
	if err := validateVersionedDestination(realPath, "artifact"); err != nil {
		return nil, err
	}
	if err := validateVersionedDestination(wrapperPath, "wrapper"); err != nil {
		return nil, err
	}

	if data, err := os.ReadFile(wrapperPath); err == nil && !wrapper.IsMarkerLine(string(data)) {
		return nil, fmt.Errorf("refusing to overwrite foreign file %q", wrapperPath)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read existing wrapper %q: %w", wrapperPath, err)
	}

	usageLog, err := wrapper.UsageLogPathFor(provider, repo)
	if err != nil {
		return nil, err
	}
	wrapperScript, err := managedWrapperScript(repo, targetName, realPath, usageLog)
	if err != nil {
		return nil, err
	}
	transaction := &VersionedInstallTransaction{
		wrapperPath:  wrapperPath,
		realPath:     realPath,
		cleanupPaths: uniqueCleanupPaths(cleanupPaths, wrapperPath, realPath),
	}
	for _, item := range []struct {
		data   []byte
		target string
	}{
		{data: binary, target: realPath},
		{data: wrapperScript, target: wrapperPath},
	} {
		temporary, err := prepareExecutable(item.data, item.target)
		if err != nil {
			if cleanupErr := transaction.cleanup(); cleanupErr != nil {
				return nil, fmt.Errorf("%w (cleanup failed: %v)", err, cleanupErr)
			}
			return nil, err
		}
		transaction.temporaryPaths = append(transaction.temporaryPaths, temporary)
	}

	for _, target := range []string{realPath, wrapperPath} {
		backup, existed, err := backupExisting(target)
		if err != nil {
			return nil, abortVersionedUpgrade(transaction, err)
		}
		if existed {
			transaction.backups = append(transaction.backups, transactionBackup{target: target, backup: backup})
		}
	}

	if err := installRename(transaction.temporaryPaths[0], realPath); err != nil {
		return nil, abortVersionedUpgrade(transaction, fmt.Errorf("activate real binary %q: %w", realPath, err))
	}
	transaction.realCommitted = true
	if err := installRename(transaction.temporaryPaths[1], wrapperPath); err != nil {
		return nil, abortVersionedUpgrade(transaction, fmt.Errorf("activate wrapper %q: %w", wrapperPath, err))
	}
	transaction.wrapperCommitted = true
	transaction.temporaryPaths = nil
	return transaction, nil
}

// Rollback restores the wrapper and artifact that were active before the
// versioned upgrade.
func (t *VersionedInstallTransaction) Rollback() error {
	if t == nil || t.finalized {
		return nil
	}
	return t.rollback()
}

// Commit finalizes the versioned upgrade and removes the retained artifact
// that is older than the previous version. Cleanup failures are reported as
// warnings because the new state and wrapper are already safe.
func (t *VersionedInstallTransaction) Commit() error {
	if t == nil || t.finalized {
		return nil
	}
	for _, backup := range t.backups {
		if err := installRemove(backup.backup); err != nil && !os.IsNotExist(err) {
			t.cleanupWarnings = append(t.cleanupWarnings, fmt.Errorf("remove previous install backup %q: %w", backup.backup, err))
		}
	}
	for _, cleanupPath := range t.cleanupPaths {
		if err := installRemove(cleanupPath); err != nil && !os.IsNotExist(err) {
			t.cleanupWarnings = append(t.cleanupWarnings, fmt.Errorf("remove retired artifact %q: %w", cleanupPath, err))
		}
	}
	for _, temporary := range t.temporaryPaths {
		if err := installRemove(temporary); err != nil && !os.IsNotExist(err) {
			t.cleanupWarnings = append(t.cleanupWarnings, fmt.Errorf("remove temporary install file %q: %w", temporary, err))
		}
	}
	t.finalized = true
	return nil
}

// CleanupWarnings returns non-fatal cleanup errors collected by Commit.
func (t *VersionedInstallTransaction) CleanupWarnings() []error {
	if t == nil || len(t.cleanupWarnings) == 0 {
		return nil
	}
	warnings := make([]error, len(t.cleanupWarnings))
	copy(warnings, t.cleanupWarnings)
	return warnings
}

func (t *VersionedInstallTransaction) rollback() error {
	var firstErr error
	if t.wrapperCommitted {
		if err := removeIfExists(t.wrapperPath); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if t.realCommitted {
		if err := removeIfExists(t.realPath); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	restored := make([]bool, len(t.backups))
	for i := len(t.backups) - 1; i >= 0; i-- {
		backup := t.backups[i]
		if err := installRename(backup.backup, backup.target); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("restore %q: %w", backup.target, err)
			}
			continue
		}
		restored[i] = true
	}
	if err := t.cleanupTemporary(); err != nil && firstErr == nil {
		firstErr = err
	}
	for i, backup := range t.backups {
		if !restored[i] {
			continue
		}
		if err := removeIfExists(backup.backup); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove restored backup %q: %w", backup.backup, err)
		}
	}
	t.finalized = true
	return firstErr
}

func (t *VersionedInstallTransaction) cleanup() error {
	var firstErr error
	if err := t.cleanupTemporary(); err != nil {
		firstErr = err
	}
	for _, backup := range t.backups {
		if err := removeIfExists(backup.backup); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t *VersionedInstallTransaction) cleanupTemporary() error {
	var firstErr error
	for _, temporary := range t.temporaryPaths {
		if err := removeIfExists(temporary); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func abortVersionedUpgrade(transaction *VersionedInstallTransaction, operationErr error) error {
	if rollbackErr := transaction.rollback(); rollbackErr != nil {
		return fmt.Errorf("%w (rollback failed: %v)", operationErr, rollbackErr)
	}
	return operationErr
}

func uniqueCleanupPaths(paths []string, excluded ...string) []string {
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, path := range excluded {
		excludedSet[path] = struct{}{}
	}
	seen := make(map[string]struct{}, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := excludedSet[path]; ok {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	return unique
}

func validateVersionedDestination(target, kind string) error {
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("check existing %s %q: %w", kind, target, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to replace %s %q: existing path is not a regular file", kind, target)
	}
	return nil
}

// WrapperUpdateTransaction describes a stable-wrapper update that can be
// rolled back until Commit is called. It is used by offline rollback, where
// the already-installed artifacts must not be rewritten.
type WrapperUpdateTransaction struct {
	wrapperPath    string
	temporaryPath  string
	backup         *transactionBackup
	committed      bool
	cleanupWarning []error
	finalized      bool
}

// PrepareWrapperUpdate stages and activates a wrapper pointing at realPath.
// The caller owns the state update and can call Rollback if that update fails.
func PrepareWrapperUpdate(wrapperPath, repo, realPath, usageLog string) (*WrapperUpdateTransaction, error) {
	if wrapperPath == "" {
		return nil, fmt.Errorf("wrapper path is empty")
	}
	if realPath == "" {
		return nil, fmt.Errorf("real artifact path is empty")
	}
	if err := validateVersionedDestination(wrapperPath, "wrapper"); err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(wrapperPath); err == nil && !wrapper.IsMarkerLine(string(data)) {
		return nil, fmt.Errorf("refusing to overwrite foreign file %q", wrapperPath)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read existing wrapper %q: %w", wrapperPath, err)
	}

	wrapperScript, err := managedWrapperScript(repo, path.Base(wrapperPath), realPath, usageLog)
	if err != nil {
		return nil, err
	}
	return prepareWrapperScriptUpdate(wrapperPath, wrapperScript)
}

// PrepareShimWrapperUpdate stages and activates a wrapper that always points
// to the managed shim runtime. It is used by project activation, where the
// artifact may not exist yet and the wrapper must retain TASK-0058 dispatch
// semantics.
func PrepareShimWrapperUpdate(wrapperPath, repo, runtimePath, targetName string) (*WrapperUpdateTransaction, error) {
	if wrapperPath == "" {
		return nil, fmt.Errorf("wrapper path is empty")
	}
	if runtimePath == "" {
		return nil, fmt.Errorf("shim runtime path is empty")
	}
	if err := validateWrapperUpdateTarget(wrapperPath); err != nil {
		return nil, err
	}
	return prepareWrapperScriptUpdate(wrapperPath, wrapper.GenerateShim(repo, runtimePath, targetName))
}

func prepareWrapperScriptUpdate(wrapperPath string, wrapperScript []byte) (*WrapperUpdateTransaction, error) {
	temporary, err := prepareExecutable(wrapperScript, wrapperPath)
	if err != nil {
		return nil, err
	}
	transaction := &WrapperUpdateTransaction{wrapperPath: wrapperPath, temporaryPath: temporary}
	backup, existed, err := backupExisting(wrapperPath)
	if err != nil {
		if cleanupErr := removeIfExists(temporary); cleanupErr != nil {
			return nil, fmt.Errorf("%w (cleanup failed: %v)", err, cleanupErr)
		}
		return nil, err
	}
	if existed {
		transaction.backup = &transactionBackup{target: wrapperPath, backup: backup}
	}
	if err := installRename(temporary, wrapperPath); err != nil {
		return nil, abortWrapperUpdate(transaction, fmt.Errorf("activate wrapper %q: %w", wrapperPath, err))
	}
	transaction.committed = true
	transaction.temporaryPath = ""
	return transaction, nil
}

func validateWrapperUpdateTarget(wrapperPath string) error {
	if err := validateVersionedDestination(wrapperPath, "wrapper"); err != nil {
		return err
	}
	if data, err := os.ReadFile(wrapperPath); err == nil && !wrapper.IsMarkerLine(string(data)) {
		return fmt.Errorf("refusing to overwrite foreign file %q", wrapperPath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read existing wrapper %q: %w", wrapperPath, err)
	}
	return nil
}

// WrapperPath returns the stable wrapper path affected by the update.
func (t *WrapperUpdateTransaction) WrapperPath() string {
	if t == nil {
		return ""
	}
	return t.wrapperPath
}

// Rollback restores the wrapper that was active before the update.
func (t *WrapperUpdateTransaction) Rollback() error {
	if t == nil || t.finalized {
		return nil
	}
	return t.rollback()
}

// Commit finalizes the wrapper update and removes its temporary backup.
func (t *WrapperUpdateTransaction) Commit() error {
	if t == nil || t.finalized {
		return nil
	}
	if t.backup != nil {
		if err := installRemove(t.backup.backup); err != nil && !os.IsNotExist(err) {
			t.cleanupWarning = append(t.cleanupWarning, fmt.Errorf("remove previous wrapper backup %q: %w", t.backup.backup, err))
		}
	}
	if t.temporaryPath != "" {
		if err := installRemove(t.temporaryPath); err != nil && !os.IsNotExist(err) {
			t.cleanupWarning = append(t.cleanupWarning, fmt.Errorf("remove temporary wrapper file %q: %w", t.temporaryPath, err))
		}
	}
	t.finalized = true
	return nil
}

// CleanupWarnings returns non-fatal cleanup errors collected by Commit.
func (t *WrapperUpdateTransaction) CleanupWarnings() []error {
	if t == nil || len(t.cleanupWarning) == 0 {
		return nil
	}
	warnings := make([]error, len(t.cleanupWarning))
	copy(warnings, t.cleanupWarning)
	return warnings
}

func (t *WrapperUpdateTransaction) rollback() error {
	var firstErr error
	if t.committed {
		if err := removeIfExists(t.wrapperPath); err != nil {
			firstErr = err
		}
	}
	if t.backup != nil {
		if err := installRename(t.backup.backup, t.backup.target); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("restore wrapper %q: %w", t.backup.target, err)
			}
		} else if err := removeIfExists(t.backup.backup); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove restored wrapper backup %q: %w", t.backup.backup, err)
		}
	}
	if t.temporaryPath != "" {
		if err := removeIfExists(t.temporaryPath); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	t.finalized = true
	return firstErr
}

func abortWrapperUpdate(transaction *WrapperUpdateTransaction, operationErr error) error {
	if rollbackErr := transaction.rollback(); rollbackErr != nil {
		return fmt.Errorf("%w (rollback failed: %v)", operationErr, rollbackErr)
	}
	return operationErr
}

// Rollback restores the files that were active before the upgrade.
func (t *InstallTransaction) Rollback() error {
	if t == nil || t.finalized {
		return nil
	}
	return t.rollback()
}

// Commit removes the previous-version backups and makes the upgrade final.
// Backup cleanup is best effort: a cleanup failure cannot make an already
// persisted new state unsafe, and the next upgrade can remove the leftover.
func (t *InstallTransaction) Commit() error {
	if t == nil || t.finalized {
		return nil
	}
	for _, backup := range t.backups {
		if err := installRemove(backup.backup); err != nil && !os.IsNotExist(err) {
			t.cleanupWarnings = append(t.cleanupWarnings, fmt.Errorf("remove previous install backup %q: %w", backup.backup, err))
		}
	}
	for _, temporary := range t.temporaryPaths {
		if err := installRemove(temporary); err != nil && !os.IsNotExist(err) {
			t.cleanupWarnings = append(t.cleanupWarnings, fmt.Errorf("remove temporary install file %q: %w", temporary, err))
		}
	}
	t.finalized = true
	return nil
}

// CleanupWarnings returns non-fatal cleanup errors collected by Commit.
func (t *InstallTransaction) CleanupWarnings() []error {
	if t == nil || len(t.cleanupWarnings) == 0 {
		return nil
	}
	warnings := make([]error, len(t.cleanupWarnings))
	copy(warnings, t.cleanupWarnings)
	return warnings
}

func (t *InstallTransaction) rollback() error {
	var firstErr error
	if t.wrapperCommitted {
		if err := removeIfExists(t.wrapperPath); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if t.realCommitted {
		if err := removeIfExists(t.realPath); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	restored := make([]bool, len(t.backups))
	for i := len(t.backups) - 1; i >= 0; i-- {
		backup := t.backups[i]
		if err := installRename(backup.backup, backup.target); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("restore %q: %w", backup.target, err)
			}
			continue
		}
		restored[i] = true
	}
	if err := t.cleanupTemporary(); err != nil && firstErr == nil {
		firstErr = err
	}
	for i, backup := range t.backups {
		if !restored[i] {
			continue
		}
		if err := removeIfExists(backup.backup); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove restored backup %q: %w", backup.backup, err)
		}
	}
	t.finalized = true
	return firstErr
}

func (t *InstallTransaction) cleanup() error {
	var firstErr error
	if err := t.cleanupTemporary(); err != nil {
		firstErr = err
	}
	for _, backup := range t.backups {
		if err := removeIfExists(backup.backup); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t *InstallTransaction) cleanupTemporary() error {
	var firstErr error
	for _, temporary := range t.temporaryPaths {
		if err := removeIfExists(temporary); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func abortUpgrade(transaction *InstallTransaction, operationErr error) error {
	if rollbackErr := transaction.rollback(); rollbackErr != nil {
		return fmt.Errorf("%w (rollback failed: %v)", operationErr, rollbackErr)
	}
	return operationErr
}

func backupExisting(target string) (string, bool, error) {
	if _, err := os.Lstat(target); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("check existing install file %q: %w", target, err)
	}
	backup := target + ".old"
	if _, err := os.Lstat(backup); err == nil {
		return "", false, fmt.Errorf("previous upgrade backup already exists at %q", backup)
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("check upgrade backup %q: %w", backup, err)
	}
	if err := installRename(target, backup); err != nil {
		return "", false, fmt.Errorf("backup existing install file %q: %w", target, err)
	}
	return backup, true, nil
}

func removeIfExists(path string) error {
	if err := installRemove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeExecutable atomically writes data to target with executable
// permissions.
func writeExecutable(data []byte, target string) error {
	temporaryPath, err := prepareExecutable(data, target)
	if err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("install file at %q: %w", target, err)
	}
	return nil
}

// ReplaceExecutable atomically replaces target with executable data and gives
// the resulting file executable permissions.
func ReplaceExecutable(data []byte, target string) error {
	return writeExecutable(data, target)
}

func prepareExecutable(data []byte, target string) (string, error) {
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return "", fmt.Errorf("create install directory %q: %w", directory, err)
	}

	temporary, err := os.CreateTemp(directory, ".zenget-*")
	if err != nil {
		return "", fmt.Errorf("create temporary install file in %q: %w", directory, err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary install file %q: %w", temporaryPath, err)
	}
	if err := temporary.Chmod(0755); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("set installed file permissions: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary install file %q: %w", temporaryPath, err)
	}

	removeTemporary = false
	return temporaryPath, nil
}

package install

import (
	"fmt"
	"sort"
	"strings"

	"zenget/internal/assetselector"
	"zenget/internal/github"
)

// AssetDisposition describes why a release asset was or was not selected.
// Values are stable strings intended for machine-readable inspection output.
type AssetDisposition string

const (
	// DispositionSelected identifies the asset selected for installation.
	DispositionSelected AssetDisposition = "selected"
	// DispositionCompatible identifies an installable candidate that lost to a
	// better compatible candidate.
	DispositionCompatible AssetDisposition = "compatible"
	// DispositionChecksum identifies checksum metadata that is not executable.
	DispositionChecksum AssetDisposition = "checksum"
	// DispositionSignature identifies signature metadata that is not executable.
	DispositionSignature AssetDisposition = "signature"
	// DispositionSource identifies a source archive that is not executable.
	DispositionSource AssetDisposition = "source"
	// DispositionNoise identifies an installer, manifest, or other known noise
	// asset that is excluded from normal selection.
	DispositionNoise AssetDisposition = "noise"
	// DispositionPlatformMismatch identifies an asset whose name does not fit
	// the requested operating system or architecture.
	DispositionPlatformMismatch AssetDisposition = "platform_mismatch"
	// DispositionLibcMismatch identifies an asset incompatible with the known
	// local Linux libc.
	DispositionLibcMismatch AssetDisposition = "libc_mismatch"
	// DispositionUnsupportedFormat identifies a package format unsupported by
	// zenget's binary extractor.
	DispositionUnsupportedFormat AssetDisposition = "unsupported_format"
	// DispositionLowerFormatPriority identifies a candidate that lost because
	// another compatible format has higher priority.
	DispositionLowerFormatPriority AssetDisposition = "lower_format_priority"
	// DispositionNameTieBreak identifies a candidate that lost a format tie by
	// deterministic name ordering.
	DispositionNameTieBreak AssetDisposition = "name_tie_break"
	// DispositionAmbiguous identifies assets tied by an explicit override.
	DispositionAmbiguous AssetDisposition = "ambiguous"
	// DispositionNoMatch identifies an asset that does not match an explicit
	// override.
	DispositionNoMatch AssetDisposition = "no_match"
	// DispositionSelectorExcluded identifies an installable asset excluded by
	// a reusable substring or regex selector.
	DispositionSelectorExcluded AssetDisposition = "selector_excluded"
)

// AssetDiagnostic describes one release asset without downloading it.
type AssetDiagnostic struct {
	ID               int64            `json:"id,omitempty"`
	Name             string           `json:"name"`
	Size             int64            `json:"size,omitempty"`
	Digest           string           `json:"digest,omitempty"`
	Format           string           `json:"format"`
	Disposition      AssetDisposition `json:"disposition"`
	Reason           string           `json:"reason"`
	ChecksumEvidence []string         `json:"checksum_evidence,omitempty"`
}

// AssetSelection contains the selected provider asset and deterministic
// diagnostics for every asset in the release. A selection error is returned
// separately so callers can still render this complete report.
type AssetSelection struct {
	SelectedAsset *github.Asset
	Assets        []AssetDiagnostic
}

// AnalyzeAssetSelection applies the same release-asset decision rules as
// SelectAsset and returns metadata-only diagnostics for every release asset.
// An explicit asset override uses the same exact-or-unique-substring behavior
// as installation. The returned diagnostics are sorted by asset name and do
// not download, open, or otherwise inspect any asset content.
func AnalyzeAssetSelection(rel *github.Release, goos, goarch, assetOverride string) (AssetSelection, error) {
	return analyzeAssetSelection(rel, goos, goarch, assetOverride, nil)
}

// AnalyzeAssetSelectionWithSelector applies a validated reusable selector to
// non-noise assets before running the normal platform, libc, format, and name
// selection rules. The returned diagnostics still include every release asset.
func AnalyzeAssetSelectionWithSelector(rel *github.Release, goos, goarch string, selector *assetselector.Selector) (AssetSelection, error) {
	if selector == nil {
		return AnalyzeAssetSelection(rel, goos, goarch, "")
	}
	if err := selector.Validate(); err != nil {
		return AssetSelection{}, err
	}
	return analyzeAssetSelection(rel, goos, goarch, "", selector)
}

func analyzeAssetSelection(rel *github.Release, goos, goarch, assetOverride string, selector *assetselector.Selector) (AssetSelection, error) {
	selection := AssetSelection{}
	if rel != nil {
		selection.Assets = newAssetDiagnostics(rel.Assets)
	}

	selectionRelease := rel
	selectionAssets := []github.Asset(nil)
	if rel != nil {
		selectionAssets = rel.Assets
	}
	if selector != nil && rel != nil {
		selectionAssets = filterSelectorCandidates(rel.Assets, selector)
		selectionReleaseCopy := *rel
		selectionReleaseCopy.Assets = selectionAssets
		selectionRelease = &selectionReleaseCopy
	}

	var selected github.Asset
	var err error
	if selector != nil && rel != nil && len(rel.Assets) > 0 && len(selectionAssets) == 0 {
		err = noSelectorMatchError(rel, selector)
		markSelectorDiagnostics(selection.Assets, rel.Assets, selector, err)
	} else {
		selected, err = selectAssetWithLibc(selectionRelease, goos, goarch, assetOverride, hostLibcFor(goos))
	}
	if rel == nil {
		return selection, err
	}
	if len(rel.Assets) == 0 {
		return selection, err
	}

	if selector != nil {
		if len(selectionAssets) > 0 {
			analyzeAutomatic(selection.Assets, selectionAssets, goos, goarch, selected, err)
		}
		markSelectorDiagnostics(selection.Assets, rel.Assets, selector, err)
	} else if assetOverride != "" {
		analyzeOverride(selection.Assets, rel.Assets, assetOverride, selected, err)
	} else {
		analyzeAutomatic(selection.Assets, rel.Assets, goos, goarch, selected, err)
	}
	if err == nil {
		selection.SelectedAsset = cloneAsset(selected)
	}
	sortAssetDiagnostics(selection.Assets)
	return selection, err
}

// SelectAsset picks the best downloadable asset from a release while keeping
// the historical return value and error behavior backed by the diagnostic
// decision engine.
func selectAssetFromAnalysis(rel *github.Release, goos, goarch, assetOverride string) (github.Asset, error) {
	selection, err := AnalyzeAssetSelection(rel, goos, goarch, assetOverride)
	if err != nil {
		return github.Asset{}, err
	}
	if selection.SelectedAsset == nil {
		return github.Asset{}, fmt.Errorf("asset selection produced no selected asset")
	}
	return *selection.SelectedAsset, nil
}

func newAssetDiagnostics(assets []github.Asset) []AssetDiagnostic {
	diagnostics := make([]AssetDiagnostic, len(assets))
	for index, asset := range assets {
		disposition, reason := initialDisposition(asset.Name)
		diagnostics[index] = AssetDiagnostic{
			ID: asset.ID, Name: asset.Name, Size: asset.Size, Digest: asset.Digest,
			Format: formatName(asset.Name), Disposition: disposition, Reason: reason,
			ChecksumEvidence: checksumEvidence(assets, asset),
		}
	}
	return diagnostics
}

func initialDisposition(name string) (AssetDisposition, string) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".sha256"), strings.HasSuffix(lower, ".sha512"), strings.HasSuffix(lower, ".md5"), strings.HasSuffix(lower, "checksums.txt"), strings.HasSuffix(lower, "sha256.sum"), strings.HasSuffix(lower, "sha256sums"), strings.HasSuffix(lower, "checksums.sha256"):
		return DispositionChecksum, "checksum metadata is excluded from binary selection"
	case strings.HasSuffix(lower, ".sig"), strings.HasSuffix(lower, ".asc"), strings.HasSuffix(lower, ".pem"):
		return DispositionSignature, "signature metadata is excluded from binary selection"
	case strings.HasSuffix(lower, "source.tar.gz"), strings.HasSuffix(lower, "source.zip"):
		return DispositionSource, "source archive is excluded from binary selection"
	case strings.HasSuffix(lower, ".deb"), strings.HasSuffix(lower, ".rpm"), strings.HasSuffix(lower, ".msi"), strings.HasSuffix(lower, ".dmg"), strings.HasSuffix(lower, ".apk"), strings.HasSuffix(lower, ".pkg"):
		return DispositionUnsupportedFormat, "package format is not supported by binary extraction"
	case strings.HasSuffix(lower, ".sbom"), strings.HasSuffix(lower, ".sh"), strings.HasSuffix(lower, ".ps1"), strings.HasSuffix(lower, ".bat"), strings.HasSuffix(lower, "dist-manifest.json"):
		return DispositionNoise, "non-binary release metadata is excluded from selection"
	default:
		return DispositionCompatible, "installable candidate"
	}
}

func formatName(name string) string {
	switch DetectFormat(name) {
	case FormatTarGz:
		return "tar.gz"
	case FormatTarXz:
		return "tar.xz"
	case FormatTarZst:
		return "tar.zst"
	case FormatTarBz2:
		return "tar.bz2"
	case FormatZip:
		return "zip"
	case FormatTar:
		return "tar"
	case FormatGz:
		return "gz"
	case FormatBz2:
		return "bz2"
	case FormatXz:
		return "xz"
	case FormatZst:
		return "zst"
	default:
		return "raw"
	}
}

func checksumEvidence(assets []github.Asset, candidate github.Asset) []string {
	var evidence []string
	if candidate.Digest != "" {
		evidence = append(evidence, "release digest")
	}
	wantedSidecars := map[string]struct{}{
		strings.ToLower(candidate.Name + ".sha256"): {},
		strings.ToLower(candidate.Name + ".sha512"): {},
	}
	for _, asset := range assets {
		lower := strings.ToLower(asset.Name)
		if _, ok := wantedSidecars[lower]; ok {
			evidence = append(evidence, asset.Name)
			continue
		}
		switch lower {
		case "checksums.txt", "sha256.sum", "sha256sums", "checksums.sha256":
			evidence = append(evidence, asset.Name)
		}
	}
	sort.Strings(evidence)
	return evidence
}

func analyzeOverride(diagnostics []AssetDiagnostic, assets []github.Asset, override string, selected github.Asset, err error) {
	if err == nil {
		setDisposition(diagnostics, selected, DispositionSelected, "selected by explicit --asset override")
		for index, asset := range assets {
			if asset == selected {
				continue
			}
			if isInitialExclusion(diagnostics[index].Disposition) {
				continue
			}
			setDispositionAt(diagnostics, index, DispositionNoMatch, "not selected by explicit --asset override")
		}
		return
	}

	lowerOverride := strings.ToLower(override)
	var exact, substring []int
	for index, asset := range assets {
		if strings.EqualFold(asset.Name, override) {
			exact = append(exact, index)
		}
		if strings.Contains(strings.ToLower(asset.Name), lowerOverride) {
			substring = append(substring, index)
		}
	}
	if len(exact) > 1 || (len(exact) == 0 && len(substring) > 1) {
		for _, index := range substring {
			setDispositionAt(diagnostics, index, DispositionAmbiguous, "multiple assets match the explicit --asset override")
		}
	}
	for index := range diagnostics {
		if isInitialExclusion(diagnostics[index].Disposition) || diagnostics[index].Disposition == DispositionAmbiguous {
			continue
		}
		setDispositionAt(diagnostics, index, DispositionNoMatch, "does not match the explicit --asset override")
	}
}

func analyzeAutomatic(diagnostics []AssetDiagnostic, assets []github.Asset, goos, goarch string, selected github.Asset, err error) {
	candidates := filterNoise(assets)
	if len(candidates) == 0 {
		return
	}
	host := hostLibcFor(goos)
	if len(candidates) == 1 {
		if err == nil {
			setDisposition(diagnostics, selected, DispositionSelected, "only installable candidate")
		} else {
			setDisposition(diagnostics, candidates[0], DispositionLibcMismatch, "candidate is incompatible with the known host libc")
		}
		return
	}

	platformCandidates := filterPlatform(candidates, goos, goarch)
	if len(platformCandidates) == 0 {
		for _, candidate := range candidates {
			setDisposition(diagnostics, candidate, DispositionPlatformMismatch, fmt.Sprintf("asset name does not match target %s/%s", goos, goarch))
		}
		return
	}
	for _, candidate := range candidates {
		if !containsAsset(platformCandidates, candidate) {
			setDisposition(diagnostics, candidate, DispositionPlatformMismatch, fmt.Sprintf("asset name does not match target %s/%s", goos, goarch))
		}
	}

	bitnessCandidates := filterBitness(platformCandidates, goarch)
	if len(bitnessCandidates) == 0 {
		for _, candidate := range platformCandidates {
			setDisposition(diagnostics, candidate, DispositionPlatformMismatch, fmt.Sprintf("asset only matches a narrower architecture than %s", goarch))
		}
		return
	}
	for _, candidate := range platformCandidates {
		if !containsAsset(bitnessCandidates, candidate) {
			setDisposition(diagnostics, candidate, DispositionPlatformMismatch, fmt.Sprintf("asset only matches a narrower architecture than %s", goarch))
		}
	}

	preferredCandidates := preferArchitectureSpecific(bitnessCandidates, goos, goarch)
	for _, candidate := range bitnessCandidates {
		if !containsAsset(preferredCandidates, candidate) {
			setDisposition(diagnostics, candidate, DispositionCompatible, "generic or universal candidate was deprioritized")
		}
	}

	libcCandidates := filterLibc(preferredCandidates, host)
	if len(libcCandidates) == 0 {
		for _, candidate := range preferredCandidates {
			setDisposition(diagnostics, candidate, DispositionLibcMismatch, "asset is incompatible with the known host libc")
		}
		return
	}
	for _, candidate := range preferredCandidates {
		if containsAsset(libcCandidates, candidate) {
			continue
		}
		if host == libcMusl {
			setDisposition(diagnostics, candidate, DispositionLibcMismatch, "explicit gnu/glibc asset is not usable on musl")
		} else {
			setDisposition(diagnostics, candidate, DispositionCompatible, "libc-specific candidate was deprioritized")
		}
	}

	if err != nil {
		for _, candidate := range libcCandidates {
			setDisposition(diagnostics, candidate, DispositionCompatible, "compatible candidate was not uniquely selected")
		}
		return
	}
	setDisposition(diagnostics, selected, DispositionSelected, "selected by format priority and deterministic name ordering")
	for _, candidate := range libcCandidates {
		if candidate == selected {
			continue
		}
		disposition := DispositionCompatible
		reason := "compatible candidate was not selected"
		if formatRank(candidate.Name) > formatRank(selected.Name) {
			disposition = DispositionLowerFormatPriority
			reason = fmt.Sprintf("format has lower priority than %s", formatName(selected.Name))
		} else if formatRank(candidate.Name) == formatRank(selected.Name) && candidate.Name != selected.Name {
			disposition = DispositionNameTieBreak
			reason = fmt.Sprintf("name loses deterministic tie-break to %s", selected.Name)
		}
		setDisposition(diagnostics, candidate, disposition, reason)
	}
}

func isInitialExclusion(disposition AssetDisposition) bool {
	switch disposition {
	case DispositionChecksum, DispositionSignature, DispositionSource, DispositionNoise, DispositionUnsupportedFormat:
		return true
	default:
		return false
	}
}

func setDisposition(diagnostics []AssetDiagnostic, asset github.Asset, disposition AssetDisposition, reason string) {
	for index, candidate := range diagnostics {
		if candidate.ID == asset.ID && candidate.Name == asset.Name && candidate.Size == asset.Size && candidate.Digest == asset.Digest {
			setDispositionAt(diagnostics, index, disposition, reason)
			return
		}
	}
}

func setDispositionAt(diagnostics []AssetDiagnostic, index int, disposition AssetDisposition, reason string) {
	diagnostics[index].Disposition = disposition
	diagnostics[index].Reason = reason
}

func containsAsset(assets []github.Asset, wanted github.Asset) bool {
	for _, asset := range assets {
		if asset == wanted {
			return true
		}
	}
	return false
}

func cloneAsset(asset github.Asset) *github.Asset {
	copy := asset
	return &copy
}

func sortAssetDiagnostics(diagnostics []AssetDiagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].Name != diagnostics[j].Name {
			return diagnostics[i].Name < diagnostics[j].Name
		}
		return diagnostics[i].ID < diagnostics[j].ID
	})
}

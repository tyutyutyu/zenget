package install

import (
	"fmt"
	"strings"

	"zenget/internal/assetselector"
	"zenget/internal/github"
)

// SelectAssetWithSelector selects an asset using a reusable substring or RE2
// selector followed by the normal platform and format rules.
func SelectAssetWithSelector(rel *github.Release, goos, goarch string, selector *assetselector.Selector) (github.Asset, error) {
	selection, err := AnalyzeAssetSelectionWithSelector(rel, goos, goarch, selector)
	if err != nil {
		return github.Asset{}, err
	}
	if selection.SelectedAsset == nil {
		return github.Asset{}, fmt.Errorf("asset selection produced no selected asset")
	}
	return *selection.SelectedAsset, nil
}

func filterSelectorCandidates(assets []github.Asset, selector *assetselector.Selector) []github.Asset {
	if selector == nil {
		return filterNoise(assets)
	}
	var candidates []github.Asset
	for _, asset := range filterNoise(assets) {
		if selector.Match(asset.Name) {
			candidates = append(candidates, asset)
		}
	}
	return candidates
}

func noSelectorMatchError(rel *github.Release, selector *assetselector.Selector) error {
	return fmt.Errorf(
		"release %q has no assets matching %s selector %q after noise filtering; use --asset to select one explicitly",
		rel.TagName,
		selector.Type,
		selector.Pattern,
	)
}

func markSelectorDiagnostics(diagnostics []AssetDiagnostic, assets []github.Asset, selector *assetselector.Selector, selectionErr error) {
	if selector == nil {
		return
	}
	for index, asset := range assets {
		if isInitialExclusion(diagnostics[index].Disposition) {
			continue
		}
		if !selector.Match(asset.Name) {
			setDispositionAt(diagnostics, index, DispositionSelectorExcluded, fmt.Sprintf("excluded by %s selector %q", selector.Type, selector.Pattern))
		}
	}
	if selectionErr == nil || !strings.Contains(selectionErr.Error(), "multiple downloadable assets") {
		return
	}
	for index, asset := range assets {
		if isInitialExclusion(diagnostics[index].Disposition) || !selector.Match(asset.Name) {
			continue
		}
		setDispositionAt(diagnostics, index, DispositionAmbiguous, fmt.Sprintf("%s selector matched multiple assets without a unique platform match", selector.Type))
	}
}

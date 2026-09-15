package install

import (
	"strings"
	"testing"

	"zenget/internal/assetselector"
	"zenget/internal/github"
)

func TestSelectAssetWithSelectorFiltersBeforeNormalSelection(t *testing.T) {
	release := &github.Release{
		TagName: "v2.0.0",
		Assets: []github.Asset{
			{Name: "tool-linux-amd64-v2.zip"},
			{Name: "tool-linux-amd64-v2.tar.gz"},
			{Name: "tool-linux-arm64-v2.tar.gz"},
			{Name: "tool-linux-amd64-v2.tar.gz.sha256"},
			{Name: "tool-linux-amd64-v1.tar.gz"},
		},
	}
	selector := assetselector.NewSubstring("linux-amd64-v2")

	got, err := SelectAssetWithSelector(release, "linux", "amd64", &selector)
	if err != nil {
		t.Fatalf("SelectAssetWithSelector() error = %v", err)
	}
	if got.Name != "tool-linux-amd64-v2.tar.gz" {
		t.Fatalf("selected asset = %q, want v2 tar.gz", got.Name)
	}

	selection, err := AnalyzeAssetSelectionWithSelector(release, "linux", "amd64", &selector)
	if err != nil {
		t.Fatalf("AnalyzeAssetSelectionWithSelector() error = %v", err)
	}
	byName := make(map[string]AssetDiagnostic, len(selection.Assets))
	for _, diagnostic := range selection.Assets {
		byName[diagnostic.Name] = diagnostic
	}
	if got := byName["tool-linux-amd64-v1.tar.gz"].Disposition; got != DispositionSelectorExcluded {
		t.Errorf("old version disposition = %q, want %q", got, DispositionSelectorExcluded)
	}
	if got := byName["tool-linux-arm64-v2.tar.gz"].Disposition; got != DispositionSelectorExcluded {
		t.Errorf("other architecture disposition = %q, want %q", got, DispositionSelectorExcluded)
	}
	if got := byName["tool-linux-amd64-v2.tar.gz.sha256"].Disposition; got != DispositionChecksum {
		t.Errorf("checksum disposition = %q, want %q", got, DispositionChecksum)
	}
}

func TestAssetSelectorCanBeReusedAcrossVersionedReleases(t *testing.T) {
	selector := assetselector.NewRegex(`^tool-linux-amd64-v[0-9.]+\.tar\.gz$`)
	for _, tag := range []string{"v1.0.0", "v2.0.0"} {
		release := &github.Release{TagName: tag, Assets: []github.Asset{
			{Name: "tool-linux-amd64-" + tag + ".tar.gz"},
			{Name: "tool-linux-amd64-" + tag + ".zip"},
		}}
		got, err := SelectAssetWithSelector(release, "linux", "amd64", &selector)
		if err != nil {
			t.Fatalf("release %s: SelectAssetWithSelector() error = %v", tag, err)
		}
		if got.Name != "tool-linux-amd64-"+tag+".tar.gz" {
			t.Errorf("release %s selected %q", tag, got.Name)
		}
	}
}

func TestAnalyzeAssetSelectionSelectorFailuresDoNotFallback(t *testing.T) {
	t.Run("zero matches", func(t *testing.T) {
		release := &github.Release{TagName: "v1.0.0", Assets: []github.Asset{{Name: "tool-linux-amd64.tar.gz"}}}
		selector := assetselector.NewSubstring("windows")
		selection, err := AnalyzeAssetSelectionWithSelector(release, "linux", "amd64", &selector)
		if err == nil || !strings.Contains(err.Error(), "no assets matching substring selector") {
			t.Fatalf("error = %v, want selector no-match error", err)
		}
		if got := selection.Assets[0].Disposition; got != DispositionSelectorExcluded {
			t.Fatalf("disposition = %q, want %q", got, DispositionSelectorExcluded)
		}
	})

	t.Run("ambiguous after selector", func(t *testing.T) {
		release := &github.Release{TagName: "v1.0.0", Assets: []github.Asset{
			{Name: "tool-a"}, {Name: "tool-b"}, {Name: "checksums.txt"},
		}}
		selector := assetselector.NewSubstring("tool")
		selection, err := AnalyzeAssetSelectionWithSelector(release, "linux", "amd64", &selector)
		if err == nil || !strings.Contains(err.Error(), "multiple downloadable assets") {
			t.Fatalf("error = %v, want ambiguity error", err)
		}
		for _, diagnostic := range selection.Assets {
			switch diagnostic.Name {
			case "tool-a", "tool-b":
				if diagnostic.Disposition != DispositionAmbiguous {
					t.Errorf("%s disposition = %q, want %q", diagnostic.Name, diagnostic.Disposition, DispositionAmbiguous)
				}
			case "checksums.txt":
				if diagnostic.Disposition != DispositionChecksum {
					t.Errorf("checksum disposition = %q, want %q", diagnostic.Disposition, DispositionChecksum)
				}
			}
		}
	})

	t.Run("invalid regex", func(t *testing.T) {
		selector := assetselector.NewRegex("[")
		if _, err := SelectAssetWithSelector(nil, "linux", "amd64", &selector); err == nil || !strings.Contains(err.Error(), "invalid asset selector regex") {
			t.Fatalf("error = %v, want invalid regex error", err)
		}
	})
}

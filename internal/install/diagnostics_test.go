package install

import (
	"reflect"
	"strings"
	"testing"

	"zenget/internal/github"
)

func TestAnalyzeAssetSelectionClassifiesAndSortsAssets(t *testing.T) {
	release := &github.Release{
		TagName: "v1.2.3",
		Assets: []github.Asset{
			{ID: 2, Name: "tool-linux-amd64.zip", Size: 20},
			{ID: 1, Name: "tool-linux-amd64.tar.gz", Size: 30, Digest: "sha256:abc"},
			{ID: 3, Name: "tool-linux-amd64.tar.gz.sha256", Size: 40},
			{ID: 4, Name: "tool-linux-amd64.tar.gz.sig", Size: 50},
			{ID: 5, Name: "tool-source.tar.gz", Size: 60},
			{ID: 6, Name: "tool-linux-amd64.deb", Size: 70},
			{ID: 7, Name: "tool-darwin-amd64.tar.gz", Size: 80},
			{ID: 8, Name: "tool-linux-amd64.sh", Size: 90},
			{ID: 9, Name: "checksums.txt", Size: 100},
		},
	}

	selection, err := AnalyzeAssetSelection(release, "linux", "amd64", "")
	if err != nil {
		t.Fatalf("AnalyzeAssetSelection() error = %v", err)
	}
	if selection.SelectedAsset == nil || selection.SelectedAsset.Name != "tool-linux-amd64.tar.gz" {
		t.Fatalf("AnalyzeAssetSelection() selected = %#v, want tar.gz asset", selection.SelectedAsset)
	}
	if got, want := len(selection.Assets), len(release.Assets); got != want {
		t.Fatalf("AnalyzeAssetSelection() diagnostics = %d, want %d", got, want)
	}

	wantNames := make([]string, len(release.Assets))
	for index, asset := range release.Assets {
		wantNames[index] = asset.Name
	}
	for index := range wantNames {
		for previous := index + 1; previous < len(wantNames); previous++ {
			if wantNames[previous] < wantNames[index] {
				wantNames[index], wantNames[previous] = wantNames[previous], wantNames[index]
			}
		}
	}
	gotNames := make([]string, len(selection.Assets))
	for index, asset := range selection.Assets {
		gotNames[index] = asset.Name
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("diagnostic order = %#v, want %#v", gotNames, wantNames)
	}

	byName := make(map[string]AssetDiagnostic, len(selection.Assets))
	for _, diagnostic := range selection.Assets {
		byName[diagnostic.Name] = diagnostic
	}
	assertDisposition := func(name string, want AssetDisposition) {
		t.Helper()
		if got := byName[name].Disposition; got != want {
			t.Errorf("%s disposition = %q, want %q", name, got, want)
		}
	}
	assertDisposition("tool-linux-amd64.tar.gz", DispositionSelected)
	assertDisposition("tool-linux-amd64.zip", DispositionLowerFormatPriority)
	assertDisposition("tool-linux-amd64.tar.gz.sha256", DispositionChecksum)
	assertDisposition("tool-linux-amd64.tar.gz.sig", DispositionSignature)
	assertDisposition("tool-source.tar.gz", DispositionSource)
	assertDisposition("tool-linux-amd64.deb", DispositionUnsupportedFormat)
	assertDisposition("tool-darwin-amd64.tar.gz", DispositionPlatformMismatch)
	assertDisposition("tool-linux-amd64.sh", DispositionNoise)
	assertDisposition("checksums.txt", DispositionChecksum)

	wantEvidence := []string{"checksums.txt", "release digest", "tool-linux-amd64.tar.gz.sha256"}
	if got := byName["tool-linux-amd64.tar.gz"].ChecksumEvidence; !reflect.DeepEqual(got, wantEvidence) {
		t.Errorf("checksum evidence = %#v, want %#v", got, wantEvidence)
	}
}

func TestAnalyzeAssetSelectionReportsTieBreak(t *testing.T) {
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "tool-linux-amd64-z.tar.gz"},
			{Name: "tool-linux-amd64-a.tar.gz"},
		},
	}

	selection, err := AnalyzeAssetSelection(release, "linux", "amd64", "")
	if err != nil {
		t.Fatalf("AnalyzeAssetSelection() error = %v", err)
	}
	if selection.SelectedAsset == nil || selection.SelectedAsset.Name != "tool-linux-amd64-a.tar.gz" {
		t.Fatalf("selected asset = %#v, want alphabetically first archive", selection.SelectedAsset)
	}
	for _, diagnostic := range selection.Assets {
		if diagnostic.Name == "tool-linux-amd64-z.tar.gz" && diagnostic.Disposition != DispositionNameTieBreak {
			t.Errorf("losing asset disposition = %q, want %q", diagnostic.Disposition, DispositionNameTieBreak)
		}
	}
}

func TestAnalyzeAssetSelectionExplicitOverrideDiagnostics(t *testing.T) {
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "tool-linux-amd64.tar.gz"},
			{Name: "tool-darwin-amd64.tar.gz"},
			{Name: "tool-linux-amd64.zip"},
			{Name: "checksums.txt"},
		},
	}

	t.Run("selected override", func(t *testing.T) {
		selection, err := AnalyzeAssetSelection(release, "linux", "amd64", "darwin")
		if err != nil {
			t.Fatalf("AnalyzeAssetSelection() error = %v", err)
		}
		if selection.SelectedAsset == nil || selection.SelectedAsset.Name != "tool-darwin-amd64.tar.gz" {
			t.Fatalf("selected asset = %#v, want darwin asset", selection.SelectedAsset)
		}
		for _, diagnostic := range selection.Assets {
			switch diagnostic.Name {
			case "tool-darwin-amd64.tar.gz":
				if diagnostic.Disposition != DispositionSelected {
					t.Errorf("selected disposition = %q, want %q", diagnostic.Disposition, DispositionSelected)
				}
			case "checksums.txt":
				if diagnostic.Disposition != DispositionChecksum {
					t.Errorf("checksum disposition = %q, want %q", diagnostic.Disposition, DispositionChecksum)
				}
			default:
				if diagnostic.Disposition != DispositionNoMatch {
					t.Errorf("%s disposition = %q, want %q", diagnostic.Name, diagnostic.Disposition, DispositionNoMatch)
				}
			}
		}
	})

	t.Run("ambiguous override", func(t *testing.T) {
		selection, err := AnalyzeAssetSelection(release, "linux", "amd64", "linux")
		if err == nil || !strings.Contains(err.Error(), "ambiguous assets matching") {
			t.Fatalf("AnalyzeAssetSelection() error = %v, want ambiguous error", err)
		}
		for _, diagnostic := range selection.Assets {
			switch diagnostic.Name {
			case "tool-linux-amd64.tar.gz", "tool-linux-amd64.zip":
				if diagnostic.Disposition != DispositionAmbiguous {
					t.Errorf("%s disposition = %q, want %q", diagnostic.Name, diagnostic.Disposition, DispositionAmbiguous)
				}
			case "checksums.txt":
				if diagnostic.Disposition != DispositionChecksum {
					t.Errorf("checksum disposition = %q, want %q", diagnostic.Disposition, DispositionChecksum)
				}
			default:
				if diagnostic.Disposition != DispositionNoMatch {
					t.Errorf("%s disposition = %q, want %q", diagnostic.Name, diagnostic.Disposition, DispositionNoMatch)
				}
			}
		}
	})

	t.Run("missing override", func(t *testing.T) {
		selection, err := AnalyzeAssetSelection(release, "linux", "amd64", "windows")
		if err == nil || !strings.Contains(err.Error(), "no asset matching") {
			t.Fatalf("AnalyzeAssetSelection() error = %v, want no-match error", err)
		}
		for _, diagnostic := range selection.Assets {
			if diagnostic.Name == "checksums.txt" {
				if diagnostic.Disposition != DispositionChecksum {
					t.Errorf("checksum disposition = %q, want %q", diagnostic.Disposition, DispositionChecksum)
				}
				continue
			}
			if diagnostic.Disposition != DispositionNoMatch {
				t.Errorf("%s disposition = %q, want %q", diagnostic.Name, diagnostic.Disposition, DispositionNoMatch)
			}
		}
	})
}

func TestAnalyzeAssetSelectionMatchesLegacySelection(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		goarch   string
		override string
		release  *github.Release
	}{
		{
			name:   "format priority",
			goos:   "linux",
			goarch: "amd64",
			release: &github.Release{TagName: "v1.0.0", Assets: []github.Asset{
				{Name: "tool-linux-amd64.zip"}, {Name: "tool-linux-amd64.tar.gz"},
			}},
		},
		{
			name:     "explicit substring",
			goos:     "linux",
			goarch:   "amd64",
			override: "darwin",
			release: &github.Release{TagName: "v1.0.0", Assets: []github.Asset{
				{Name: "tool-linux-amd64"}, {Name: "tool-darwin-amd64"},
			}},
		},
		{
			name:   "no compatible platform",
			goos:   "linux",
			goarch: "amd64",
			release: &github.Release{TagName: "v1.0.0", Assets: []github.Asset{
				{Name: "tool-darwin-amd64"}, {Name: "tool-windows-amd64"},
			}},
		},
		{
			name:    "empty release",
			goos:    "linux",
			goarch:  "amd64",
			release: &github.Release{TagName: "v1.0.0"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want, wantErr := selectAssetWithLibc(test.release, test.goos, test.goarch, test.override, hostLibcFor(test.goos))
			selection, gotErr := AnalyzeAssetSelection(test.release, test.goos, test.goarch, test.override)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("AnalyzeAssetSelection() error = %v, legacy error = %v", gotErr, wantErr)
			}
			if wantErr != nil {
				if gotErr.Error() != wantErr.Error() {
					t.Errorf("AnalyzeAssetSelection() error = %q, legacy error = %q", gotErr, wantErr)
				}
				return
			}
			if selection.SelectedAsset == nil || *selection.SelectedAsset != want {
				t.Errorf("selected asset = %#v, legacy asset = %#v", selection.SelectedAsset, want)
			}
		})
	}
}

func TestAnalyzeAssetSelectionNilRelease(t *testing.T) {
	selection, err := AnalyzeAssetSelection(nil, "linux", "amd64", "")
	if err == nil || !strings.Contains(err.Error(), "nil release") {
		t.Fatalf("AnalyzeAssetSelection() error = %v, want nil-release error", err)
	}
	if selection.SelectedAsset != nil || len(selection.Assets) != 0 {
		t.Fatalf("AnalyzeAssetSelection(nil) = %#v, want empty selection", selection)
	}
}

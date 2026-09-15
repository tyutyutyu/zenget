package install

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/limits"
	"zenget/internal/wrapper"
)

func TestSelectAsset(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		goarch   string
		override string
		assets   []github.Asset
		want     github.Asset
		errText  string
	}{
		{
			name:    "no assets",
			errText: `release "v1.2.3" has no downloadable assets`,
		},
		{
			name:   "one asset",
			assets: []github.Asset{{Name: "tool-linux-amd64"}},
			want:   github.Asset{Name: "tool-linux-amd64"},
		},
		{
			name:   "single asset after noise filtering",
			assets: []github.Asset{{Name: "tool.deb"}, {Name: "tool-linux-amd64.tar.gz"}},
			want:   github.Asset{Name: "tool-linux-amd64.tar.gz"},
		},
		{
			name:   "multiple assets with linux amd64 match",
			assets: []github.Asset{{Name: "tool-linux-amd64"}, {Name: "tool-darwin-arm64"}},
			want:   github.Asset{Name: "tool-linux-amd64"},
		},
		{
			name:    "multiple assets without platform match",
			assets:  []github.Asset{{Name: "tool-a"}, {Name: "tool-b"}},
			errText: "multiple downloadable assets and none match linux/amd64",
		},
		{
			name:    "all noise",
			assets:  []github.Asset{{Name: "tool.sha256"}, {Name: "tool.deb"}},
			errText: "no downloadable assets after filtering",
		},
		{
			name:     "override exact match",
			assets:   []github.Asset{{Name: "tool-linux-amd64"}, {Name: "tool-darwin-arm64"}},
			want:     github.Asset{Name: "tool-darwin-arm64"},
			override: "tool-darwin-arm64",
		},
		{
			name:     "override substring match",
			assets:   []github.Asset{{Name: "tool-linux-amd64"}, {Name: "tool-darwin-arm64"}},
			want:     github.Asset{Name: "tool-darwin-arm64"},
			override: "darwin",
		},
		{
			name:     "override no match",
			assets:   []github.Asset{{Name: "tool-linux-amd64"}, {Name: "tool-darwin-arm64"}},
			errText:  "no asset matching",
			override: "windows",
		},
		{
			name:     "override ambiguous substring",
			assets:   []github.Asset{{Name: "tool-linux-amd64"}, {Name: "tool-linux-x86_64"}},
			errText:  "ambiguous assets matching",
			override: "linux",
		},
		{
			name:   "tar.gz preferred over zip",
			assets: []github.Asset{{Name: "tool-linux-amd64.zip"}, {Name: "tool-linux-amd64.tar.gz"}},
			want:   github.Asset{Name: "tool-linux-amd64.tar.gz"},
		},
		{
			name:   "zip preferred over raw",
			assets: []github.Asset{{Name: "tool-linux-amd64"}, {Name: "tool-linux-amd64.zip"}},
			want:   github.Asset{Name: "tool-linux-amd64.zip"},
		},
		{
			name:   "tar.xz preferred over zip",
			assets: []github.Asset{{Name: "tool-linux-amd64.zip"}, {Name: "tool-linux-amd64.tar.xz"}},
			want:   github.Asset{Name: "tool-linux-amd64.tar.xz"},
		},
		{
			name:   "tar.bz2 preferred over zip",
			assets: []github.Asset{{Name: "tool-linux-amd64.zip"}, {Name: "tool-linux-amd64.tar.bz2"}},
			want:   github.Asset{Name: "tool-linux-amd64.tar.bz2"},
		},
		{
			name:   "zip preferred over tar",
			assets: []github.Asset{{Name: "tool-linux-amd64.tar"}, {Name: "tool-linux-amd64.zip"}},
			want:   github.Asset{Name: "tool-linux-amd64.zip"},
		},
		{
			name:   "compressed raw preferred over raw",
			assets: []github.Asset{{Name: "tool-linux-amd64"}, {Name: "tool-linux-amd64.gz"}},
			want:   github.Asset{Name: "tool-linux-amd64.gz"},
		},
		{
			name:   "ties broken by name",
			goos:   "darwin",
			goarch: "amd64",
			assets: []github.Asset{{Name: "tool-macos-amd64.tar.gz"}, {Name: "tool-darwin-amd64.tar.gz"}},
			want:   github.Asset{Name: "tool-darwin-amd64.tar.gz"},
		},
		{
			name:   "x86 does not match x86_64",
			goarch: "386",
			assets: []github.Asset{
				{Name: "tool-linux-x86_64.tar.gz"},
				{Name: "tool-linux-x86.tar.gz"},
			},
			want: github.Asset{Name: "tool-linux-x86.tar.gz"},
		},
		{
			name:   "x86_64 matches amd64",
			assets: []github.Asset{{Name: "tool-linux-x86_64"}},
			want:   github.Asset{Name: "tool-linux-x86_64"},
		},
		{
			name:   "aarch64 matches arm64",
			goarch: "arm64",
			assets: []github.Asset{{Name: "tool-linux-aarch64"}},
			want:   github.Asset{Name: "tool-linux-aarch64"},
		},
		{
			name:   "armv7 matches arm",
			goos:   "linux",
			goarch: "arm",
			assets: []github.Asset{{Name: "tool-linux-armv7"}},
			want:   github.Asset{Name: "tool-linux-armv7"},
		},
		{
			name:   "windows win alias",
			goos:   "windows",
			goarch: "amd64",
			assets: []github.Asset{{Name: "tool-win-x64"}},
			want:   github.Asset{Name: "tool-win-x64"},
		},
		{
			name:   "macos alias",
			goos:   "darwin",
			goarch: "arm64",
			assets: []github.Asset{{Name: "tool-macos-arm64"}},
			want:   github.Asset{Name: "tool-macos-arm64"},
		},
		{
			name:   "osx alias",
			goos:   "darwin",
			goarch: "amd64",
			assets: []github.Asset{{Name: "tool-osx-x86_64"}},
			want:   github.Asset{Name: "tool-osx-x86_64"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			goos, goarch := test.goos, test.goarch
			if goos == "" {
				goos = "linux"
			}
			if goarch == "" {
				goarch = "amd64"
			}

			got, err := SelectAsset(&github.Release{TagName: "v1.2.3", Assets: test.assets}, goos, goarch, test.override)
			if test.errText != "" {
				if err == nil {
					t.Fatal("SelectAsset() error = nil, want error")
				}
				if !strings.Contains(err.Error(), test.errText) {
					t.Errorf("SelectAsset() error = %q, want text %q", err, test.errText)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectAsset() error = %v", err)
			}
			if got != test.want {
				t.Errorf("SelectAsset() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestSelectAssetDarwinUniversalAndBitness(t *testing.T) {
	tests := []struct {
		name   string
		goarch string
		assets []github.Asset
		want   string
	}{
		{
			name:   "universal amd64",
			goarch: "amd64",
			assets: []github.Asset{{Name: "tool-darwin-universal.tar.gz"}},
			want:   "tool-darwin-universal.tar.gz",
		},
		{
			name:   "all arm64",
			goarch: "arm64",
			assets: []github.Asset{{Name: "tool-darwin-all.zip"}},
			want:   "tool-darwin-all.zip",
		},
		{
			name:   "specific amd64 beats universal",
			goarch: "amd64",
			assets: []github.Asset{
				{Name: "tool-darwin-universal.tar.gz"},
				{Name: "tool-darwin-amd64.zip"},
			},
			want: "tool-darwin-amd64.zip",
		},
		{
			name:   "specific arm64 beats universal",
			goarch: "arm64",
			assets: []github.Asset{
				{Name: "tool-darwin-all.tar.gz"},
				{Name: "tool-darwin-arm64.zip"},
			},
			want: "tool-darwin-arm64.zip",
		},
		{
			name:   "x86_64 is not narrow x86",
			goarch: "amd64",
			assets: []github.Asset{
				{Name: "tool-linux-x86"},
				{Name: "tool-linux-x86_64"},
			},
			want: "tool-linux-x86_64",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SelectAsset(&github.Release{TagName: "v1.0.0", Assets: test.assets}, "darwin", test.goarch, "")
			if strings.Contains(test.name, "x86_64") {
				got, err = SelectAsset(&github.Release{TagName: "v1.0.0", Assets: test.assets}, "linux", test.goarch, "")
			}
			if err != nil {
				t.Fatalf("SelectAsset() error = %v", err)
			}
			if got.Name != test.want {
				t.Fatalf("SelectAsset() = %q, want %q", got.Name, test.want)
			}
		})
	}
}

func TestSelectAssetRejectsOnlyUnsupported32BitCandidates(t *testing.T) {
	release := &github.Release{
		TagName: "v1.0.0",
		Assets: []github.Asset{
			{Name: "tool-linux-x86"},
			{Name: "tool-linux-i686"},
		},
	}
	if _, err := SelectAsset(release, "linux", "amd64", ""); err == nil || !strings.Contains(err.Error(), "none match linux/amd64") {
		t.Fatalf("SelectAsset() error = %v, want no-match error", err)
	}
}

func TestSelectAssetDoesNotTreatLinuxUniversalAsPlatformMatch(t *testing.T) {
	release := &github.Release{
		TagName: "v1.0.0",
		Assets:  []github.Asset{{Name: "tool-linux-all"}, {Name: "tool-darwin-universal"}},
	}
	if _, err := SelectAsset(release, "linux", "amd64", ""); err == nil || !strings.Contains(err.Error(), "none match linux/amd64") {
		t.Fatalf("SelectAsset() error = %v, want no-match error", err)
	}
}

func TestFilterLibc(t *testing.T) {
	assets := []github.Asset{
		{Name: "tool-linux-amd64-musl.tar.xz"},
		{Name: "tool-linux-amd64-gnu.tar.gz"},
		{Name: "tool-linux-amd64.tar.gz"},
	}

	tests := []struct {
		name   string
		host   libcKind
		assets []github.Asset
		want   []github.Asset
	}{
		{
			name:   "musl prefers musl over neutral and gnu",
			host:   libcMusl,
			assets: assets,
			want:   []github.Asset{{Name: "tool-linux-amd64-musl.tar.xz"}},
		},
		{
			name: "musl prefers neutral over gnu",
			host: libcMusl,
			assets: []github.Asset{
				{Name: "tool-linux-amd64-gnu.tar.gz"},
				{Name: "tool-linux-amd64.tar.gz"},
			},
			want: []github.Asset{{Name: "tool-linux-amd64.tar.gz"}},
		},
		{
			name: "musl has no compatible candidates",
			host: libcMusl,
			assets: []github.Asset{
				{Name: "tool-linux-amd64-gnu.tar.gz"},
				{Name: "tool-linux-amd64-glibc.tar.gz"},
			},
			want: nil,
		},
		{
			name:   "glibc retains all candidates",
			host:   libcGlibc,
			assets: assets,
			want:   assets,
		},
		{
			name:   "unknown retains all candidates",
			host:   libcUnknown,
			assets: assets,
			want:   assets,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := filterLibc(test.assets, test.host); !reflect.DeepEqual(got, test.want) {
				t.Errorf("filterLibc() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestSelectAssetWithLibc(t *testing.T) {
	assets := []github.Asset{
		{Name: "tool-linux-amd64-musl.tar.xz"},
		{Name: "tool-linux-amd64-gnu.tar.gz"},
		{Name: "tool-linux-amd64.tar.gz"},
	}

	tests := []struct {
		name    string
		host    libcKind
		goos    string
		assets  []github.Asset
		want    github.Asset
		errText string
	}{
		{
			name:   "musl asset wins before format preference",
			host:   libcMusl,
			assets: assets,
			want:   github.Asset{Name: "tool-linux-amd64-musl.tar.xz"},
		},
		{
			name: "neutral asset wins over gnu",
			host: libcMusl,
			assets: []github.Asset{
				{Name: "tool-linux-amd64-gnu.tar.gz"},
				{Name: "tool-linux-amd64.tar.gz"},
			},
			want: github.Asset{Name: "tool-linux-amd64.tar.gz"},
		},
		{
			name:   "glibc keeps previous format choice",
			host:   libcGlibc,
			assets: assets,
			want:   github.Asset{Name: "tool-linux-amd64-gnu.tar.gz"},
		},
		{
			name:   "unknown keeps previous format choice",
			host:   libcUnknown,
			assets: assets,
			want:   github.Asset{Name: "tool-linux-amd64-gnu.tar.gz"},
		},
		{
			name: "non-linux ignores musl host parameter",
			host: libcMusl,
			goos: "darwin",
			assets: []github.Asset{
				{Name: "tool-darwin-amd64-musl.tar.xz"},
				{Name: "tool-darwin-amd64-gnu.tar.gz"},
				{Name: "tool-darwin-amd64.tar.gz"},
			},
			want: github.Asset{Name: "tool-darwin-amd64-gnu.tar.gz"},
		},
		{
			name: "gnu-only fails with candidates",
			host: libcMusl,
			assets: []github.Asset{
				{Name: "tool-linux-amd64-gnu.tar.gz"},
				{Name: "tool-linux-amd64-glibc.tar.gz"},
			},
			errText: "tool-linux-amd64-gnu.tar.gz, tool-linux-amd64-glibc.tar.gz",
		},
		{
			name: "single gnu-only asset fails with candidate",
			host: libcMusl,
			assets: []github.Asset{
				{Name: "tool-linux-amd64-gnu.tar.gz"},
			},
			errText: "tool-linux-amd64-gnu.tar.gz",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			goos := test.goos
			if goos == "" {
				goos = "linux"
			}
			got, err := selectAssetWithLibc(
				&github.Release{TagName: "v1.2.3", Assets: test.assets},
				goos,
				"amd64",
				"",
				test.host,
			)
			if test.errText != "" {
				if err == nil {
					t.Fatal("selectAssetWithLibc() error = nil, want error")
				}
				if !strings.Contains(err.Error(), test.errText) {
					t.Errorf("selectAssetWithLibc() error = %q, want text %q", err, test.errText)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectAssetWithLibc() error = %v", err)
			}
			if got != test.want {
				t.Errorf("selectAssetWithLibc() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestSelectAssetNilRelease(t *testing.T) {
	_, err := SelectAsset(nil, "linux", "amd64", "")
	if err == nil {
		t.Fatal("SelectAsset() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "nil release") {
		t.Errorf("SelectAsset() error = %q, want text %q", err, "nil release")
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name   string
		format Format
	}{
		{name: "tool", format: FormatRaw},
		{name: "tool.TAR.GZ", format: FormatTarGz},
		{name: "tool.tgz", format: FormatTarGz},
		{name: "tool.Zip", format: FormatZip},
		{name: "tool.tar.xz", format: FormatTarXz},
		{name: "tool.tar.bz2", format: FormatTarBz2},
		{name: "tool.tar.zst", format: FormatTarZst},
		{name: "tool.tzst", format: FormatTarZst},
		{name: "tool.tar", format: FormatTar},
		{name: "tool.gz", format: FormatGz},
		{name: "tool.bz2", format: FormatBz2},
		{name: "tool.xz", format: FormatXz},
		{name: "tool.zst", format: FormatZst},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DetectFormat(test.name); got != test.format {
				t.Errorf("DetectFormat(%q) = %d, want %d", test.name, got, test.format)
			}
		})
	}
}

func TestFormatRank(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"tar.gz over tar.xz", "tool.tar.gz", "tool.tar.xz", true},
		{"tar.xz over tar.zst", "tool.tar.xz", "tool.tar.zst", true},
		{"tar.zst over tar.bz2", "tool.tar.zst", "tool.tar.bz2", true},
		{"tar.bz2 over zip", "tool.tar.bz2", "tool.zip", true},
		{"zip over tar", "tool.zip", "tool.tar", true},
		{"tar over compressed raw", "tool.tar", "tool.gz", true},
		{"compressed raw over raw", "tool.gz", "tool", true},
		{"zst compressed raw over raw", "tool.zst", "tool", true},
		{"raw last", "tool", "tool.bz2", false},
		{"tar.gz precedence vs gz", "tool.tar.gz", "tool.gz", true},
		{"tar.zst precedence vs zst", "tool.tar.zst", "tool.zst", true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := formatRank(test.a) < formatRank(test.b)
			if got != test.want {
				t.Errorf("formatRank(%q) < formatRank(%q) = %v, want %v", test.a, test.b, got, test.want)
			}
		})
	}
}

func TestExtractBinaryRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	want := []byte("raw binary")
	if err := os.WriteFile(path, want, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := ExtractBinary(path, FormatRaw, "tool")
	if err != nil {
		t.Fatalf("ExtractBinary() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ExtractBinary() = %q, want %q", got, want)
	}
}

func TestExtractBinaryRejectsRawAndCompressedCandidatesAboveLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	resourceLimits := limits.Defaults()
	resourceLimits.BinaryBytes = 4

	rawPath := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(rawPath, []byte("12345"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinary(rawPath, FormatRaw, "tool", resourceLimits); err == nil || !strings.Contains(err.Error(), "binary candidate bytes") {
		t.Fatalf("raw extraction error = %v, want binary limit", err)
	}

	compressedPath := filepath.Join(t.TempDir(), "tool.gz")
	writeGzipped(t, compressedPath, []byte("12345"))
	if _, err := extractBinary(compressedPath, FormatGz, "tool", resourceLimits); err == nil || !strings.Contains(err.Error(), "binary candidate bytes") {
		t.Fatalf("compressed extraction error = %v, want binary limit", err)
	}
}

func TestExtractTarEnforcesEntryAndCumulativeLimits(t *testing.T) {
	assetPath := filepath.Join(t.TempDir(), "asset.tar")
	writeTar(t, assetPath, []archiveFile{
		{name: "readme", mode: 0644, contents: []byte("metadata")},
		{name: "tool", mode: 0755, contents: []byte("binary")},
	})
	resourceLimits := limits.Defaults()
	resourceLimits.ArchiveEntries = 1
	if _, err := extractBinary(assetPath, FormatTar, "tool", resourceLimits); err == nil || !strings.Contains(err.Error(), "archive entry count") {
		t.Fatalf("entry-limited extraction error = %v, want entry limit", err)
	}

	tarData, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatal(err)
	}
	resourceLimits = limits.Defaults()
	resourceLimits.ArchiveBytes = int64(len(tarData) - 1)
	if _, err := extractBinary(assetPath, FormatTar, "tool", resourceLimits); err == nil || !strings.Contains(err.Error(), "archive decompressed bytes") {
		t.Fatalf("byte-limited extraction error = %v, want cumulative archive limit", err)
	}
}

func TestExtractZipEnforcesEntryAndDeclaredCandidateLimits(t *testing.T) {
	assetPath := filepath.Join(t.TempDir(), "asset.zip")
	writeZip(t, assetPath, []archiveFile{
		{name: "readme", mode: 0644, contents: []byte("metadata")},
		{name: "tool", mode: 0755, contents: []byte("binary")},
	})
	resourceLimits := limits.Defaults()
	resourceLimits.ArchiveEntries = 1
	if _, err := extractBinary(assetPath, FormatZip, "tool", resourceLimits); err == nil || !strings.Contains(err.Error(), "archive entry count") {
		t.Fatalf("entry-limited ZIP extraction error = %v, want entry limit", err)
	}

	resourceLimits = limits.Defaults()
	resourceLimits.BinaryBytes = 4
	if _, err := extractBinary(assetPath, FormatZip, "tool", resourceLimits); err == nil || !strings.Contains(err.Error(), "binary candidate bytes") {
		t.Fatalf("candidate-limited ZIP extraction error = %v, want binary limit", err)
	}
}

func TestExtractBinaryTarGz(t *testing.T) {
	tests := []struct {
		name    string
		files   []archiveFile
		exeName string
		want    []byte
		errText string
	}{
		{
			name: "exact match wins among multiple executables",
			files: []archiveFile{
				{name: "nested/tool", mode: 0755, contents: []byte("tar exact binary")},
				{name: "nested/tool-linux-amd64", mode: 0755, contents: []byte("tar prefix binary")},
				{name: "nested/completion", mode: 0755, contents: []byte("tar completion")},
			},
			exeName: "tool",
			want:    []byte("tar exact binary"),
		},
		{
			name: "unique prefix match wins among multiple executables",
			files: []archiveFile{
				{name: "nested/tool-linux-amd64", mode: 0755, contents: []byte("tar prefix binary")},
				{name: "nested/completion", mode: 0755, contents: []byte("tar completion")},
			},
			exeName: "tool",
			want:    []byte("tar prefix binary"),
		},
		{
			name: "single unmatched executable uses fallback",
			files: []archiveFile{
				{name: "nested/release", mode: 0755, contents: []byte("tar fallback binary")},
			},
			exeName: "tool",
			want:    []byte("tar fallback binary"),
		},
		{
			name: "no executables",
			files: []archiveFile{
				{name: "README", mode: 0644, contents: []byte("readme")},
			},
			exeName: "tool",
			errText: "archive contains 0 executable files",
		},
		{
			name: "ambiguous prefix matches list candidates",
			files: []archiveFile{
				{name: "tool-linux-amd64", mode: 0755, contents: []byte("a")},
				{name: "tool-debug", mode: 0755, contents: []byte("b")},
			},
			exeName: "tool",
			errText: "candidates: tool-linux-amd64, tool-debug",
		},
		{
			name: "multiple nonmatching executables list candidates",
			files: []archiveFile{
				{name: "helper-a", mode: 0755, contents: []byte("a")},
				{name: "helper-b", mode: 0755, contents: []byte("b")},
			},
			exeName: "tool",
			errText: "candidates: helper-a, helper-b",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assetPath := filepath.Join(t.TempDir(), "asset.tar.gz")
			writeTarGz(t, assetPath, test.files)
			got, err := ExtractBinary(assetPath, FormatTarGz, test.exeName)
			assertExtractionResult(t, got, err, test.want, test.errText)
		})
	}
}

func TestExtractBinaryZip(t *testing.T) {
	tests := []struct {
		name    string
		files   []archiveFile
		exeName string
		want    []byte
		errText string
	}{
		{
			name: "exact match wins among multiple executables",
			files: []archiveFile{
				{name: "nested/tool", mode: 0755, contents: []byte("zip exact binary")},
				{name: "nested/tool-linux-amd64", mode: 0755, contents: []byte("zip prefix binary")},
				{name: "nested/completion", mode: 0755, contents: []byte("zip completion")},
			},
			exeName: "tool",
			want:    []byte("zip exact binary"),
		},
		{
			name: "unique prefix match wins among multiple executables",
			files: []archiveFile{
				{name: "nested/tool-linux-amd64", mode: 0755, contents: []byte("zip prefix binary")},
				{name: "nested/completion", mode: 0755, contents: []byte("zip completion")},
			},
			exeName: "tool",
			want:    []byte("zip prefix binary"),
		},
		{
			name: "single unmatched executable uses fallback",
			files: []archiveFile{
				{name: "nested/release", mode: 0755, contents: []byte("zip fallback binary")},
			},
			exeName: "tool",
			want:    []byte("zip fallback binary"),
		},
		{
			name: "no executables",
			files: []archiveFile{
				{name: "README", mode: 0644, contents: []byte("readme")},
			},
			exeName: "tool",
			errText: "archive contains 0 executable files",
		},
		{
			name: "ambiguous prefix matches list candidates",
			files: []archiveFile{
				{name: "tool-linux-amd64", mode: 0755, contents: []byte("a")},
				{name: "tool-debug", mode: 0755, contents: []byte("b")},
			},
			exeName: "tool",
			errText: "candidates: tool-linux-amd64, tool-debug",
		},
		{
			name: "multiple nonmatching executables list candidates",
			files: []archiveFile{
				{name: "helper-a", mode: 0755, contents: []byte("a")},
				{name: "helper-b", mode: 0755, contents: []byte("b")},
			},
			exeName: "tool",
			errText: "candidates: helper-a, helper-b",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assetPath := filepath.Join(t.TempDir(), "asset.zip")
			writeZip(t, assetPath, test.files)
			got, err := ExtractBinary(assetPath, FormatZip, test.exeName)
			assertExtractionResult(t, got, err, test.want, test.errText)
		})
	}
}

func TestExtractBinaryTarVariants(t *testing.T) {
	tests := []struct {
		name    string
		format  Format
		writer  func(t *testing.T, path string, files []archiveFile)
		ext     string
		files   []archiveFile
		want    []byte
		errText string
	}{
		{
			name:   "tar.xz one executable",
			format: FormatTarXz,
			writer: writeTarXz,
			ext:    ".tar.xz",
			files: []archiveFile{
				{name: "nested/tool", mode: 0755, contents: []byte("tar.xz binary")},
			},
			want: []byte("tar.xz binary"),
		},
		{
			name:   "tar.bz2 one executable",
			format: FormatTarBz2,
			writer: writeTarBz2,
			ext:    ".tar.bz2",
			files: []archiveFile{
				{name: "nested/tool", mode: 0755, contents: []byte("tar.bz2 binary")},
			},
			want: []byte("tar.bz2 binary"),
		},
		{
			name:   "tar.zst one executable",
			format: FormatTarZst,
			writer: writeTarZst,
			ext:    ".tar.zst",
			files: []archiveFile{
				{name: "nested/tool", mode: 0755, contents: []byte("tar.zst binary")},
			},
			want: []byte("tar.zst binary"),
		},
		{
			name:   "tzst one executable",
			format: FormatTarZst,
			writer: writeTarZst,
			ext:    ".tzst",
			files: []archiveFile{
				{name: "nested/tool", mode: 0755, contents: []byte("tzst binary")},
			},
			want: []byte("tzst binary"),
		},
		{
			name:   "tar one executable",
			format: FormatTar,
			writer: writeTar,
			ext:    ".tar",
			files: []archiveFile{
				{name: "nested/tool", mode: 0755, contents: []byte("tar binary")},
			},
			want: []byte("tar binary"),
		},
		{
			name:   "tar.xz no executables",
			format: FormatTarXz,
			writer: writeTarXz,
			ext:    ".tar.xz",
			files: []archiveFile{
				{name: "README", mode: 0644, contents: []byte("readme")},
			},
			errText: "archive contains 0 executable files",
		},
		{
			name:   "tar.bz2 two executables",
			format: FormatTarBz2,
			writer: writeTarBz2,
			ext:    ".tar.bz2",
			files: []archiveFile{
				{name: "tool-a", mode: 0755, contents: []byte("a")},
				{name: "tool-b", mode: 0755, contents: []byte("b")},
			},
			errText: "archive contains 2 executable files",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assetPath := filepath.Join(t.TempDir(), "asset"+test.ext)
			test.writer(t, assetPath, test.files)
			got, err := ExtractBinary(assetPath, test.format, "tool")
			assertExtractionResult(t, got, err, test.want, test.errText)
		})
	}
}

func TestExtractBinaryCompressedRaw(t *testing.T) {
	tests := []struct {
		name    string
		format  Format
		writer  func(t *testing.T, path string, contents []byte)
		ext     string
		want    []byte
		errText string
	}{
		{
			name:   "gz raw binary",
			format: FormatGz,
			writer: writeGzipped,
			ext:    ".gz",
			want:   []byte("gz binary"),
		},
		{
			name:   "bz2 raw binary",
			format: FormatBz2,
			writer: writeBz2,
			ext:    ".bz2",
			want:   []byte("bz2 binary"),
		},
		{
			name:   "xz raw binary",
			format: FormatXz,
			writer: writeXz,
			ext:    ".xz",
			want:   []byte("xz binary"),
		},
		{
			name:   "zst raw binary",
			format: FormatZst,
			writer: writeZst,
			ext:    ".zst",
			want:   []byte("zst binary"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assetPath := filepath.Join(t.TempDir(), "asset"+test.ext)
			test.writer(t, assetPath, test.want)
			got, err := ExtractBinary(assetPath, test.format, "tool")
			assertExtractionResult(t, got, err, test.want, test.errText)
		})
	}
}

func TestExtractBinaryDetectsContentFormat(t *testing.T) {
	archiveFiles := []archiveFile{{
		name:     "nested/tool",
		mode:     0755,
		contents: []byte("archive binary"),
	}}
	rawContents := []byte("compressed raw binary")
	tests := []struct {
		name   string
		format Format
		write  func(*testing.T, string)
		want   []byte
	}{
		{
			name:   "extensionless tar.gz",
			format: FormatRaw,
			write: func(t *testing.T, path string) {
				writeTarGz(t, path, archiveFiles)
			},
			want: []byte("archive binary"),
		},
		{
			name:   "extensionless tar.xz",
			format: FormatRaw,
			write: func(t *testing.T, path string) {
				writeTarXz(t, path, archiveFiles)
			},
			want: []byte("archive binary"),
		},
		{
			name:   "extensionless tar.bz2",
			format: FormatRaw,
			write: func(t *testing.T, path string) {
				writeTarBz2(t, path, archiveFiles)
			},
			want: []byte("archive binary"),
		},
		{
			name:   "extensionless tar",
			format: FormatRaw,
			write: func(t *testing.T, path string) {
				writeTar(t, path, archiveFiles)
			},
			want: []byte("archive binary"),
		},
		{
			name:   "extensionless zip",
			format: FormatRaw,
			write: func(t *testing.T, path string) {
				writeZip(t, path, archiveFiles)
			},
			want: []byte("archive binary"),
		},
		{
			name:   "extensionless gzip raw binary",
			format: FormatRaw,
			write: func(t *testing.T, path string) {
				writeGzipped(t, path, rawContents)
			},
			want: rawContents,
		},
		{
			name:   "extensionless bzip2 raw binary",
			format: FormatRaw,
			write: func(t *testing.T, path string) {
				writeBz2(t, path, rawContents)
			},
			want: rawContents,
		},
		{
			name:   "extensionless xz raw binary",
			format: FormatRaw,
			write: func(t *testing.T, path string) {
				writeXz(t, path, rawContents)
			},
			want: rawContents,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assetPath := filepath.Join(t.TempDir(), "asset")
			test.write(t, assetPath)
			got, err := ExtractBinary(assetPath, test.format, "tool")
			assertExtractionResult(t, got, err, test.want, "")
		})
	}
}

func TestExtractBinaryContentWinsOverFilename(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		write  func(*testing.T, string)
		want   []byte
	}{
		{
			name:   "tar.gz content named gz",
			format: FormatGz,
			write: func(t *testing.T, path string) {
				writeTarGz(t, path, []archiveFile{{name: "tool", mode: 0755, contents: []byte("tar content")}})
			},
			want: []byte("tar content"),
		},
		{
			name:   "gzip raw content named tar.gz",
			format: FormatTarGz,
			write: func(t *testing.T, path string) {
				writeGzipped(t, path, []byte("gzip content"))
			},
			want: []byte("gzip content"),
		},
		{
			name:   "zip content named tar.gz",
			format: FormatTarGz,
			write: func(t *testing.T, path string) {
				writeZip(t, path, []archiveFile{{name: "tool", mode: 0755, contents: []byte("zip content")}})
			},
			want: []byte("zip content"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assetPath := filepath.Join(t.TempDir(), "asset.tar.gz")
			test.write(t, assetPath)
			got, err := ExtractBinary(assetPath, test.format, "tool")
			assertExtractionResult(t, got, err, test.want, "")
		})
	}
}

func TestExtractBinaryUnrecognizedContentFallsBackToFilename(t *testing.T) {
	assetPath := filepath.Join(t.TempDir(), "asset.tar.gz")
	writeFile(t, assetPath, []byte("not an archive"))

	_, err := ExtractBinary(assetPath, DetectFormat(filepath.Base(assetPath)), "tool")
	if err == nil {
		t.Fatal("ExtractBinary() error = nil, want tar.gz fallback error")
	}
	if !strings.Contains(err.Error(), "open gzip asset") {
		t.Errorf("ExtractBinary() error = %q, want gzip fallback error", err)
	}
}

func TestListExecutablesDetectsContentFormat(t *testing.T) {
	assetPath := filepath.Join(t.TempDir(), "asset")
	writeTarGz(t, assetPath, []archiveFile{{name: "bin/tool", mode: 0755, contents: []byte("tool")}})

	names, err := ListExecutables(assetPath, FormatRaw)
	if err != nil {
		t.Fatalf("ListExecutables() error = %v", err)
	}
	if !reflect.DeepEqual(names, []string{"bin/tool"}) {
		t.Fatalf("ListExecutables() = %#v, want [bin/tool]", names)
	}
}

func TestTargetPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := TargetPath("acme/tool")
	if err != nil {
		t.Fatalf("TargetPath() error = %v", err)
	}
	want := filepath.Join(home, ".local", "bin", "tool")
	if got != want {
		t.Errorf("TargetPath() = %q, want %q", got, want)
	}
}

func TestTargetPathForAndFilterAssets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got, err := TargetPathFor("acme/widget", "custom")
	if err != nil {
		t.Fatalf("TargetPathFor() error = %v", err)
	}
	if want := filepath.Join(home, ".local", "bin", "custom"); got != want {
		t.Errorf("TargetPathFor() = %q, want %q", got, want)
	}

	assets := []github.Asset{
		{Name: "widget-linux-amd64.tar.gz"},
		{Name: "widget-darwin-arm64.zip"},
		{Name: "widget.sha256"},
	}
	filtered := FilterAssets(assets, "linux", "amd64")
	if len(filtered) != 1 || filtered[0].Name != "widget-linux-amd64.tar.gz" {
		t.Fatalf("FilterAssets() = %#v, want linux asset only", filtered)
	}
}

func TestListExecutablesAndExtractBinaryByName(t *testing.T) {
	dir := t.TempDir()
	// Build a two-member archive to exercise deterministic listing and explicit
	// extraction without relying on the automatic repository-name heuristic.
	assetPath := filepath.Join(dir, "multi.tar.gz")
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range []struct {
		name string
		data string
	}{
		{"bin/zeta", "zeta"},
		{"bin/alpha", "alpha"},
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: member.name, Mode: 0755, Size: int64(len(member.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(member.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assetPath, buffer.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	names, err := ListExecutables(assetPath, FormatTarGz)
	if err != nil {
		t.Fatalf("ListExecutables() error = %v", err)
	}
	if !reflect.DeepEqual(names, []string{"bin/alpha", "bin/zeta"}) {
		t.Fatalf("ListExecutables() = %#v", names)
	}
	got, err := ExtractBinaryByName(assetPath, FormatTarGz, "zeta")
	if err != nil {
		t.Fatalf("ExtractBinaryByName() error = %v", err)
	}
	if string(got) != "zeta" {
		t.Fatalf("ExtractBinaryByName() = %q, want zeta", got)
	}
}

func TestExtractBinaryByNameUsesExactArchivePath(t *testing.T) {
	dir := t.TempDir()
	assetPath := filepath.Join(dir, "duplicate-basename.tar.gz")
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range []struct {
		name string
		data string
	}{
		{"one/widget", "one"},
		{"two/widget", "two"},
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: member.name, Mode: 0755, Size: int64(len(member.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(member.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assetPath, buffer.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := ExtractBinaryByName(assetPath, FormatTarGz, "two/widget")
	if err != nil {
		t.Fatalf("ExtractBinaryByName() error = %v", err)
	}
	if string(got) != "two" {
		t.Fatalf("ExtractBinaryByName() = %q, want two", got)
	}
}

func TestTargetPathWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")

	_, err := TargetPath("acme/tool")
	if err == nil {
		t.Fatal("TargetPath() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "user home directory") {
		t.Errorf("TargetPath() error = %q, want text %q", err, "user home directory")
	}
}

func TestTargetPathHonorsConfiguredInstallDir(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	cfg := &config.Config{InstallDir: "~/custom"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := TargetPath("acme/tool")
	if err != nil {
		t.Fatalf("TargetPath() error = %v", err)
	}
	want := filepath.Join(home, "custom", "tool")
	if got != want {
		t.Errorf("TargetPath() = %q, want %q", got, want)
	}
}

func TestExtractBinaryErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) (assetPath string, format Format)
		errText string
	}{
		{
			name: "raw missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing"), FormatRaw
			},
			errText: "no such file",
		},
		{
			name: "unsupported format",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "tool"), Format(99)
			},
			errText: "unsupported asset format 99",
		},
		{
			name: "tar.gz missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.tar.gz"), FormatTarGz
			},
			errText: "open tar.gz asset",
		},
		{
			name: "tar.gz not gzip",
			setup: func(t *testing.T) (string, Format) {
				assetPath := filepath.Join(t.TempDir(), "asset.tar.gz")
				writeFile(t, assetPath, []byte("not a gzip stream"))
				return assetPath, FormatTarGz
			},
			errText: "open gzip asset",
		},
		{
			name: "tar.gz corrupt tar",
			setup: func(t *testing.T) (string, Format) {
				assetPath := filepath.Join(t.TempDir(), "asset.tar.gz")
				corruptTar := make([]byte, 512)
				copy(corruptTar[tarMagicOffset:], tarMagic)
				writeGzipped(t, assetPath, corruptTar)
				return assetPath, FormatTarGz
			},
			errText: "read tar.gz asset",
		},
		{
			name: "zip corrupt entry",
			setup: func(t *testing.T) (string, Format) {
				assetPath := filepath.Join(t.TempDir(), "asset.zip")
				writeZip(t, assetPath, []archiveFile{
					{name: "tool", mode: 0755, contents: []byte("zip binary")},
				})
				raw, err := os.ReadFile(assetPath)
				if err != nil {
					t.Fatalf("ReadFile() error = %v", err)
				}
				offset := bytes.Index(raw, []byte("zip binary"))
				if offset < 0 {
					t.Fatal("zip entry contents not found in archive")
				}
				raw[offset] ^= 0xff
				writeFile(t, assetPath, raw)
				return assetPath, FormatZip
			},
			errText: "read executable",
		},
		{
			name: "zip missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.zip"), FormatZip
			},
			errText: "open zip asset",
		},
		{
			name: "zip not zip",
			setup: func(t *testing.T) (string, Format) {
				assetPath := filepath.Join(t.TempDir(), "asset.zip")
				writeFile(t, assetPath, []byte("not a zip archive"))
				return assetPath, FormatZip
			},
			errText: "open zip asset",
		},
		{
			name: "tar.xz missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.tar.xz"), FormatTarXz
			},
			errText: "open tar.xz asset",
		},
		{
			name: "tar.xz not xz",
			setup: func(t *testing.T) (string, Format) {
				assetPath := filepath.Join(t.TempDir(), "asset.tar.xz")
				writeFile(t, assetPath, []byte("not an xz stream"))
				return assetPath, FormatTarXz
			},
			errText: "open xz asset",
		},
		{
			name: "tar.bz2 missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.tar.bz2"), FormatTarBz2
			},
			errText: "open tar.bz2 asset",
		},
		{
			name: "tar missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.tar"), FormatTar
			},
			errText: "open tar asset",
		},
		{
			name: "gz missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.gz"), FormatGz
			},
			errText: "open gzip asset",
		},
		{
			name: "gz not gzip",
			setup: func(t *testing.T) (string, Format) {
				assetPath := filepath.Join(t.TempDir(), "asset.gz")
				writeFile(t, assetPath, []byte("not a gzip stream"))
				return assetPath, FormatGz
			},
			errText: "open gzip asset",
		},
		{
			name: "bz2 missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.bz2"), FormatBz2
			},
			errText: "open bzip2 asset",
		},
		{
			name: "xz missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.xz"), FormatXz
			},
			errText: "open xz asset",
		},
		{
			name: "tar.zst missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.tar.zst"), FormatTarZst
			},
			errText: "open tar.zst asset",
		},
		{
			name: "tar.zst not zstd",
			setup: func(t *testing.T) (string, Format) {
				assetPath := filepath.Join(t.TempDir(), "asset.tar.zst")
				writeFile(t, assetPath, []byte("not a zstd stream"))
				return assetPath, FormatTarZst
			},
			errText: "tar.zst asset",
		},
		{
			name: "zst missing file",
			setup: func(t *testing.T) (string, Format) {
				return filepath.Join(t.TempDir(), "missing.zst"), FormatZst
			},
			errText: "open zstd asset",
		},
		{
			name: "zst not zstd",
			setup: func(t *testing.T) (string, Format) {
				assetPath := filepath.Join(t.TempDir(), "asset.zst")
				writeFile(t, assetPath, []byte("not a zstd stream"))
				return assetPath, FormatZst
			},
			errText: "zstd asset",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assetPath, format := test.setup(t)
			got, err := ExtractBinary(assetPath, format, "tool")
			assertExtractionResult(t, got, err, nil, test.errText)
		})
	}
}

func TestInstallWritesWrapperAndRealBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	want := []byte("#!/bin/sh\necho hi\n")
	wrapperPath, realPath, err := Install(want, "acme/tool", false)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	if wrapperPath != filepath.Join(home, ".local", "bin", "tool") {
		t.Errorf("wrapperPath = %q, want %q", wrapperPath, filepath.Join(home, ".local", "bin", "tool"))
	}
	if realPath != filepath.Join(home, ".local", "share", "zenget", "bin", "tool") {
		t.Errorf("realPath = %q, want %q", realPath, filepath.Join(home, ".local", "share", "zenget", "bin", "tool"))
	}

	wrapper, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("ReadFile(wrapper) error = %v", err)
	}
	if !strings.Contains(string(wrapper), "zenget-managed wrapper") {
		t.Errorf("wrapper missing marker")
	}
	info, err := os.Stat(wrapperPath)
	if err != nil {
		t.Fatalf("Stat(wrapper) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0755 {
		t.Errorf("wrapper mode = %04o, want 0755", got)
	}

	got, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("ReadFile(real) error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("real binary = %q, want %q", got, want)
	}
	info, err = os.Stat(realPath)
	if err != nil {
		t.Fatalf("Stat(real) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0755 {
		t.Errorf("real binary mode = %04o, want 0755", got)
	}
}

func TestInstallVersionedWritesNamespacedArtifactAndStableWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	binary := []byte("version one")
	wrapperPath, realPath, err := InstallVersioned(binary, "github", "acme/tool", "v1.0.0", false, "tool")
	if err != nil {
		t.Fatalf("InstallVersioned() error = %v", err)
	}
	wantReal := filepath.Join(home, ".local", "share", "zenget", "bin", "github", "acme", "tool", "v1.0.0", "tool")
	if realPath != wantReal {
		t.Errorf("realPath = %q, want %q", realPath, wantReal)
	}
	if want := filepath.Join(home, ".local", "bin", "tool"); wrapperPath != want {
		t.Errorf("wrapperPath = %q, want %q", wrapperPath, want)
	}
	if got, err := os.ReadFile(realPath); err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("versioned binary = %q, err = %v", got, err)
	}
	wrapperContents, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("read stable wrapper: %v", err)
	}
	if !strings.Contains(string(wrapperContents), realPath) {
		t.Fatalf("stable wrapper does not point to versioned artifact: %q", wrapperContents)
	}
}

func TestInstallVersionedGeneratesManagedShimWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	previousArg0 := os.Args[0]
	os.Args[0] = "zenget"
	t.Cleanup(func() { os.Args[0] = previousArg0 })

	wrapperPath, realPath, err := InstallVersioned([]byte("version one"), "github", "acme/tool", "v1.0.0", false, "tool")
	if err != nil {
		t.Fatalf("InstallVersioned() error = %v", err)
	}
	wrapperContents, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("read managed wrapper: %v", err)
	}
	runtimePath, err := wrapper.ShimRuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	text := string(wrapperContents)
	if !strings.Contains(text, "zenget-managed wrapper") || !strings.Contains(text, runtimePath) || !strings.Contains(text, `shim_target="tool"`) {
		t.Fatalf("managed wrapper = %q, want runtime and target metadata", text)
	}
	if strings.Contains(text, realPath) {
		t.Fatalf("managed wrapper directly embeds artifact path: %q", text)
	}
	runtimeInfo, err := os.Lstat(runtimePath)
	if err != nil {
		t.Fatalf("stat managed runtime: %v", err)
	}
	if runtimeInfo.Mode()&os.ModeSymlink != 0 || runtimeInfo.Mode().Perm() != 0755 {
		t.Fatalf("managed runtime mode = %o, want regular 0755", runtimeInfo.Mode())
	}
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeData, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runtimeData, source) {
		t.Fatal("managed runtime does not match the current executable")
	}
}

func TestInstallUpgradeVersionedRetainsExactlyOnePreviousArtifact(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	if _, _, err := InstallVersioned([]byte("version one"), "github", "acme/tool", "v1.0.0", false, "tool"); err != nil {
		t.Fatalf("initial InstallVersioned() error = %v", err)
	}
	v1, err := VersionedRealPath("github", "acme/tool", "v1.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := VersionedRealPath("github", "acme/tool", "v2.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	v3, err := VersionedRealPath("github", "acme/tool", "v3.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}

	transaction, err := InstallUpgradeVersioned([]byte("version two"), "github", "acme/tool", "v2.0.0", "tool")
	if err != nil {
		t.Fatalf("v2 InstallUpgradeVersioned() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("v2 Commit() error = %v", err)
	}
	if _, err := os.Stat(v1); err != nil {
		t.Fatalf("v1 artifact after first upgrade: %v", err)
	}

	transaction, err = InstallUpgradeVersioned([]byte("version three"), "github", "acme/tool", "v3.0.0", "tool", v1)
	if err != nil {
		t.Fatalf("v3 InstallUpgradeVersioned() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("v3 Commit() error = %v", err)
	}
	if _, err := os.Stat(v1); !os.IsNotExist(err) {
		t.Fatalf("oldest artifact still exists, stat err = %v", err)
	}
	if got, err := os.ReadFile(v2); err != nil || string(got) != "version two" {
		t.Fatalf("previous artifact = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(v3); err != nil || string(got) != "version three" {
		t.Fatalf("active artifact = %q, err = %v", got, err)
	}
}

func TestInstallUpgradeVersionedRollsBackAfterWrapperActivationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	oldWrapper, oldReal, err := InstallVersioned([]byte("version one"), "github", "acme/tool", "v1.0.0", false, "tool")
	if err != nil {
		t.Fatalf("initial InstallVersioned() error = %v", err)
	}
	oldWrapperContents, err := os.ReadFile(oldWrapper)
	if err != nil {
		t.Fatalf("read old wrapper: %v", err)
	}
	v2, err := VersionedRealPath("github", "acme/tool", "v2.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}

	originalRename := installRename
	t.Cleanup(func() { installRename = originalRename })
	var renameCalls int
	installRename = func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 3 {
			return errors.New("injected wrapper activation failure")
		}
		return os.Rename(oldPath, newPath)
	}

	transaction, err := InstallUpgradeVersioned([]byte("version two"), "github", "acme/tool", "v2.0.0", "tool")
	if transaction != nil {
		t.Fatal("InstallUpgradeVersioned() returned a transaction after failed activation")
	}
	if err == nil || !strings.Contains(err.Error(), "activate wrapper") {
		t.Fatalf("InstallUpgradeVersioned() error = %v, want wrapper activation failure", err)
	}
	if got, err := os.ReadFile(oldReal); err != nil || string(got) != "version one" {
		t.Fatalf("old artifact after rollback = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(oldWrapper); err != nil || !bytes.Equal(got, oldWrapperContents) {
		t.Fatalf("old wrapper after rollback = %q, err = %v", got, err)
	}
	if _, err := os.Stat(v2); !os.IsNotExist(err) {
		t.Fatalf("new artifact after rollback exists or could not be checked: %v", err)
	}
}

func TestPrepareWrapperUpdateRollsBackAfterActivationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	oldWrapper, _, err := InstallVersioned([]byte("version one"), "github", "acme/tool", "v1.0.0", false, "tool")
	if err != nil {
		t.Fatalf("initial InstallVersioned() error = %v", err)
	}
	oldContents, err := os.ReadFile(oldWrapper)
	if err != nil {
		t.Fatal(err)
	}
	usageLog, err := wrapper.UsageLogPath("acme/tool")
	if err != nil {
		t.Fatal(err)
	}

	originalRename := installRename
	t.Cleanup(func() { installRename = originalRename })
	var renameCalls int
	installRename = func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 2 {
			return errors.New("injected wrapper activation failure")
		}
		return os.Rename(oldPath, newPath)
	}

	transaction, err := PrepareWrapperUpdate(oldWrapper, "acme/tool", "/new/real/path", usageLog)
	if transaction != nil {
		t.Fatal("PrepareWrapperUpdate() returned a transaction after failed activation")
	}
	if err == nil || !strings.Contains(err.Error(), "activate wrapper") {
		t.Fatalf("PrepareWrapperUpdate() error = %v, want activation failure", err)
	}
	if got, err := os.ReadFile(oldWrapper); err != nil || !bytes.Equal(got, oldContents) {
		t.Fatalf("wrapper after failed update = %q, err = %v", got, err)
	}
	if _, err := os.Stat(oldWrapper + ".old"); !os.IsNotExist(err) {
		t.Fatalf("wrapper backup after failed update exists or could not be checked: %v", err)
	}
}

func TestPrepareWrapperUpdateCommitReportsCleanupWarning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	oldWrapper, _, err := InstallVersioned([]byte("version one"), "github", "acme/tool", "v1.0.0", false, "tool")
	if err != nil {
		t.Fatalf("initial InstallVersioned() error = %v", err)
	}
	oldContents, err := os.ReadFile(oldWrapper)
	if err != nil {
		t.Fatal(err)
	}
	usageLog, err := wrapper.UsageLogPath("acme/tool")
	if err != nil {
		t.Fatal(err)
	}

	transaction, err := PrepareWrapperUpdate(oldWrapper, "acme/tool", "/new/real/path", usageLog)
	if err != nil {
		t.Fatalf("PrepareWrapperUpdate() error = %v", err)
	}
	if transaction.WrapperPath() != oldWrapper {
		t.Fatalf("WrapperPath() = %q, want %q", transaction.WrapperPath(), oldWrapper)
	}
	updatedContents, err := os.ReadFile(oldWrapper)
	if err != nil {
		t.Fatal(err)
	}
	if string(updatedContents) == string(oldContents) || !strings.Contains(string(updatedContents), "/new/real/path") {
		t.Fatalf("updated wrapper = %q, want new real path", updatedContents)
	}

	originalRemove := installRemove
	t.Cleanup(func() { installRemove = originalRemove })
	installRemove = func(string) error {
		return errors.New("injected wrapper cleanup failure")
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("transaction.Commit() error = %v", err)
	}
	if got := transaction.CleanupWarnings(); len(got) != 1 {
		t.Fatalf("cleanup warnings = %v, want one warning", got)
	}
	if _, err := os.Stat(oldWrapper + ".old"); err != nil {
		t.Fatalf("wrapper backup after cleanup warning: %v", err)
	}
	if got, err := os.ReadFile(oldWrapper); err != nil || !strings.Contains(string(got), "/new/real/path") {
		t.Fatalf("committed wrapper = %q, err = %v", got, err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() after Commit() error = %v", err)
	}
}

func TestPrepareWrapperUpdateRollbackRestoresWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	oldWrapper, _, err := InstallVersioned([]byte("version one"), "github", "acme/tool", "v1.0.0", false, "tool")
	if err != nil {
		t.Fatalf("initial InstallVersioned() error = %v", err)
	}
	oldContents, err := os.ReadFile(oldWrapper)
	if err != nil {
		t.Fatal(err)
	}
	usageLog, err := wrapper.UsageLogPath("acme/tool")
	if err != nil {
		t.Fatal(err)
	}

	transaction, err := PrepareWrapperUpdate(oldWrapper, "acme/tool", "/new/real/path", usageLog)
	if err != nil {
		t.Fatalf("PrepareWrapperUpdate() error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("transaction.Rollback() error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("second transaction.Rollback() error = %v", err)
	}
	if got, err := os.ReadFile(oldWrapper); err != nil || !bytes.Equal(got, oldContents) {
		t.Fatalf("restored wrapper = %q, err = %v", got, err)
	}
	if _, err := os.Stat(oldWrapper + ".old"); !os.IsNotExist(err) {
		t.Fatalf("wrapper backup after rollback exists or could not be checked: %v", err)
	}
	if got := transaction.CleanupWarnings(); got != nil {
		t.Fatalf("cleanup warnings after rollback = %v, want nil", got)
	}
}

func TestInstallProtectsForeignWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	wrapperPath := filepath.Join(home, ".local", "bin", "tool")
	if err := os.MkdirAll(filepath.Dir(wrapperPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapperPath, []byte("foreign"), 0755); err != nil {
		t.Fatal(err)
	}

	_, _, err := Install([]byte("binary"), "acme/tool", true)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite foreign file") {
		t.Fatalf("Install() error = %v, want foreign-file protection error", err)
	}

	got, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "foreign" {
		t.Errorf("foreign wrapper = %q, want original %q", got, "foreign")
	}
}

func TestInstallOverwritesExistingWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	wrapperPath := filepath.Join(home, ".local", "bin", "tool")
	if err := os.MkdirAll(filepath.Dir(wrapperPath), 0755); err != nil {
		t.Fatal(err)
	}
	oldWrapper := []byte("# zenget-managed wrapper\nold")
	if err := os.WriteFile(wrapperPath, oldWrapper, 0755); err != nil {
		t.Fatal(err)
	}

	_, _, err := Install([]byte("binary"), "acme/tool", false)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	wrapper, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("ReadFile(wrapper) error = %v", err)
	}
	if string(wrapper) == string(oldWrapper) {
		t.Error("existing wrapper was not replaced")
	}
}

func TestInstallErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		errText string
	}{
		{
			name: "directory creation fails",
			setup: func(t *testing.T) string {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "file"))
				// A file where the data directory should be makes MkdirAll fail.
				if err := os.WriteFile(os.Getenv("XDG_DATA_HOME"), []byte("in the way"), 0644); err != nil {
					t.Fatal(err)
				}
				return "acme/tool"
			},
			errText: "create install directory",
		},
		{
			name: "temporary file creation fails",
			setup: func(t *testing.T) string {
				home := t.TempDir()
				t.Setenv("HOME", home)
				dataHome := filepath.Join(home, "data")
				t.Setenv("XDG_DATA_HOME", dataHome)
				// Create the parent tree writable and only the final bin
				// directory read-only, so writing the temporary file fails.
				if err := os.MkdirAll(filepath.Join(dataHome, "zenget"), 0755); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				directory := filepath.Join(dataHome, "zenget", "bin")
				if err := os.Mkdir(directory, 0500); err != nil {
					t.Fatalf("Mkdir() error = %v", err)
				}
				return "acme/tool"
			},
			errText: "create temporary install file",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := test.setup(t)

			_, _, err := Install([]byte("binary"), repo, false)
			if err == nil {
				t.Fatal("Install() error = nil, want error")
			}
			if !strings.Contains(err.Error(), test.errText) {
				t.Errorf("Install() error = %q, want text %q", err, test.errText)
			}
		})
	}
}

type archiveFile struct {
	name     string
	mode     int64
	contents []byte
}

func writeFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func writeGzipped(t *testing.T, assetPath string, contents []byte) {
	t.Helper()
	file, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	gzipWriter := gzip.NewWriter(file)
	if _, err := gzipWriter.Write(contents); err != nil {
		t.Fatalf("gzip Write() error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("file Close() error = %v", err)
	}
}

func writeBz2(t *testing.T, assetPath string, contents []byte) {
	t.Helper()
	out, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer out.Close()

	cmd := bzip2Compress(t, contents)
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		t.Fatalf("bzip2 command error = %v", err)
	}
}

func writeXz(t *testing.T, assetPath string, contents []byte) {
	t.Helper()
	file, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer file.Close()

	xzWriter, err := xz.NewWriter(file)
	if err != nil {
		t.Fatalf("xz NewWriter() error = %v", err)
	}
	if _, err := xzWriter.Write(contents); err != nil {
		t.Fatalf("xz Write() error = %v", err)
	}
	if err := xzWriter.Close(); err != nil {
		t.Fatalf("xz Close() error = %v", err)
	}
}

func writeZst(t *testing.T, assetPath string, contents []byte) {
	t.Helper()
	file, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer file.Close()

	zstWriter, err := zstd.NewWriter(file)
	if err != nil {
		t.Fatalf("zstd NewWriter() error = %v", err)
	}
	if _, err := zstWriter.Write(contents); err != nil {
		t.Fatalf("zstd Write() error = %v", err)
	}
	if err := zstWriter.Close(); err != nil {
		t.Fatalf("zstd Close() error = %v", err)
	}
}

func writeTar(t *testing.T, assetPath string, files []archiveFile) {
	t.Helper()
	file, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer file.Close()
	writeTarTo(t, file, files)
}

func writeTarGz(t *testing.T, assetPath string, files []archiveFile) {
	t.Helper()
	file, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer file.Close()

	gzipWriter := gzip.NewWriter(file)
	defer gzipWriter.Close()
	writeTarTo(t, gzipWriter, files)
}

func writeTarXz(t *testing.T, assetPath string, files []archiveFile) {
	t.Helper()
	file, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer file.Close()

	xzWriter, err := xz.NewWriter(file)
	if err != nil {
		t.Fatalf("xz NewWriter() error = %v", err)
	}
	defer xzWriter.Close()
	writeTarTo(t, xzWriter, files)
}

func writeTarZst(t *testing.T, assetPath string, files []archiveFile) {
	t.Helper()
	file, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer file.Close()

	zstWriter, err := zstd.NewWriter(file)
	if err != nil {
		t.Fatalf("zstd NewWriter() error = %v", err)
	}
	defer zstWriter.Close()
	writeTarTo(t, zstWriter, files)
}

func writeTarBz2(t *testing.T, assetPath string, files []archiveFile) {
	t.Helper()
	out, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer out.Close()

	cmd := bzip2Compress(t, nil)
	cmd.Stdout = out
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() error = %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("bzip2 Start() error = %v", err)
	}
	writeTarTo(t, stdin, files)
	if err := stdin.Close(); err != nil {
		t.Fatalf("Stdin Close() error = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("bzip2 Wait() error = %v", err)
	}
}

func bzip2Compress(t *testing.T, contents []byte) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("bzip2", "-c")
	if contents != nil {
		cmd.Stdin = bytes.NewReader(contents)
	}
	return cmd
}

func writeTarTo(t *testing.T, w io.Writer, files []archiveFile) {
	t.Helper()
	tarWriter := tar.NewWriter(w)
	for _, archiveFile := range files {
		header := &tar.Header{
			Name: archiveFile.name,
			Mode: archiveFile.mode,
			Size: int64(len(archiveFile.contents)),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("tar WriteHeader() error = %v", err)
		}
		if _, err := tarWriter.Write(archiveFile.contents); err != nil {
			t.Fatalf("tar Write() error = %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("tar Close() error = %v", err)
	}
}

func writeZip(t *testing.T, assetPath string, files []archiveFile) {
	t.Helper()
	file, err := os.Create(assetPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	zipWriter := zip.NewWriter(file)
	for _, archiveFile := range files {
		header := &zip.FileHeader{Name: archiveFile.name, Method: zip.Store}
		header.SetMode(os.FileMode(archiveFile.mode))
		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			t.Fatalf("zip CreateHeader() error = %v", err)
		}
		if _, err := writer.Write(archiveFile.contents); err != nil {
			t.Fatalf("zip Write() error = %v", err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("zip Close() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("file Close() error = %v", err)
	}
}

func assertExtractionResult(t *testing.T, got []byte, err error, want []byte, errText string) {
	t.Helper()
	if errText != "" {
		if err == nil {
			t.Fatal("ExtractBinary() error = nil, want error")
		}
		if !strings.Contains(err.Error(), errText) {
			t.Errorf("ExtractBinary() error = %q, want text %q", err, errText)
		}
		return
	}
	if err != nil {
		t.Fatalf("ExtractBinary() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ExtractBinary() = %q, want %q", got, want)
	}
}

func TestInstallUpgradeCommitRemovesBackups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	oldBinary := []byte("old binary")
	oldWrapper, oldReal, err := Install(oldBinary, "acme/widget", false)
	if err != nil {
		t.Fatalf("initial Install() error = %v", err)
	}

	transaction, err := InstallUpgrade([]byte("new binary"), "acme/widget", "widget")
	if err != nil {
		t.Fatalf("InstallUpgrade() error = %v", err)
	}
	if transaction.WrapperPath() != oldWrapper || transaction.RealPath() != oldReal {
		t.Fatalf("transaction paths = (%q, %q), want (%q, %q)", transaction.WrapperPath(), transaction.RealPath(), oldWrapper, oldReal)
	}
	if got, err := os.ReadFile(oldReal); err != nil || string(got) != "new binary" {
		t.Fatalf("active real binary = %q, err = %v", got, err)
	}
	for _, target := range []string{oldWrapper, oldReal} {
		if _, err := os.Stat(target + ".old"); err != nil {
			t.Fatalf("backup %q: %v", target+".old", err)
		}
	}

	if err := transaction.Commit(); err != nil {
		t.Fatalf("transaction.Commit() error = %v", err)
	}
	assertNoUpgradeArtifacts(t, oldWrapper, oldReal)
	if got, err := os.ReadFile(oldReal); err != nil || string(got) != "new binary" {
		t.Fatalf("committed real binary = %q, err = %v", got, err)
	}
}

func TestInstallUpgradeRollbackRestoresFilesAfterSwapFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	oldBinary := []byte("old binary")
	oldWrapper, oldReal, err := Install(oldBinary, "acme/widget", false)
	if err != nil {
		t.Fatalf("initial Install() error = %v", err)
	}
	oldWrapperContents, err := os.ReadFile(oldWrapper)
	if err != nil {
		t.Fatalf("read old wrapper: %v", err)
	}

	originalRename := installRename
	t.Cleanup(func() { installRename = originalRename })
	var renameCalls int
	installRename = func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 4 {
			return errors.New("injected wrapper activation failure")
		}
		return os.Rename(oldPath, newPath)
	}

	transaction, err := InstallUpgrade([]byte("new binary"), "acme/widget", "widget")
	if transaction != nil {
		t.Fatal("InstallUpgrade() returned a transaction after a failed swap")
	}
	if err == nil || !strings.Contains(err.Error(), "activate wrapper") {
		t.Fatalf("InstallUpgrade() error = %v, want wrapper activation failure", err)
	}
	if renameCalls != 6 {
		t.Fatalf("rename calls = %d, want 6 including rollback restores", renameCalls)
	}
	if got, err := os.ReadFile(oldReal); err != nil || !bytes.Equal(got, oldBinary) {
		t.Fatalf("restored real binary = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(oldWrapper); err != nil || !bytes.Equal(got, oldWrapperContents) {
		t.Fatalf("restored wrapper = %q, err = %v", got, err)
	}
	assertNoUpgradeArtifacts(t, oldWrapper, oldReal)
}

func TestInstallUpgradePreservesBackupWhenRestoreFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	oldWrapper, oldReal, err := Install([]byte("old binary"), "acme/widget", false)
	if err != nil {
		t.Fatalf("initial Install() error = %v", err)
	}
	oldWrapperContents, err := os.ReadFile(oldWrapper)
	if err != nil {
		t.Fatalf("read old wrapper: %v", err)
	}

	originalRename := installRename
	t.Cleanup(func() { installRename = originalRename })
	var renameCalls int
	installRename = func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 4 || renameCalls == 5 {
			return errors.New("injected rename failure")
		}
		return os.Rename(oldPath, newPath)
	}

	transaction, err := InstallUpgrade([]byte("new binary"), "acme/widget", "widget")
	if transaction != nil {
		t.Fatal("InstallUpgrade() returned a transaction after a failed swap")
	}
	if err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("InstallUpgrade() error = %v, want rollback failure", err)
	}
	if got, err := os.ReadFile(oldReal); err != nil || string(got) != "old binary" {
		t.Fatalf("restored real binary = %q, err = %v", got, err)
	}
	if _, err := os.Stat(oldWrapper); !os.IsNotExist(err) {
		t.Fatalf("wrapper after failed restore exists or could not be checked: %v", err)
	}
	if got, err := os.ReadFile(oldWrapper + ".old"); err != nil || !bytes.Equal(got, oldWrapperContents) {
		t.Fatalf("preserved wrapper backup = %q, err = %v", got, err)
	}
	assertNoUpgradeTemporaryFiles(t, oldWrapper, oldReal)
}

func TestInstallUpgradeCommitReportsCleanupWarnings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	oldWrapper, oldReal, err := Install([]byte("old binary"), "acme/widget", false)
	if err != nil {
		t.Fatalf("initial Install() error = %v", err)
	}
	transaction, err := InstallUpgrade([]byte("new binary"), "acme/widget", "widget")
	if err != nil {
		t.Fatalf("InstallUpgrade() error = %v", err)
	}

	originalRemove := installRemove
	t.Cleanup(func() { installRemove = originalRemove })
	installRemove = func(string) error {
		return errors.New("injected cleanup failure")
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("transaction.Commit() error = %v", err)
	}
	if got := transaction.CleanupWarnings(); len(got) != 2 {
		t.Fatalf("cleanup warnings = %v, want two warnings", got)
	}
	for _, target := range []string{oldWrapper, oldReal} {
		if _, err := os.Stat(target + ".old"); err != nil {
			t.Fatalf("backup %q after cleanup warning: %v", target+".old", err)
		}
	}
}

func assertNoUpgradeArtifacts(t *testing.T, targets ...string) {
	t.Helper()
	seenDirectories := make(map[string]struct{})
	for _, target := range targets {
		if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
			t.Errorf("upgrade backup %q still exists or could not be checked: %v", target+".old", err)
		}
		seenDirectories[filepath.Dir(target)] = struct{}{}
	}
	assertNoUpgradeTemporaryFilesInDirectories(t, seenDirectories)
}

func assertNoUpgradeTemporaryFiles(t *testing.T, targets ...string) {
	t.Helper()
	seenDirectories := make(map[string]struct{})
	for _, target := range targets {
		seenDirectories[filepath.Dir(target)] = struct{}{}
	}
	assertNoUpgradeTemporaryFilesInDirectories(t, seenDirectories)
}

func assertNoUpgradeTemporaryFilesInDirectories(t *testing.T, directories map[string]struct{}) {
	t.Helper()
	for directory := range directories {
		matches, err := filepath.Glob(filepath.Join(directory, ".zenget-*"))
		if err != nil {
			t.Errorf("glob upgrade temporary files in %q: %v", directory, err)
			continue
		}
		if len(matches) > 0 {
			t.Errorf("upgrade temporary files remain in %q: %v", directory, matches)
		}
	}
}

func TestValidPlatform(t *testing.T) {
	tests := []struct {
		goos   string
		goarch string
		want   bool
	}{
		{"linux", "amd64", true},
		{"darwin", "arm64", true},
		{"windows", "386", true},
		{"linux", "arm", true},
		{"plan9", "amd64", false},
		{"linux", "mips", false},
		{"", "amd64", false},
		{"linux", "", false},
	}
	for _, test := range tests {
		t.Run(test.goos+"/"+test.goarch, func(t *testing.T) {
			if got := ValidPlatform(test.goos, test.goarch); got != test.want {
				t.Errorf("ValidPlatform(%q, %q) = %v, want %v", test.goos, test.goarch, got, test.want)
			}
		})
	}
}

func TestSupportedPlatforms(t *testing.T) {
	oses, arches := SupportedPlatforms()
	wantOSes := []string{"darwin", "linux", "windows"}
	wantArches := []string{"386", "amd64", "arm", "arm64"}
	if !reflect.DeepEqual(oses, wantOSes) {
		t.Errorf("SupportedPlatforms() oses = %v, want %v", oses, wantOSes)
	}
	if !reflect.DeepEqual(arches, wantArches) {
		t.Errorf("SupportedPlatforms() arches = %v, want %v", arches, wantArches)
	}
}

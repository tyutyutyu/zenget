package recipe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zenget/internal/assetselector"
)

func validTestRecipe() Recipe {
	selector := assetselector.NewRegex(`^widget-linux-amd64-v[0-9]+\.tar\.gz$`)
	return Recipe{
		SchemaVersion:    SchemaVersion,
		Repository:       "acme/widget",
		SupportedSystems: []string{"DARWIN/ARM64", "linux/amd64"},
		AssetSelector:    &selector,
		ArchiveBinary:    "./bin/widget",
		TargetName:       "widget-tool",
		Checksum: &Checksum{
			Type:    ChecksumTypeExact,
			Pattern: "widget-linux-amd64-v1.tar.gz.sha256",
			Format:  ChecksumFormatRaw,
		},
	}
}

func TestRecipeNormalizeAndMarshalRoundTrip(t *testing.T) {
	original := validTestRecipe()
	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(data), `"schema_version": 1`) || !strings.Contains(string(data), `"supported_systems"`) {
		t.Fatalf("Marshal() = %s", data)
	}

	path := filepath.Join(t.TempDir(), "recipe.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := original
	want.SupportedSystems = []string{"darwin/arm64", "linux/amd64"}
	want.ArchiveBinary = "bin/widget"
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("Load() = %#v, want %#v", loaded, want)
	}
}

func TestRecipeDecodeRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "missing schema", data: `{"repository":"acme/widget"}`, want: "missing schema_version"},
		{name: "missing repository", data: `{"schema_version":1}`, want: "missing repository"},
		{name: "unknown field", data: `{"schema_version":1,"repository":"acme/widget","unknown":true}`, want: "unknown field"},
		{name: "duplicate field", data: `{"schema_version":1,"schema_version":1,"repository":"acme/widget"}`, want: "duplicate JSON object key"},
		{name: "duplicate nested field", data: `{"schema_version":1,"repository":"acme/widget","asset_selector":{"version":1,"type":"substring","pattern":"x","pattern":"y"}}`, want: "duplicate JSON object key"},
		{name: "unsupported version", data: `{"schema_version":2,"repository":"acme/widget"}`, want: "unsupported recipe schema_version"},
		{name: "bad repository", data: `{"schema_version":1,"repository":"acme/widget/extra"}`, want: "invalid recipe repository"},
		{name: "bad platform", data: `{"schema_version":1,"repository":"acme/widget","supported_systems":["plan9/amd64"]}`, want: "unsupported platform"},
		{name: "duplicate normalized platform", data: `{"schema_version":1,"repository":"acme/widget","supported_systems":["linux/amd64"," LINUX/AMD64 "]}`, want: "duplicate platform"},
		{name: "bad selector", data: `{"schema_version":1,"repository":"acme/widget","asset_selector":{"version":1,"type":"regex","pattern":"["}}`, want: "invalid asset_selector"},
		{name: "bad archive path", data: `{"schema_version":1,"repository":"acme/widget","archive_binary":"../widget"}`, want: "invalid archive_binary"},
		{name: "bad target name", data: `{"schema_version":1,"repository":"acme/widget","target_name":"bin/widget"}`, want: "invalid target_name"},
		{name: "bad checksum type", data: `{"schema_version":1,"repository":"acme/widget","checksum":{"type":"glob","pattern":"checksums.txt","format":"raw"}}`, want: "unsupported checksum selector type"},
		{name: "bad checksum regex", data: `{"schema_version":1,"repository":"acme/widget","checksum":{"type":"regex","pattern":"[","format":"raw"}}`, want: "invalid checksum selector regex"},
		{name: "bad checksum format", data: `{"schema_version":1,"repository":"acme/widget","checksum":{"type":"exact","pattern":"checksums.txt","format":"sha512"}}`, want: "unsupported checksum format"},
		{name: "null optional", data: `{"schema_version":1,"repository":"acme/widget","target_name":null}`, want: "must not be null"},
		{name: "trailing value", data: `{"schema_version":1,"repository":"acme/widget"}{}`, want: "multiple JSON values"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode([]byte(test.data), "fixture")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Decode() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRecipeLoadRejectsNonLocalAndNonRegularInputs(t *testing.T) {
	if _, err := Load("https://example.test/recipe.json"); err == nil {
		t.Fatal("Load() accepted URL")
	}
	if _, err := Load("-"); err == nil {
		t.Fatal("Load() accepted stdin marker")
	}

	directory := t.TempDir()
	if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Load(directory) error = %v", err)
	}

	target := filepath.Join(t.TempDir(), "recipe.json")
	if err := os.WriteFile(target, mustRecipeJSON(t), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Load(symlink) error = %v", err)
	}
}

func TestChecksumValidation(t *testing.T) {
	valid := []Checksum{
		{Type: ChecksumTypeExact, Pattern: "checksums.txt", Format: ChecksumFormatRaw},
		{Type: ChecksumTypeRegex, Pattern: `^checksums-.*\.txt$`, Format: ChecksumFormatSHA256Sum},
	}
	for _, checksum := range valid {
		if err := checksum.validate(); err != nil {
			t.Errorf("Checksum.validate(%#v) error = %v", checksum, err)
		}
	}
}

func mustRecipeJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(validTestRecipe())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

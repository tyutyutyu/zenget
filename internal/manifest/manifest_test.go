package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/state"
)

func validManifest() state.Manifest {
	return state.Manifest{
		SchemaVersion: SchemaVersion,
		SchemaURL:     SchemaURL,
		Apps: []state.ManifestApp{{
			Repository:    "acme/widget",
			Provider:      ProviderGitHub,
			Tag:           "v1.2.3",
			Asset:         "widget-linux-amd64.tar.gz",
			PlatformOS:    "linux",
			PlatformArch:  "amd64",
			ArchiveBinary: "bin/widget",
			TargetName:    "widget",
		}},
	}
}

func TestNewManifestIsStableAndSchemaReferenced(t *testing.T) {
	first, err := Marshal(New())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Marshal(New())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("empty manifests differ:\n%s\n---\n%s", first, second)
	}
	want := "{\n  \"schema_version\": 1,\n  \"$schema\": \"" + SchemaURL + "\",\n  \"apps\": []\n}\n"
	if string(first) != want {
		t.Fatalf("empty manifest = %q, want %q", first, want)
	}
}

func TestValidateManifestFixtures(t *testing.T) {
	valid := validManifest()
	if err := Validate(valid); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*state.Manifest)
	}{
		{name: "schema", mutate: func(file *state.Manifest) { file.SchemaVersion = 2 }},
		{name: "schema url", mutate: func(file *state.Manifest) { file.SchemaURL = "https://example.test/schema.json" }},
		{name: "nil apps", mutate: func(file *state.Manifest) { file.Apps = nil }},
		{name: "repository", mutate: func(file *state.Manifest) { file.Apps[0].Repository = "widget" }},
		{name: "provider", mutate: func(file *state.Manifest) { file.Apps[0].Provider = "gitlab" }},
		{name: "tag", mutate: func(file *state.Manifest) { file.Apps[0].Tag = "bad tag" }},
		{name: "platform pair", mutate: func(file *state.Manifest) { file.Apps[0].PlatformArch = "" }},
		{name: "platform value", mutate: func(file *state.Manifest) { file.Apps[0].PlatformOS = "freebsd" }},
		{name: "asset url", mutate: func(file *state.Manifest) { file.Apps[0].Asset = "https://example.test/tool" }},
		{name: "archive absolute", mutate: func(file *state.Manifest) { file.Apps[0].ArchiveBinary = "/tmp/tool" }},
		{name: "archive parent", mutate: func(file *state.Manifest) { file.Apps[0].ArchiveBinary = "bin/../tool" }},
		{name: "target path", mutate: func(file *state.Manifest) { file.Apps[0].TargetName = "bin/tool" }},
		{name: "duplicate repository", mutate: func(file *state.Manifest) {
			file.Apps = append(file.Apps, state.ManifestApp{Repository: "acme/widget", Provider: ProviderGitHub, Tag: "v9.0.0"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file := valid
			file.Apps = append([]state.ManifestApp(nil), valid.Apps...)
			test.mutate(&file)
			if err := Validate(file); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func TestLoadStrictlyValidatesAndCanonicalizes(t *testing.T) {
	directory := t.TempDir()
	file := validManifest()
	file.Apps = append(file.Apps, state.ManifestApp{
		Repository: "zeta/tool",
		Provider:   ProviderGitHub,
		Tag:        "v2.0.0",
	})
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Apps[0].Repository != "acme/widget" || loaded.Apps[1].Repository != "zeta/tool" {
		t.Fatalf("loaded order = %#v", loaded.Apps)
	}

	invalid := map[string]string{
		"unknown field":   `{"schema_version":1,"apps":[],"unknown":true}`,
		"duplicate key":   `{"schema_version":1,"schema_version":1,"apps":[]}`,
		"multiple values": `{"schema_version":1,"apps":[]} {}`,
		"missing schema":  `{"apps":[]}`,
		"missing apps":    `{"schema_version":1}`,
	}
	for name, contents := range invalid {
		t.Run(name, func(t *testing.T) {
			invalidPath := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(invalidPath, []byte(contents), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(invalidPath); err == nil {
				t.Fatalf("Load(%q) error = nil", contents)
			}
		})
	}
	if _, err := Load("https://example.test/manifest.json"); err == nil || !strings.Contains(err.Error(), "URLs and stdin") {
		t.Fatalf("remote Load() error = %v", err)
	}
}

func TestWriteFileProtectsExistingAndSymlinkTargets(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "manifest.json")
	first := []byte("first\n")
	second := []byte("second\n")
	if err := WriteFile(output, first, false); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(output, second, false); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("no-clobber error = %v", err)
	}
	if got, err := os.ReadFile(output); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("output after no-clobber = %q, %v", got, err)
	}
	if err := WriteFile(output, second, true); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte("target\n"), 0644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "link.json")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, overwrite := range []bool{false, true} {
		if err := WriteFile(symlink, []byte("replacement\n"), overwrite); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink overwrite=%t error = %v", overwrite, err)
		}
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "target\n" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".zenget-manifest-") {
			t.Errorf("temporary manifest remains: %s", entry.Name())
		}
	}
}

func TestSchemaDocumentHasPublicDraftAndClosedObjects(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "schemas", "manifest-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Schema      string                     `json:"$schema"`
		ID          string                     `json:"$id"`
		Additional  bool                       `json:"additionalProperties"`
		Properties  map[string]json.RawMessage `json:"properties"`
		Required    []string                   `json:"required"`
		Definitions map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Schema != "https://json-schema.org/draft/2020-12/schema" || schema.ID != SchemaURL || schema.Additional {
		t.Fatalf("schema metadata = %#v", schema)
	}
	for _, required := range []string{"schema_version", "apps"} {
		if !containsString(schema.Required, required) {
			t.Errorf("schema required fields omit %q", required)
		}
	}
	for _, property := range []string{"$schema", "schema_version", "apps"} {
		if _, ok := schema.Properties[property]; !ok {
			t.Errorf("schema properties omit %q", property)
		}
	}
	if _, ok := schema.Definitions["app"]; !ok {
		t.Fatal("schema $defs omit app")
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

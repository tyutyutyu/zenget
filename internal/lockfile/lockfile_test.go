package lockfile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/state"
)

func validLock() Lockfile {
	return Lockfile{
		SchemaVersion: SchemaVersion,
		Apps: []Entry{{
			Repository:           "acme/widget",
			Provider:             ProviderGitHub,
			SelectionFingerprint: "867ffd394609797884a260e4b49a31ef4204568fc1ba75fadd2afc0dcb8e469a",
			Tag:                  "v1.2.3",
			PlatformOS:           "linux",
			PlatformArch:         "amd64",
			PlatformLibc:         UnknownLibc,
			AssetID:              42,
			AssetName:            "widget.tar.gz",
			AssetSize:            12,
			ProviderDigest:       "sha256:" + strings.Repeat("a", 64),
			ArchiveSHA256:        strings.Repeat("b", 64),
		}},
	}
}

func TestFingerprintGolden(t *testing.T) {
	entry := state.ManifestApp{
		Repository:    "acme/widget",
		Provider:      ProviderGitHub,
		Tag:           "v1.2.3",
		Asset:         "widget.tar.gz",
		PlatformOS:    "linux",
		PlatformArch:  "amd64",
		ArchiveBinary: "bin/widget",
		TargetName:    "widget",
	}
	got, err := Fingerprint(entry)
	if err != nil {
		t.Fatal(err)
	}
	want := "867ffd394609797884a260e4b49a31ef4204568fc1ba75fadd2afc0dcb8e469a"
	if got != want {
		t.Fatalf("Fingerprint() = %q, want %q", got, want)
	}
	entry.TargetName = "other"
	other, err := Fingerprint(entry)
	if err != nil {
		t.Fatal(err)
	}
	if other == got {
		t.Fatal("different manifest choices have the same fingerprint")
	}
}

func TestMarshalCanonicalGolden(t *testing.T) {
	lock := validLock()
	lock.Apps = append(lock.Apps, Entry{
		Repository:           "zeta/tool",
		Provider:             ProviderGitHub,
		SelectionFingerprint: strings.Repeat("c", 64),
		Tag:                  "v2.0.0",
		PlatformOS:           "linux",
		PlatformArch:         "amd64",
		PlatformLibc:         UnknownLibc,
		AssetID:              7,
		AssetName:            "tool",
		AssetSize:            3,
		ArchiveSHA256:        strings.Repeat("d", 64),
	})
	data, err := Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "schema_version": 1,
  "apps": [
    {
      "repository": "acme/widget",
      "provider": "github",
      "selection_fingerprint": "867ffd394609797884a260e4b49a31ef4204568fc1ba75fadd2afc0dcb8e469a",
      "tag": "v1.2.3",
      "platform_os": "linux",
      "platform_arch": "amd64",
      "platform_libc": "unknown",
      "asset_id": 42,
      "asset_name": "widget.tar.gz",
      "asset_size": 12,
      "provider_digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "archive_sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    },
    {
      "repository": "zeta/tool",
      "provider": "github",
      "selection_fingerprint": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "tag": "v2.0.0",
      "platform_os": "linux",
      "platform_arch": "amd64",
      "platform_libc": "unknown",
      "asset_id": 7,
      "asset_name": "tool",
      "asset_size": 3,
      "archive_sha256": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    }
  ]
}
`
	if got := string(data); got != want {
		t.Fatalf("canonical lock =\n%s\nwant\n%s", got, want)
	}
}

func TestValidateRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Lockfile)
	}{
		{name: "unknown schema", mutate: func(lock *Lockfile) { lock.SchemaVersion = 2 }},
		{name: "nil apps", mutate: func(lock *Lockfile) { lock.Apps = nil }},
		{name: "provider", mutate: func(lock *Lockfile) { lock.Apps[0].Provider = "gitlab" }},
		{name: "tag", mutate: func(lock *Lockfile) { lock.Apps[0].Tag = "bad tag" }},
		{name: "platform", mutate: func(lock *Lockfile) { lock.Apps[0].PlatformArch = "not-an-arch" }},
		{name: "libc", mutate: func(lock *Lockfile) { lock.Apps[0].PlatformLibc = "bionic" }},
		{name: "fingerprint", mutate: func(lock *Lockfile) { lock.Apps[0].SelectionFingerprint = strings.Repeat("A", 64) }},
		{name: "asset id", mutate: func(lock *Lockfile) { lock.Apps[0].AssetID = 0 }},
		{name: "asset name", mutate: func(lock *Lockfile) { lock.Apps[0].AssetName = "https://evil.example/asset" }},
		{name: "asset size", mutate: func(lock *Lockfile) { lock.Apps[0].AssetSize = -1 }},
		{name: "provider digest", mutate: func(lock *Lockfile) { lock.Apps[0].ProviderDigest = "sha512:" + strings.Repeat("a", 64) }},
		{name: "archive hash", mutate: func(lock *Lockfile) { lock.Apps[0].ArchiveSHA256 = "bad" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lock := validLock()
			tt.mutate(&lock)
			if err := Validate(lock); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func TestLoadFileRejectsUnknownDuplicateAndNonCanonicalJSON(t *testing.T) {
	directory := t.TempDir()
	validData, err := Marshal(validLock())
	if err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(directory, "valid.json")
	if err := os.WriteFile(validPath, validData, 0644); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadFile(validPath); err != nil || len(loaded.Apps) != 1 {
		t.Fatalf("LoadFile(valid) = %#v, %v", loaded, err)
	}
	if loaded, err := Load(validPath); err != nil || len(loaded.Apps) != 1 {
		t.Fatalf("Load(valid) = %#v, %v", loaded, err)
	}

	unknownPath := filepath.Join(directory, "unknown.json")
	unknown := bytes.TrimSpace(validData)
	unknown = append(unknown[:len(unknown)-1], []byte(`,"secret":"not allowed"}`)...)
	if err := os.WriteFile(unknownPath, unknown, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(unknownPath); err == nil {
		t.Fatal("unknown field was accepted")
	}

	duplicatePath := filepath.Join(directory, "duplicate.json")
	if err := os.WriteFile(duplicatePath, []byte(`{"schema_version":1,"schema_version":1,"apps":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(duplicatePath); err == nil {
		t.Fatal("duplicate field was accepted")
	}

	nonCanonical := validLock()
	nonCanonical.Apps = append(nonCanonical.Apps, Entry{
		Repository:           "aa/tool",
		Provider:             ProviderGitHub,
		SelectionFingerprint: strings.Repeat("c", 64),
		Tag:                  "v1.0.0",
		PlatformOS:           "linux",
		PlatformArch:         "amd64",
		PlatformLibc:         UnknownLibc,
		AssetID:              2,
		AssetName:            "tool",
		AssetSize:            1,
		ArchiveSHA256:        strings.Repeat("d", 64),
	})
	nonCanonicalPath := filepath.Join(directory, "order.json")
	nonCanonicalData, err := json.Marshal(nonCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nonCanonicalPath, nonCanonicalData, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(nonCanonicalPath); err == nil {
		t.Fatal("non-canonical entry order was accepted")
	}
}

func TestLoadFileRejectsMissingFieldsAndTrailingData(t *testing.T) {
	tests := map[string]string{
		"missing schema":  `{"apps":[]}`,
		"missing apps":    `{"schema_version":1}`,
		"null apps":       `{"schema_version":1,"apps":null}`,
		"multiple values": `{"schema_version":1,"apps":[]} {}`,
		"non-object":      `[]`,
		"malformed":       `{"schema_version":1,"apps":[`,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.json")
			if err := os.WriteFile(path, []byte(data), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadFile(path); err == nil {
				t.Fatalf("LoadFile(%q) error = nil", data)
			}
		})
	}

	if _, err := LoadFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing lockfile error = nil")
	}
}

func TestWriteFileIsAtomicAndNoClobbering(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "zenget.lock")
	if err := WriteFile(output, []byte("first\n"), false); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(output, []byte("second\n"), false); err == nil {
		t.Fatal("existing lockfile was overwritten without permission")
	}
	if got, err := os.ReadFile(output); err != nil || string(got) != "first\n" {
		t.Fatalf("lockfile after refused write = %q, %v", got, err)
	}
	if err := WriteFile(output, []byte("second\n"), true); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(output); err != nil || string(got) != "second\n" {
		t.Fatalf("lockfile after forced write = %q, %v", got, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".zenget-lock-") {
			t.Errorf("temporary lockfile remains: %s", entry.Name())
		}
	}
	if err := WriteFile(filepath.Join(t.TempDir(), "missing", "lock.json"), []byte("data"), false); err == nil {
		t.Fatal("write into missing directory error = nil")
	}
}

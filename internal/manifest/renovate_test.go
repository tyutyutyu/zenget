package manifest

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

type renovatePreset struct {
	CustomManagers []renovateManager `json:"customManagers"`
}

type renovateManager struct {
	CustomType          string   `json:"customType"`
	Description         string   `json:"description"`
	ManagerFilePatterns []string `json:"managerFilePatterns"`
	MatchStrings        []string `json:"matchStrings"`
	DatasourceTemplate  string   `json:"datasourceTemplate"`
	VersioningTemplate  string   `json:"versioningTemplate"`
}

type renovateMatch struct {
	Start        int
	End          int
	Repository   string
	CurrentValue string
}

func TestRenovatePresetIsStrictAndConfiguresGitHubReleases(t *testing.T) {
	preset := loadRenovatePreset(t)
	if len(preset.CustomManagers) != 1 {
		t.Fatalf("custom manager count = %d, want 1", len(preset.CustomManagers))
	}
	manager := preset.CustomManagers[0]
	if manager.CustomType != "regex" {
		t.Fatalf("customType = %q, want regex", manager.CustomType)
	}
	if len(manager.ManagerFilePatterns) != 1 {
		t.Fatalf("managerFilePatterns = %#v, want one pattern", manager.ManagerFilePatterns)
	}
	if len(manager.MatchStrings) != 1 {
		t.Fatalf("matchStrings = %#v, want one pattern", manager.MatchStrings)
	}
	if manager.DatasourceTemplate != "github-releases" {
		t.Fatalf("datasourceTemplate = %q, want github-releases", manager.DatasourceTemplate)
	}
	if manager.VersioningTemplate != "loose" {
		t.Fatalf("versioningTemplate = %q, want loose", manager.VersioningTemplate)
	}

	filePattern, err := regexp.Compile(stripRenovateDelimiters(t, manager.ManagerFilePatterns[0]))
	if err != nil {
		t.Fatalf("manager file pattern does not compile with Go RE2: %v", err)
	}
	for _, path := range []string{"zenget.json", "projects/zenget.json"} {
		if !filePattern.MatchString(path) {
			t.Errorf("manager file pattern does not match %q", path)
		}
	}
	for _, path := range []string{"zenget.json.bak", "manifest.json", "projects/zenget.json.bak"} {
		if filePattern.MatchString(path) {
			t.Errorf("manager file pattern unexpectedly matches %q", path)
		}
	}

	managerPattern, err := regexp.Compile(manager.MatchStrings[0])
	if err != nil {
		t.Fatalf("match string does not compile with Go RE2: %v", err)
	}
	for _, forbidden := range []string{"(?=", "(?!", "(?<=", "(?<!", "\\1"} {
		if strings.Contains(manager.MatchStrings[0], forbidden) {
			t.Errorf("match string contains unsupported regex construct %q", forbidden)
		}
	}
	for _, name := range []string{"depName", "currentValue"} {
		if managerPattern.SubexpIndex(name) < 0 {
			t.Errorf("match string does not define named capture %q", name)
		}
	}
}

func TestRenovateManagerExtractsIndependentManifestEntries(t *testing.T) {
	preset := loadRenovatePreset(t)
	managerPattern := compileMatchString(t, preset.CustomManagers[0])
	content := readFixture(t, "renovate-valid.json")
	matches := findRenovateMatches(managerPattern, content)

	want := []renovateMatch{
		{Repository: "acme/widget", CurrentValue: "v1.2.3"},
		{Repository: "zeta/tool", CurrentValue: "release-2025.09"},
	}
	if len(matches) != len(want) {
		t.Fatalf("match count = %d, want %d (%#v)", len(matches), len(want), matches)
	}
	for index := range want {
		if matches[index].Repository != want[index].Repository || matches[index].CurrentValue != want[index].CurrentValue {
			t.Errorf("match[%d] = %#v, want %#v", index, matches[index], want[index])
		}
	}

	manifestPath := writeFixture(t, "valid.json", content)
	if _, err := Load(manifestPath); err != nil {
		t.Fatalf("positive fixture is not a valid schema-v1 manifest: %v", err)
	}
}

func TestRenovateManagerSafelyRejectsLegacyAndCrossEntryFixture(t *testing.T) {
	preset := loadRenovatePreset(t)
	managerPattern := compileMatchString(t, preset.CustomManagers[0])
	content := readFixture(t, "renovate-invalid.json")
	matches := findRenovateMatches(managerPattern, content)
	if len(matches) != 1 || matches[0].Repository != "legacy/tool" {
		t.Fatalf("negative fixture matches = %#v, want only legacy/tool", matches)
	}
	matchText := content[matches[0].Start:matches[0].End]
	if strings.Contains(matchText, `"split/first"`) || strings.Contains(matchText, `"v9.9.9"`) {
		t.Fatal("manager captured provider/tag from a different manifest entry")
	}

	manifestPath := writeFixture(t, "invalid.json", content)
	if _, err := Load(manifestPath); err == nil {
		t.Fatal("legacy fixture loaded successfully, want schema validation error")
	}
}

func TestRenovateManagerReplacesOnlyCurrentValueAndKeepsManifestValid(t *testing.T) {
	preset := loadRenovatePreset(t)
	managerPattern := compileMatchString(t, preset.CustomManagers[0])
	content := readFixture(t, "renovate-valid.json")
	originalPath := writeFixture(t, "original.json", content)
	original, err := Load(originalPath)
	if err != nil {
		t.Fatalf("load original fixture: %v", err)
	}

	updates := map[string]string{
		"acme/widget": "v2.0.0",
		"zeta/tool":   "release-2026.01",
	}
	updatedContent := replaceRenovateTags(content, managerPattern, updates)
	wantContent := strings.Replace(content, `"v1.2.3"`, `"v2.0.0"`, 1)
	wantContent = strings.Replace(wantContent, `"release-2025.09"`, `"release-2026.01"`, 1)
	if updatedContent != wantContent {
		t.Fatalf("tag replacement changed content outside currentValue captures:\n got:\n%s\nwant:\n%s", updatedContent, wantContent)
	}

	updatedPath := writeFixture(t, "updated.json", updatedContent)
	updated, err := Load(updatedPath)
	if err != nil {
		t.Fatalf("updated fixture is invalid: %v", err)
	}
	wantManifest := original
	wantManifest.Apps[0].Tag = updates[wantManifest.Apps[0].Repository]
	wantManifest.Apps[1].Tag = updates[wantManifest.Apps[1].Repository]
	if !reflect.DeepEqual(updated, wantManifest) {
		t.Fatalf("updated manifest = %#v, want %#v", updated, wantManifest)
	}
}

func loadRenovatePreset(t *testing.T) renovatePreset {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "renovate", "zenget.json"))
	if err != nil {
		t.Fatalf("read Renovate preset: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var preset renovatePreset
	if err := decoder.Decode(&preset); err != nil {
		t.Fatalf("decode Renovate preset: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("Renovate preset has trailing JSON: %v", err)
	}
	return preset
}

func compileMatchString(t *testing.T, manager renovateManager) *regexp.Regexp {
	t.Helper()
	pattern, err := regexp.Compile(manager.MatchStrings[0])
	if err != nil {
		t.Fatalf("compile match string: %v", err)
	}
	return pattern
}

func stripRenovateDelimiters(t *testing.T, value string) string {
	t.Helper()
	if len(value) < 2 || value[0] != '/' || value[len(value)-1] != '/' {
		t.Fatalf("Renovate regex %q has no / delimiters", value)
	}
	return value[1 : len(value)-1]
}

func findRenovateMatches(pattern *regexp.Regexp, content string) []renovateMatch {
	indices := pattern.FindAllStringSubmatchIndex(content, -1)
	depNameIndex := pattern.SubexpIndex("depName")
	currentValueIndex := pattern.SubexpIndex("currentValue")
	matches := make([]renovateMatch, 0, len(indices))
	for _, index := range indices {
		matches = append(matches, renovateMatch{
			Start:        index[0],
			End:          index[1],
			Repository:   content[index[2*depNameIndex]:index[2*depNameIndex+1]],
			CurrentValue: content[index[2*currentValueIndex]:index[2*currentValueIndex+1]],
		})
	}
	return matches
}

func replaceRenovateTags(content string, pattern *regexp.Regexp, updates map[string]string) string {
	indices := pattern.FindAllStringSubmatchIndex(content, -1)
	depNameIndex := pattern.SubexpIndex("depName")
	currentValueIndex := pattern.SubexpIndex("currentValue")
	var output strings.Builder
	output.Grow(len(content))
	cursor := 0
	for _, index := range indices {
		repository := content[index[2*depNameIndex]:index[2*depNameIndex+1]]
		replacement, ok := updates[repository]
		if !ok {
			continue
		}
		currentStart := index[2*currentValueIndex]
		currentEnd := index[2*currentValueIndex+1]
		output.WriteString(content[cursor:currentStart])
		output.WriteString(replacement)
		cursor = currentEnd
	}
	output.WriteString(content[cursor:])
	return output.String()
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return string(data)
}

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture %q: %v", name, err)
	}
	return path
}

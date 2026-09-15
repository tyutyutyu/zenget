package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"zenget/internal/github"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

func TestRunListEmptyState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var output bytes.Buffer
	if err := printList(mustLoadState(t), nil, &output, false, time.Now().UTC()); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got, want := output.String(), "No applications installed.\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunListTableSortedByName(t *testing.T) {
	saveFixtureState(t, map[string]state.App{
		"zzz/tool":    {Version: "v2.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/tool"},
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 19, 14, 5, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	var output bytes.Buffer
	if err := printList(mustLoadState(t), nil, &output, false, time.Now().UTC()); err != nil {
		t.Fatalf("list: %v", err)
	}
	text := output.String()
	for _, want := range []string{"NAME", "VERSION", "INSTALLED", "PATH", "acme/widget", "v1.0.0", "2026-08-19 14:05", "zzz/tool", "v2.0.0"} {
		if !strings.Contains(text, want) {
			t.Errorf("table output missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "acme/widget") > strings.Index(text, "zzz/tool") {
		t.Errorf("rows not sorted by name:\n%s", text)
	}
}

func TestRunListJSONRoundTrip(t *testing.T) {
	apps := map[string]state.App{
		"acme/widget": {
			Version:     "v1.0.0",
			InstalledAt: time.Date(2026, 8, 19, 14, 5, 0, 0, time.UTC),
			DownloadURL: "https://example.com/widget.tar.gz",
			InstallPath: "/home/u/.local/bin/widget",
			XXH3_64:     "0123456789abcdef",
		},
	}
	saveFixtureState(t, apps)

	var output bytes.Buffer
	if err := printList(mustLoadState(t), nil, &output, true, time.Now().UTC()); err != nil {
		t.Fatalf("list --json: %v", err)
	}

	var got map[string]state.App
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse JSON output: %v", err)
	}
	if !reflect.DeepEqual(got, apps) {
		t.Fatalf("JSON round trip = %+v, want %+v", got, apps)
	}
}

// mustLoadState loads the state store, failing the test on error.
func mustLoadState(t *testing.T) *state.State {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

// saveFixtureState points the state store at a temporary config dir and saves
// apps into it.
func saveFixtureState(t *testing.T, apps map[string]state.App) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st := &state.State{Apps: apps}
	if err := st.Save(); err != nil {
		t.Fatalf("save fixture state: %v", err)
	}
}

func TestFormatLatest(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	age := 5
	v := "v1.4.0"
	if got := formatLatest(latestInfo{Version: &v, AgeDays: &age}, now); got != "v1.4.0 (5d old)" {
		t.Errorf("formatLatest = %q, want v1.4.0 (5d old)", got)
	}
	if got := formatLatest(latestInfo{}, now); got != "?" {
		t.Errorf("formatLatest missing info = %q, want ?", got)
	}
}

func TestAgeDays(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		published time.Time
		want      int
	}{
		{time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), 0},
		{time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC), 1},
		{time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), 6},
		{time.Date(2026, 8, 25, 23, 59, 0, 0, time.UTC), 5},
	}
	for _, test := range tests {
		if got := ageDays(test.published, now); got != test.want {
			t.Errorf("ageDays(%v) = %d, want %d", test.published, got, test.want)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	if org, repo, ok := splitRepo("acme/widget"); !ok || org != "acme" || repo != "widget" {
		t.Errorf("splitRepo(acme/widget) = %q, %q, %v, want acme, widget, true", org, repo, ok)
	}
	for _, bad := range []string{"acme", "acme/", "/widget", "acme/widget/extra", ""} {
		if _, _, ok := splitRepo(bad); ok {
			t.Errorf("splitRepo(%q) = true, want false", bad)
		}
	}
}

// newListTestServer returns a test server that serves the latest release
// endpoint. handler controls the response body per repository path; returning
// nil means a 404 response.
func newListTestServer(t *testing.T, handler func(path string) map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		payload := handler(r.URL.Path)
		if payload == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

func newListTestClient(t *testing.T, server *httptest.Server) *github.Client {
	t.Helper()
	client := github.New()
	client.BaseURL = server.URL
	return client
}

// runListWithTestFlags drives runListWithClient with explicit flag values,
// restoring the package flag variables afterwards.
func runListWithTestFlags(t *testing.T, client *github.Client, updates, asJSON bool, colorMode string, now time.Time) (*bytes.Buffer, error) {
	t.Helper()
	return runListWithAllTestFlags(t, client, updates, asJSON, false, colorMode, now)
}

func runListWithAllTestFlags(t *testing.T, client *github.Client, updates, asJSON, check bool, colorMode string, now time.Time) (*bytes.Buffer, error) {
	t.Helper()
	listUpdates = updates
	listJSON = asJSON
	listCheck = check
	colorFlag = colorMode
	t.Cleanup(func() {
		listUpdates = false
		listJSON = false
		listCheck = false
		colorFlag = colorModeAuto
	})
	cmd, output, _ := newBufferedCommand()
	return output, runListWithClient(cmd.Context(), client, cmd, now)
}

func TestValidateListCheckRequiresUpdates(t *testing.T) {
	listCheck = true
	listUpdates = false
	t.Cleanup(func() {
		listCheck = false
		listUpdates = false
	})

	err := validateListArgs(listCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--check requires --updates") {
		t.Fatalf("error = %v, want --check requires --updates", err)
	}
}

func TestRunListCheckStatuses(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		installed string
		latest    map[string]any
		wantErr   error
	}{
		{
			name:      "current",
			installed: "v1.0.0",
			latest:    map[string]any{"tag_name": "v1.0.0", "published_at": "2026-08-25T10:00:00Z"},
		},
		{
			name:      "behind",
			installed: "v1.0.0",
			latest:    map[string]any{"tag_name": "v1.1.0", "published_at": "2026-08-25T10:00:00Z"},
			wantErr:   errUpdatesAvailable,
		},
		{
			name:      "lookup error",
			installed: "v1.0.0",
			wantErr:   errUpdateCheckIndeterminate,
		},
		{
			name:      "malformed installed version",
			installed: "not-a-version",
			latest:    map[string]any{"tag_name": "v1.0.0", "published_at": "2026-08-25T10:00:00Z"},
			wantErr:   errUpdateCheckIndeterminate,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			saveFixtureState(t, map[string]state.App{
				"acme/widget": {Version: test.installed, InstallPath: "/home/u/.local/bin/widget"},
			})
			server := newListTestServer(t, func(path string) map[string]any {
				if path == "/repos/acme/widget/releases/latest" {
					return test.latest
				}
				return nil
			})

			output, err := runListWithAllTestFlags(t, newListTestClient(t, server), true, false, true, colorModeNever, now)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			for _, want := range []string{"NAME", "VERSION", "LATEST", "acme/widget"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("output missing %q:\n%s", want, output.String())
				}
			}
		})
	}
}

func TestRunListCheckIndeterminateTakesPrecedence(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/behind": {Version: "v1.0.0", InstallPath: "/home/u/.local/bin/behind"},
		"acme/error":  {Version: "v1.0.0", InstallPath: "/home/u/.local/bin/error"},
	})
	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/behind/releases/latest" {
			return map[string]any{"tag_name": "v2.0.0", "published_at": "2026-08-25T10:00:00Z"}
		}
		return nil
	})

	_, err := runListWithAllTestFlags(t, newListTestClient(t, server), true, false, true, colorModeNever, now)
	if !errors.Is(err, errUpdateCheckIndeterminate) {
		t.Fatalf("error = %v, want indeterminate status", err)
	}
}

func TestRunListCheckJSONPreservesOutput(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstallPath: "/home/u/.local/bin/widget"},
	})
	server := newListTestServer(t, func(path string) map[string]any {
		return map[string]any{"tag_name": "v1.1.0", "published_at": "2026-08-25T10:00:00Z"}
	})

	output, err := runListWithAllTestFlags(t, newListTestClient(t, server), true, true, true, colorModeNever, now)
	if !errors.Is(err, errUpdatesAvailable) {
		t.Fatalf("error = %v, want updates available", err)
	}
	var got map[string]struct {
		LatestVersion string `json:"latest_version"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse JSON output: %v\n%s", err, output.String())
	}
	if got["acme/widget"].LatestVersion != "v1.1.0" {
		t.Errorf("latest_version = %q, want v1.1.0", got["acme/widget"].LatestVersion)
	}
}

func TestRunListWithUpdatesTable(t *testing.T) {
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{
				"tag_name":     "v1.1.0",
				"published_at": "2026-08-25T10:00:00Z",
			}
		}
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, true, false, colorModeAuto, now)
	if err != nil {
		t.Fatalf("list --updates: %v", err)
	}
	text := output.String()
	for _, want := range []string{"NAME", "VERSION", "LATEST", "INSTALLED", "PATH", "v1.0.0", "v1.1.0 (5d old)"} {
		if !strings.Contains(text, want) {
			t.Errorf("table output missing %q:\n%s", want, text)
		}
	}
	if got := ageDays(time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC), now); got != 5 {
		t.Errorf("computed age = %d, want 5", got)
	}
}

func TestRunListWithUpdatesPerRowError(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
		"bad-key":     {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/bad"},
	})

	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{
				"tag_name":     "v1.1.0",
				"published_at": "2026-08-25T10:00:00Z",
			}
		}
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, true, false, colorModeAuto, now)
	if err != nil {
		t.Fatalf("list --updates: %v", err)
	}
	text := output.String()
	if !strings.Contains(text, "v1.1.0 (5d old)") {
		t.Errorf("successful row missing update:\n%s", text)
	}
	if !strings.Contains(text, "bad-key") || !strings.Contains(text, "?") {
		t.Errorf("failed row missing ?:\n%s", text)
	}
}

func TestRunListWithUpdatesJSON(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{
				"tag_name":     "v1.1.0",
				"published_at": "2026-08-25T10:00:00Z",
			}
		}
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, true, true, colorModeAuto, now)
	if err != nil {
		t.Fatalf("list --json --updates: %v", err)
	}

	var got map[string]struct {
		Version       string `json:"version"`
		LatestVersion string `json:"latest_version"`
		LatestAgeDays int    `json:"latest_age_days"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse JSON output: %v\n%s", err, output.String())
	}
	app, ok := got["acme/widget"]
	if !ok {
		t.Fatalf("JSON output missing acme/widget: %s", output.String())
	}
	if app.Version != "v1.0.0" {
		t.Errorf("version = %q, want v1.0.0", app.Version)
	}
	if app.LatestVersion != "v1.1.0" {
		t.Errorf("latest_version = %q, want v1.1.0", app.LatestVersion)
	}
	if app.LatestAgeDays != 5 {
		t.Errorf("latest_age_days = %d, want 5", app.LatestAgeDays)
	}
}

// TestRunListJSONNeverColored locks the guarantee that --json output contains
// no ANSI codes even when colors are forced on.
func TestRunListJSONNeverColored(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{"tag_name": "v1.1.0", "published_at": "2026-08-25T10:00:00Z"}
		}
		return nil
	})
	client := newListTestClient(t, server)

	for _, updates := range []bool{false, true} {
		output, err := runListWithTestFlags(t, client, updates, true, colorModeAlways, now)
		if err != nil {
			t.Fatalf("list --json: %v", err)
		}
		if strings.Contains(output.String(), "\033[") {
			t.Errorf("JSON output contains ANSI codes with --color always:\n%s", output.String())
		}
	}
}

func TestRunListUpdatesDisabledNoNetwork(t *testing.T) {
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	called := false
	server := newListTestServer(t, func(path string) map[string]any {
		called = true
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, false, false, colorModeAuto, time.Now().UTC())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if called {
		t.Error("server received a request even though --updates was not set")
	}
	text := output.String()
	if strings.Contains(text, "LATEST") {
		t.Errorf("output contains LATEST column without --updates:\n%s", text)
	}
}

func TestRunListPlainJSONUnchanged(t *testing.T) {
	apps := map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 19, 14, 5, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	}
	saveFixtureState(t, apps)

	server := newListTestServer(t, func(path string) map[string]any { return nil })
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, false, true, colorModeAuto, time.Now().UTC())
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}

	var got map[string]state.App
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse JSON output: %v", err)
	}
	if !reflect.DeepEqual(got, apps) {
		t.Fatalf("JSON round trip = %+v, want %+v", got, apps)
	}
}

func TestSemverDiff(t *testing.T) {
	tests := []struct {
		installed string
		latest    string
		want      diffLevel
	}{
		{"v1.0.0", "v1.0.0", diffSame},
		{"1.0.0", "v1.0.0", diffSame},
		{"v1.0.0", "v1.0.1", diffPatch},
		{"v1.0.0", "v1.1.0", diffMinor},
		{"v1.0.0", "v2.0.0", diffMajor},
		{"v1.0.0", "v2.0.1", diffMajor},
		{"not-a-version", "v1.0.0", diffInvalid},
		{"v1.0.0", "not-a-version", diffInvalid},
	}
	for _, test := range tests {
		if got := semverDiff(test.installed, test.latest); got != test.want {
			t.Errorf("semverDiff(%q, %q) = %d, want %d", test.installed, test.latest, got, test.want)
		}
	}
}

func TestVersionBehind(t *testing.T) {
	tests := []struct {
		installed string
		latest    string
		behind    bool
		valid     bool
	}{
		{installed: "v1.0.0", latest: "v1.0.0", valid: true},
		{installed: "v1.0.0", latest: "v1.0.1", behind: true, valid: true},
		{installed: "v1.2.0", latest: "v1.1.9", valid: true},
		{installed: "v2.0.0", latest: "v1.9.9", valid: true},
		{installed: "bad", latest: "v1.0.0"},
	}
	for _, test := range tests {
		behind, valid := versionBehind(test.installed, test.latest)
		if behind != test.behind || valid != test.valid {
			t.Errorf("versionBehind(%q, %q) = (%v, %v), want (%v, %v)", test.installed, test.latest, behind, valid, test.behind, test.valid)
		}
	}
}

func TestRunListWithUpdatesColoredVersions(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/same":  {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/same"},
		"acme/patch": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/patch"},
		"acme/minor": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/minor"},
		"acpe/major": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/major"},
	})

	server := newListTestServer(t, func(path string) map[string]any {
		switch path {
		case "/repos/acme/same/releases/latest":
			return map[string]any{"tag_name": "v1.0.0", "published_at": "2026-08-25T10:00:00Z"}
		case "/repos/acme/patch/releases/latest":
			return map[string]any{"tag_name": "v1.0.1", "published_at": "2026-08-25T10:00:00Z"}
		case "/repos/acme/minor/releases/latest":
			return map[string]any{"tag_name": "v1.1.0", "published_at": "2026-08-25T10:00:00Z"}
		case "/repos/acpe/major/releases/latest":
			return map[string]any{"tag_name": "v2.0.0", "published_at": "2026-08-25T10:00:00Z"}
		}
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, true, false, colorModeAlways, now)
	if err != nil {
		t.Fatalf("list --updates: %v", err)
	}
	text := output.String()

	for _, want := range []string{
		colorWhite + "NAME" + colorReset,
		colorWhite + "VERSION" + colorReset,
		colorWhite + "LATEST" + colorReset,
		colorWhite + "INSTALLED" + colorReset,
		colorWhite + "PATH" + colorReset,
		colorGreen + "v1.0.0" + colorReset,
		colorYellow + "v1.0.0" + colorReset,
		colorOrange + "v1.0.0" + colorReset,
		colorRed + "v1.0.0" + colorReset,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

func TestRunListNoColorWithoutUpdates(t *testing.T) {
	now := time.Now().UTC()
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	server := newListTestServer(t, func(path string) map[string]any { return nil })
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, false, false, colorModeAuto, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(output.String(), "\033[") {
		t.Errorf("output contains ANSI codes without --updates:\n%s", output.String())
	}
}

// TestRunListUpdatesAutoModeNoAnsiOnBuffer locks the default behavior: in
// auto mode a non-terminal destination (pipe, file, test buffer) never
// receives ANSI codes, even without NO_COLOR.
func TestRunListUpdatesAutoModeNoAnsiOnBuffer(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{"tag_name": "v1.1.0", "published_at": "2026-08-25T10:00:00Z"}
		}
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, true, false, colorModeAuto, now)
	if err != nil {
		t.Fatalf("list --updates: %v", err)
	}
	text := output.String()
	if strings.Contains(text, "\033[") {
		t.Errorf("auto mode wrote ANSI codes to a non-terminal:\n%s", text)
	}
	for _, want := range []string{"NAME", "VERSION", "LATEST", "v1.0.0", "v1.1.0 (5d old)"} {
		if !strings.Contains(text, want) {
			t.Errorf("uncolored output missing %q:\n%s", want, text)
		}
	}
}

// TestRunListUpdatesAlwaysOverridesNoColor verifies that --color always
// colors the output even when NO_COLOR is set to a disabling value.
func TestRunListUpdatesAlwaysOverridesNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{"tag_name": "v1.1.0", "published_at": "2026-08-25T10:00:00Z"}
		}
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, true, false, colorModeAlways, now)
	if err != nil {
		t.Fatalf("list --updates --color always: %v", err)
	}
	text := output.String()
	for _, want := range []string{
		colorWhite + "NAME" + colorReset,
		colorOrange + "v1.0.0" + colorReset,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q despite --color always:\n%s", want, text)
		}
	}
}

// TestRunListUpdatesNeverDisablesColors verifies that --color never keeps
// the table plain even when colors would otherwise apply.
func TestRunListUpdatesNeverDisablesColors(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{"tag_name": "v1.1.0", "published_at": "2026-08-25T10:00:00Z"}
		}
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, true, false, colorModeNever, now)
	if err != nil {
		t.Fatalf("list --updates --color never: %v", err)
	}
	if strings.Contains(output.String(), "\033[") {
		t.Errorf("--color never output contains ANSI codes:\n%s", output.String())
	}
}

func TestRunListUpdatesInvalidVersionNeutral(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "not-a-version", InstalledAt: time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), InstallPath: "/home/u/.local/bin/widget"},
	})

	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{"tag_name": "v1.0.0", "published_at": "2026-08-25T10:00:00Z"}
		}
		return nil
	})
	client := newListTestClient(t, server)

	output, err := runListWithTestFlags(t, client, true, false, colorModeAlways, now)
	if err != nil {
		t.Fatalf("list --updates: %v", err)
	}
	text := output.String()
	if strings.Contains(text, colorGreen+"not-a-version") || strings.Contains(text, colorYellow+"not-a-version") ||
		strings.Contains(text, colorOrange+"not-a-version") || strings.Contains(text, colorRed+"not-a-version") {
		t.Errorf("malformed version was colored:\n%s", text)
	}
	if !strings.Contains(text, "not-a-version") {
		t.Errorf("version text missing from output:\n%s", text)
	}
}

// writeUsageLog creates a JSONL usage log for app with a single event at start.
func writeUsageLog(t *testing.T, app string, start time.Time) {
	t.Helper()
	logPath, err := wrapper.UsageLogPath(app)
	if err != nil {
		t.Fatalf("usage log path for %q: %v", app, err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatalf("create usage log dir: %v", err)
	}
	line := fmt.Sprintf(`{"start":"%s","duration_s":1,"exit_code":0}`+"\n", start.Format(time.RFC3339))
	if err := os.WriteFile(logPath, []byte(line), 0644); err != nil {
		t.Fatalf("write usage log: %v", err)
	}
}

func runListUnusedWithTestFlags(t *testing.T, asJSON bool, now time.Time) (*bytes.Buffer, error) {
	t.Helper()
	listUnused = true
	listJSON = asJSON
	t.Cleanup(func() {
		listUnused = false
		listJSON = false
	})
	cmd, output, _ := newBufferedCommand()
	return output, runListWithClient(cmd.Context(), nil, cmd, now)
}

func TestRunListUnusedEmptyState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	output, err := runListUnusedWithTestFlags(t, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("list --unused: %v", err)
	}
	if got, want := output.String(), "No apps unused for 90 days or more.\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestRunListUnusedNoOldApps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: now.Add(-7 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/widget"},
	})
	writeUsageLog(t, "acme/widget", now.Add(-10*24*time.Hour))

	output, err := runListUnusedWithTestFlags(t, false, now)
	if err != nil {
		t.Fatalf("list --unused: %v", err)
	}
	if got, want := output.String(), "No apps unused for 90 days or more.\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestRunListUnusedBucketsAndOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

	saveFixtureState(t, map[string]state.App{
		"acme/year":    {Version: "v1.0.0", InstalledAt: now.Add(-400 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/year"},
		"acme/half":    {Version: "v1.0.0", InstalledAt: now.Add(-200 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/half"},
		"acme/quarter": {Version: "v1.0.0", InstalledAt: now.Add(-100 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/quarter"},
		"acme/fresh":   {Version: "v1.0.0", InstalledAt: now.Add(-10 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/fresh"},
		"acme/never":   {Version: "v1.0.0", InstalledAt: now.Add(-30 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/never"},
	})
	writeUsageLog(t, "acme/year", now.Add(-365*24*time.Hour))
	writeUsageLog(t, "acme/half", now.Add(-180*24*time.Hour))
	writeUsageLog(t, "acme/quarter", now.Add(-90*24*time.Hour))
	writeUsageLog(t, "acme/fresh", now.Add(-10*24*time.Hour))
	// acme/never has no usage log

	output, err := runListUnusedWithTestFlags(t, false, now)
	if err != nil {
		t.Fatalf("list --unused: %v", err)
	}
	text := output.String()

	for _, want := range []string{"NAME", "LAST USE", "DAYS", "PATH", "acme/year", "acme/half", "acme/quarter", "acme/never", "never", "365", "180", "90"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "acme/fresh") {
		t.Errorf("fresh app should not appear:\n%s", text)
	}

	// never group first, then year, half, quarter; descending days within groups.
	positions := map[string]int{
		"acme/never":     strings.Index(text, "acme/never"),
		"1 year or more": strings.Index(text, "1 year or more"),
		"acme/year":      strings.Index(text, "acme/year"),
		"6-12 months":    strings.Index(text, "6-12 months"),
		"acme/half":      strings.Index(text, "acme/half"),
		"3-6 months":     strings.Index(text, "3-6 months"),
		"acme/quarter":   strings.Index(text, "acme/quarter"),
	}
	for key, pos := range positions {
		if pos == -1 {
			t.Errorf("missing %q in output:\n%s", key, text)
		}
	}
	if positions["acme/never"] >= positions["acme/year"] {
		t.Errorf("never app should appear before used apps:\n%s", text)
	}
	if positions["acme/year"] >= positions["acme/half"] {
		t.Errorf("year app should appear before half app:\n%s", text)
	}
	if positions["acme/half"] >= positions["acme/quarter"] {
		t.Errorf("half app should appear before quarter app:\n%s", text)
	}
}

func TestRunListUnusedBoundaryDays(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

	saveFixtureState(t, map[string]state.App{
		"acme/b89":  {Version: "v1.0.0", InstalledAt: now.Add(-100 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/b89"},
		"acme/b90":  {Version: "v1.0.0", InstalledAt: now.Add(-100 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/b90"},
		"acme/b179": {Version: "v1.0.0", InstalledAt: now.Add(-200 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/b179"},
		"acme/b180": {Version: "v1.0.0", InstalledAt: now.Add(-200 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/b180"},
		"acme/b364": {Version: "v1.0.0", InstalledAt: now.Add(-400 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/b364"},
		"acme/b365": {Version: "v1.0.0", InstalledAt: now.Add(-400 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/b365"},
	})
	writeUsageLog(t, "acme/b89", now.Add(-89*24*time.Hour))
	writeUsageLog(t, "acme/b90", now.Add(-90*24*time.Hour))
	writeUsageLog(t, "acme/b179", now.Add(-179*24*time.Hour))
	writeUsageLog(t, "acme/b180", now.Add(-180*24*time.Hour))
	writeUsageLog(t, "acme/b364", now.Add(-364*24*time.Hour))
	writeUsageLog(t, "acme/b365", now.Add(-365*24*time.Hour))

	output, err := runListUnusedWithTestFlags(t, false, now)
	if err != nil {
		t.Fatalf("list --unused: %v", err)
	}
	text := output.String()

	if strings.Contains(text, "acme/b89") {
		t.Errorf("89-day app should not appear:\n%s", text)
	}
	for _, want := range []string{"acme/b90", "acme/b179", "acme/b180", "acme/b364", "acme/b365"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}

	// b90 is in 3-6 months, b179/b180 in 6-12 months, b364/b365 in 1 year+.
	if !strings.Contains(text, "3-6 months") || !strings.Contains(text, "6-12 months") || !strings.Contains(text, "1 year or more") {
		t.Errorf("expected all three buckets:\n%s", text)
	}
}

func TestRunListUnusedJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

	saveFixtureState(t, map[string]state.App{
		"acme/used":  {Version: "v1.0.0", InstalledAt: now.Add(-100 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/used"},
		"acme/never": {Version: "v1.0.0", InstalledAt: now.Add(-100 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/never"},
	})
	writeUsageLog(t, "acme/used", now.Add(-120*24*time.Hour))
	// acme/never has no usage log

	output, err := runListUnusedWithTestFlags(t, true, now)
	if err != nil {
		t.Fatalf("list --unused --json: %v", err)
	}

	var got []struct {
		Name             string `json:"name"`
		InstallPath      string `json:"install_path"`
		DaysSinceLastUse *int   `json:"days_since_last_use"`
		Bucket           string `json:"bucket"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse JSON output: %v\n%s", err, output.String())
	}
	if len(got) != 2 {
		t.Fatalf("got %d apps, want 2", len(got))
	}

	if got[0].Name != "acme/never" {
		t.Errorf("first app = %q, want acme/never", got[0].Name)
	}
	if got[0].DaysSinceLastUse != nil {
		t.Errorf("never app days_since_last_use = %v, want nil", got[0].DaysSinceLastUse)
	}
	if got[0].Bucket != "never" {
		t.Errorf("never app bucket = %q, want never", got[0].Bucket)
	}

	if got[1].Name != "acme/used" {
		t.Errorf("second app = %q, want acme/used", got[1].Name)
	}
	if got[1].DaysSinceLastUse == nil || *got[1].DaysSinceLastUse != 120 {
		t.Errorf("used app days_since_last_use = %v, want 120", got[1].DaysSinceLastUse)
	}
	if got[1].Bucket != "3-6 months" {
		t.Errorf("used app bucket = %q, want 3-6 months", got[1].Bucket)
	}
}

func TestRunListUnusedIgnoresUpdates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)

	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstalledAt: now.Add(-100 * 24 * time.Hour), InstallPath: "/home/u/.local/bin/widget"},
	})
	// No usage log -> never used.

	called := false
	server := newListTestServer(t, func(path string) map[string]any {
		called = true
		return nil
	})
	client := newListTestClient(t, server)

	listUnused = true
	listUpdates = true
	listJSON = false
	t.Cleanup(func() {
		listUnused = false
		listUpdates = false
		listJSON = false
	})

	cmd, output, _ := newBufferedCommand()
	if err := runListWithClient(cmd.Context(), client, cmd, now); err != nil {
		t.Fatalf("list --unused --updates: %v", err)
	}
	if called {
		t.Error("server received a request even though --unused was set")
	}
	text := output.String()
	if !strings.Contains(text, "acme/widget") || !strings.Contains(text, "never") {
		t.Errorf("unused output missing expected content:\n%s", text)
	}
}

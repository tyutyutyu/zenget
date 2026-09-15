package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/config"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

func TestPrintStatsEmptyState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var output bytes.Buffer
	if err := printStats(&state.State{Apps: map[string]state.App{}}, &output, false); err != nil {
		t.Fatalf("printStats: %v", err)
	}
	if got, want := output.String(), "No applications installed.\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestPrintStatsTableAggregates(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	apps := map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstallPath: "/home/u/.local/bin/widget"},
	}

	logDir := filepath.Join(dataHome, "zenget", "usage")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	logPath := filepath.Join(logDir, "widget.jsonl")
	data := `{"start":"2026-08-25T10:00:00Z","duration_s":2,"exit_code":0}
{"start":"2026-08-25T10:01:00Z","duration_s":4,"exit_code":0}
`
	if err := os.WriteFile(logPath, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var output bytes.Buffer
	if err := printStats(&state.State{Apps: apps}, &output, false); err != nil {
		t.Fatalf("printStats: %v", err)
	}
	text := output.String()
	for _, want := range []string{"NAME", "RUNS", "LAST RUN", "TOTAL", "AVERAGE", "acme/widget", "2", "2026-08-25 10:01", "6.00s", "3.00s"} {
		if !strings.Contains(text, want) {
			t.Errorf("table output missing %q:\n%s", want, text)
		}
	}
}

func TestPrintStatsJSON(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	apps := map[string]state.App{
		"zzz/tool": {Version: "v2.0.0", InstallPath: "/home/u/.local/bin/tool"},
	}

	logDir := filepath.Join(dataHome, "zenget", "usage")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	logPath := filepath.Join(logDir, "tool.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"start":"2026-08-25T12:00:00Z","duration_s":1,"exit_code":0}`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var output bytes.Buffer
	if err := printStats(&state.State{Apps: apps}, &output, true); err != nil {
		t.Fatalf("printStats --json: %v", err)
	}

	var got []map[string]interface{}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse JSON output: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("JSON output has %d entries, want 1", len(got))
	}
	if got[0]["app"] != "zzz/tool" {
		t.Errorf("app = %v, want zzz/tool", got[0]["app"])
	}
	if got[0]["runs"] != float64(1) {
		t.Errorf("runs = %v, want 1", got[0]["runs"])
	}
}

func TestPrintStatsSeparatesNamespacedSameBasenameRepositories(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	apps := map[string]state.App{
		"acme/tool": {
			Version:        "v1.0.0",
			ActiveArtifact: &state.Artifact{Version: "v1.0.0", RealPath: "/data/acme/tool"},
		},
		"other/tool": {
			Version:        "v1.0.0",
			ActiveArtifact: &state.Artifact{Version: "v1.0.0", RealPath: "/data/other/tool"},
		},
	}
	paths := make([]string, 0, len(apps))
	for _, repository := range []string{"acme/tool", "other/tool"} {
		path, err := wrapper.UsageLogPath(repository)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths[0], []byte(`{"start":"2026-08-25T10:00:00Z","duration_s":1,"exit_code":0}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[1], []byte(`{"start":"2026-08-25T10:00:00Z","duration_s":3,"exit_code":0}`), 0644); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := printStats(&state.State{Apps: apps}, &output, true); err != nil {
		t.Fatalf("printStats --json: %v", err)
	}
	var got []struct {
		App  string `json:"app"`
		Runs int    `json:"runs"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse JSON output: %v", err)
	}
	if len(got) != 2 || got[0].App != "acme/tool" || got[0].Runs != 1 || got[1].App != "other/tool" || got[1].Runs != 1 {
		t.Fatalf("stats = %#v, want separate one-run entries", got)
	}
}

func TestPrintStatsHandlesMissingLog(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	apps := map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstallPath: "/home/u/.local/bin/widget"},
	}

	var output bytes.Buffer
	if err := printStats(&state.State{Apps: apps}, &output, false); err != nil {
		t.Fatalf("printStats: %v", err)
	}
	text := output.String()
	if !strings.Contains(text, "acme/widget") {
		t.Errorf("table output missing app name:\n%s", text)
	}
	if !strings.Contains(text, "0") {
		t.Errorf("table output missing zero runs:\n%s", text)
	}
}

func TestRunStatsTriggersUploadWhenEnabled(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})

	logDir := filepath.Join(dataHome, "zenget", "usage")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	logPath := filepath.Join(logDir, "widget.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"start":"2026-08-25T10:00:00Z","duration_s":2,"exit_code":0}
`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := config.Config{UsageTracking: true, UsageUpload: true}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	var received bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("ZENGET_USAGE_URL", server.URL)

	cmd := &cobra.Command{}
	if err := runStats(cmd, nil); err != nil {
		t.Fatalf("runStats: %v", err)
	}
	if !received {
		t.Error("stats did not trigger upload")
	}
}

func TestRoundDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0.00s"},
		{1500 * time.Millisecond, "1.50s"},
		{123456789 * time.Nanosecond, "0.12s"},
	}
	for _, tc := range cases {
		got := roundDuration(tc.d)
		if got != tc.want {
			t.Errorf("roundDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/config"
	"zenget/internal/state"
)

func TestRunFlushSilentWhenUploadDisabled(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cmd := &cobra.Command{}
	if err := runFlush(cmd, nil); err != nil {
		t.Fatalf("runFlush: %v", err)
	}
}

func TestRunFlushUploadsWhenEnabled(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	logDir := filepath.Join(dataHome, "zenget", "usage")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	logPath := filepath.Join(logDir, "widget.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"start":"2026-08-25T10:00:00Z","duration_s":2,"exit_code":0}
`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var received bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("ZENGET_USAGE_URL", server.URL)

	cmd := &cobra.Command{}
	if err := runFlush(cmd, nil); err != nil {
		t.Fatalf("runFlush: %v", err)
	}
	if !received {
		logPath := filepath.Join(dataHome, "zenget", "upload", "errors.log")
		data, _ := os.ReadFile(logPath)
		t.Errorf("collector did not receive upload; error log:\n%s", data)
	}
}

func TestRunFlushReturnsZeroOnUploadFailure(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	logDir := filepath.Join(dataHome, "zenget", "usage")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	logPath := filepath.Join(logDir, "widget.jsonl")
	if err := os.WriteFile(logPath, []byte(`{"start":"2026-08-25T10:00:00Z","duration_s":2,"exit_code":0}
`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("ZENGET_USAGE_URL", server.URL)

	cmd := &cobra.Command{}
	if err := runFlush(cmd, nil); err != nil {
		t.Fatalf("runFlush returned error on silent failure: %v", err)
	}
}

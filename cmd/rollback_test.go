package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zenget/internal/appcfg"
	"zenget/internal/hashx"
	"zenget/internal/install"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

type rollbackFixture struct {
	activePath   string
	previousPath string
	wrapperPath  string
	usagePath    string
	statePath    string
	configPath   string
	activeData   []byte
	previousData []byte
	wrapperData  []byte
	stateData    []byte
	usageData    []byte
	configData   []byte
}

func newRollbackFixture(t *testing.T) rollbackFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	activeData := []byte("version two")
	previousData := []byte("version one")
	if _, _, err := install.InstallVersioned(previousData, "github", "acme/tool", "v1.0.0", false, "tool"); err != nil {
		t.Fatalf("initial InstallVersioned() error = %v", err)
	}
	transaction, err := install.InstallUpgradeVersioned(activeData, "github", "acme/tool", "v2.0.0", "tool")
	if err != nil {
		t.Fatalf("InstallUpgradeVersioned() error = %v", err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("upgrade Commit() error = %v", err)
	}

	activePath, err := wrapper.VersionedRealPath("github", "acme/tool", "v2.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	previousPath, err := wrapper.VersionedRealPath("github", "acme/tool", "v1.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	wrapperPath, err := install.TargetPathFor("acme/tool", "tool")
	if err != nil {
		t.Fatal(err)
	}
	usagePath, err := wrapper.UsageLogPath("acme/tool")
	if err != nil {
		t.Fatal(err)
	}
	usageData := []byte(`{"start":"2026-09-01T10:00:00Z","duration_s":2,"exit_code":0}`)
	if err := os.MkdirAll(filepath.Dir(usagePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usagePath, usageData, 0644); err != nil {
		t.Fatal(err)
	}

	config := &appcfg.AppConfig{Asset: "tool", ArchiveBinary: "tool", TargetName: "tool"}
	if err := config.Save("acme/tool"); err != nil {
		t.Fatalf("save app config: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Set("acme/tool", state.App{
		Version:      "v2.0.0",
		InstalledAt:  time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		DownloadURL:  "https://example.test/tool-v2",
		InstallPath:  wrapperPath,
		RealPath:     activePath,
		XXH3_64:      hashx.Bytes(activeData),
		Pinned:       true,
		PlatformOS:   "linux",
		PlatformArch: "amd64",
		ActiveArtifact: &state.Artifact{
			Version:  "v2.0.0",
			RealPath: activePath,
			XXH3_64:  hashx.Bytes(activeData),
		},
		PreviousArtifact: &state.Artifact{
			Version:  "v1.0.0",
			RealPath: previousPath,
			XXH3_64:  hashx.Bytes(previousData),
		},
	})
	if err := st.Save(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	statePath, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	configPath, err := appcfg.Path("acme/tool")
	if err != nil {
		t.Fatal(err)
	}
	fixture := rollbackFixture{
		activePath:   activePath,
		previousPath: previousPath,
		wrapperPath:  wrapperPath,
		usagePath:    usagePath,
		statePath:    statePath,
		configPath:   configPath,
		activeData:   append([]byte(nil), activeData...),
		previousData: append([]byte(nil), previousData...),
		usageData:    append([]byte(nil), usageData...),
	}
	fixture.wrapperData = readRollbackFile(t, wrapperPath)
	fixture.stateData = readRollbackFile(t, statePath)
	fixture.configData = readRollbackFile(t, configPath)
	return fixture
}

func readRollbackFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return data
}

func TestRunRollbackSwitchesVersionsOfflineAndPreservesMetadata(t *testing.T) {
	fixture := newRollbackFixture(t)

	var output, errOutput bytes.Buffer
	if err := runRollbackWithWriters("acme/tool", &output, &errOutput); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got, want := output.String(), "Rolled back acme/tool v2.0.0 -> v1.0.0\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if errOutput.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOutput.String())
	}
	assertRollbackState(t, "v1.0.0", fixture.previousPath, "v2.0.0", fixture.activePath)
	if got := readRollbackFile(t, fixture.previousPath); !bytes.Equal(got, fixture.previousData) {
		t.Fatalf("rolled-back active artifact = %q, want %q", got, fixture.previousData)
	}
	if got := readRollbackFile(t, fixture.activePath); !bytes.Equal(got, fixture.activeData) {
		t.Fatalf("retained previous artifact = %q, want %q", got, fixture.activeData)
	}
	wrapperData := readRollbackFile(t, fixture.wrapperPath)
	if !bytes.Contains(wrapperData, []byte(fixture.previousPath)) || bytes.Contains(wrapperData, []byte(fixture.activePath)) {
		t.Fatalf("wrapper does not target rolled-back artifact: %q", wrapperData)
	}
	if got := readRollbackFile(t, fixture.usagePath); !bytes.Equal(got, fixture.usageData) {
		t.Fatalf("usage data changed: %q", got)
	}
	if got := readRollbackFile(t, fixture.configPath); !bytes.Equal(got, fixture.configData) {
		t.Fatalf("app config changed: %q", got)
	}

	listState, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	var listOutput bytes.Buffer
	if err := printList(listState, nil, &listOutput, false, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(listOutput.String(), "acme/tool") || !strings.Contains(listOutput.String(), "v1.0.0") {
		t.Fatalf("list output = %q, want rolled-back version", listOutput.String())
	}

	// A second rollback deterministically returns to v2 without creating a
	// third artifact or changing the usage/configuration files.
	output.Reset()
	if err := runRollbackWithWriters("acme/tool", &output, io.Discard); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if got, want := output.String(), "Rolled back acme/tool v1.0.0 -> v2.0.0\n"; got != want {
		t.Fatalf("second output = %q, want %q", got, want)
	}
	assertRollbackState(t, "v2.0.0", fixture.activePath, "v1.0.0", fixture.previousPath)
	if got := readRollbackFile(t, fixture.usagePath); !bytes.Equal(got, fixture.usageData) {
		t.Fatalf("usage data changed after second rollback: %q", got)
	}
	if got := readRollbackFile(t, fixture.configPath); !bytes.Equal(got, fixture.configData) {
		t.Fatalf("app config changed after second rollback: %q", got)
	}
}

func assertRollbackState(t *testing.T, version, activePath, previousVersion, previousPath string) {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, ok := st.Get("acme/tool")
	if !ok {
		t.Fatal("state does not contain acme/tool")
	}
	if app.Version != version || app.RealPath != activePath || app.ActiveArtifact == nil || app.ActiveArtifact.Version != version || app.ActiveArtifact.RealPath != activePath || app.PreviousArtifact == nil || app.PreviousArtifact.Version != previousVersion || app.PreviousArtifact.RealPath != previousPath || !app.Pinned || app.PlatformOS != "linux" || app.PlatformArch != "amd64" {
		t.Fatalf("rollback state = %#v", app)
	}
}

func TestRunRollbackRejectsInvalidFixturesWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, fixture rollbackFixture)
		want   string
	}{
		{
			name: "missing previous artifact",
			mutate: func(t *testing.T, fixture rollbackFixture) {
				t.Helper()
				if err := os.Remove(fixture.previousPath); err != nil {
					t.Fatal(err)
				}
			},
			want: "previous artifact",
		},
		{
			name: "active hash mismatch",
			mutate: func(t *testing.T, fixture rollbackFixture) {
				t.Helper()
				if err := os.WriteFile(fixture.activePath, []byte("tampered"), 0755); err != nil {
					t.Fatal(err)
				}
			},
			want: "hash mismatch",
		},
		{
			name: "missing wrapper",
			mutate: func(t *testing.T, fixture rollbackFixture) {
				t.Helper()
				if err := os.Remove(fixture.wrapperPath); err != nil {
					t.Fatal(err)
				}
			},
			want: "wrapper",
		},
		{
			name: "foreign wrapper",
			mutate: func(t *testing.T, fixture rollbackFixture) {
				t.Helper()
				if err := os.WriteFile(fixture.wrapperPath, []byte("foreign"), 0755); err != nil {
					t.Fatal(err)
				}
			},
			want: "foreign",
		},
		{
			name: "wrapper targets wrong artifact",
			mutate: func(t *testing.T, fixture rollbackFixture) {
				t.Helper()
				data := wrapper.Generate("acme/tool", "/wrong/artifact", fixture.usagePath)
				if err := os.WriteFile(fixture.wrapperPath, data, 0755); err != nil {
					t.Fatal(err)
				}
			},
			want: "does not target active artifact",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRollbackFixture(t)
			test.mutate(t, fixture)
			originalState := readRollbackFile(t, fixture.statePath)
			originalActive := readRollbackFile(t, fixture.activePath)
			var output bytes.Buffer
			err := runRollbackWithWriters("acme/tool", &output, io.Discard)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("rollback error = %v, want %q", err, test.want)
			}
			if got := readRollbackFile(t, fixture.statePath); !bytes.Equal(got, originalState) {
				t.Fatalf("state changed after rejected rollback: %q", got)
			}
			if got := readRollbackFile(t, fixture.activePath); !bytes.Equal(got, originalActive) {
				t.Fatalf("active artifact changed after rejected rollback: %q", got)
			}
		})
	}
}

func TestRunRollbackRejectsLegacyState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Set("acme/tool", state.App{Version: "v1.0.0", InstallPath: filepath.Join(home, ".local", "bin", "tool")})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	err = runRollbackWithWriters("acme/tool", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "active and previous artifact") {
		t.Fatalf("rollback error = %v, want legacy-state error", err)
	}
}

func TestRunRollbackRestoresWrapperWhenStateSaveFails(t *testing.T) {
	fixture := newRollbackFixture(t)
	originalPersistState := persistState
	t.Cleanup(func() { persistState = originalPersistState })
	persistState = func(*state.State) error {
		return errors.New("injected state save failure")
	}

	err := runRollbackWithWriters("acme/tool", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "save state") {
		t.Fatalf("rollback error = %v, want state-save failure", err)
	}
	if got := readRollbackFile(t, fixture.wrapperPath); !bytes.Equal(got, fixture.wrapperData) {
		t.Fatalf("wrapper after failed rollback = %q, want original", got)
	}
	if got := readRollbackFile(t, fixture.statePath); !bytes.Equal(got, fixture.stateData) {
		t.Fatalf("state after failed rollback = %q, want original", got)
	}
	if got := readRollbackFile(t, fixture.activePath); !bytes.Equal(got, fixture.activeData) {
		t.Fatalf("active artifact after failed rollback = %q, want original", got)
	}
	if got := readRollbackFile(t, fixture.previousPath); !bytes.Equal(got, fixture.previousData) {
		t.Fatalf("previous artifact after failed rollback = %q, want original", got)
	}
}

package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/hashx"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

// installFixture registers repository in the state store with a binary on
// disk and returns the binary path.
func installFixture(t *testing.T, repository, repo string, binary []byte) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	target := filepath.Join(home, ".local", "bin", repo)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, binary, 0755); err != nil {
		t.Fatal(err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Set(repository, state.App{
		Version:     "v1.0.0",
		InstalledAt: time.Now().UTC(),
		InstallPath: target,
		XXH3_64:     hashx.Bytes(binary),
	})
	if err := st.Save(); err != nil {
		t.Fatalf("save state: %v", err)
	}
	return target
}

func TestRunUninstallRemovesBinaryAndStateEntry(t *testing.T) {
	target := installFixture(t, "acme/widget", "widget", []byte("widget binary"))

	var output, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	cmd.SetErr(&errOut)
	if err := runUninstall(cmd, []string{"acme/widget"}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got, want := output.String(), "Removed acme/widget ("+target+")\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("binary still present, stat err = %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, ok := st.Get("acme/widget"); ok {
		t.Fatal("state still contains acme/widget")
	}
}

func TestRunUninstallResolvesUniqueShortName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	realPath := filepath.Join(home, ".local", "share", "zenget", "bin", "widget")
	wrapperPath := filepath.Join(home, ".local", "bin", "widget")
	for path, contents := range map[string][]byte{
		realPath:    []byte("widget binary"),
		wrapperPath: []byte("#!/bin/sh\n# zenget-managed wrapper\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Set("acme/widget", state.App{
		Version:     "v1.0.0",
		InstalledAt: time.Now().UTC(),
		InstallPath: wrapperPath,
		RealPath:    realPath,
		XXH3_64:     hashx.Bytes([]byte("widget binary")),
	})
	if err := st.Save(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	if err := runUninstall(cmd, []string{"widget"}); err != nil {
		t.Fatalf("uninstall short name: %v", err)
	}
	if got, want := output.String(), "Removed acme/widget ("+realPath+")\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	for _, path := range []string{realPath, wrapperPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("installed path %q still present, stat err = %v", path, err)
		}
	}
}

func TestRunUninstallRejectsAmbiguousShortNameWithoutDeleting(t *testing.T) {
	target := installFixture(t, "acme/widget", "widget", []byte("widget binary"))

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Set("other/widget", st.Apps["acme/widget"])
	if err := st.Save(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err = runUninstall(cmd, []string{"widget"})
	if err == nil {
		t.Fatal("uninstall succeeded, want ambiguity error")
	}
	for _, want := range []string{"acme/widget", "other/widget"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want candidate %q", err, want)
		}
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("binary changed after ambiguous uninstall, stat err = %v", statErr)
	}
	st, err = state.Load()
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if len(st.Apps) != 2 {
		t.Fatalf("state entries = %d, want 2", len(st.Apps))
	}
}

func TestRunUninstallUnknownShortName(t *testing.T) {
	target := installFixture(t, "acme/widget", "widget", []byte("widget binary"))

	err := runUninstall(&cobra.Command{}, []string{"Widget"})
	if err == nil || !strings.Contains(err.Error(), "Widget is not installed") {
		t.Fatalf("error = %v, want not installed", err)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("binary changed after case-mismatched uninstall, stat err = %v", statErr)
	}
}

func TestRunUninstallRefusesModifiedBinary(t *testing.T) {
	target := installFixture(t, "acme/widget", "widget", []byte("widget binary"))
	if err := os.WriteFile(target, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}

	err := runUninstallWithWriters("acme/widget", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("error = %v, want hash mismatch", err)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("binary must survive hash mismatch, stat err = %v", statErr)
	}

	st, loadErr := state.Load()
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if _, ok := st.Get("acme/widget"); !ok {
		t.Fatal("state entry must survive hash mismatch")
	}
}

func TestRunUninstallMissingBinaryRemovesStateEntry(t *testing.T) {
	target := installFixture(t, "acme/widget", "widget", []byte("widget binary"))
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	var output, errOut bytes.Buffer
	if err := runUninstallWithWriters("acme/widget", &output, &errOut); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got, want := errOut.String(), "warning: "+target+" not found on disk, removing registry entry\n"; got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, ok := st.Get("acme/widget"); ok {
		t.Fatal("stale state entry was not removed")
	}
}

func TestRunUninstallNotInstalled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	err := runUninstallWithWriters("acme/widget", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "acme/widget is not installed") {
		t.Fatalf("error = %v, want not installed", err)
	}
}

func TestRunUninstallRemovesBinaryFromConfiguredInstallDir(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	target := filepath.Join(home, "custom", "widget")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	binary := []byte("widget binary")
	if err := os.WriteFile(target, binary, 0755); err != nil {
		t.Fatal(err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	st.Set("acme/widget", state.App{
		Version:     "v1.0.0",
		InstalledAt: time.Now().UTC(),
		InstallPath: target,
		XXH3_64:     hashx.Bytes(binary),
	})
	if err := st.Save(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	var output, errOut bytes.Buffer
	if err := runUninstallWithWriters("acme/widget", &output, &errOut); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got, want := output.String(), "Removed acme/widget ("+target+")\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("wrapper still present, stat err = %v", err)
	}
}

func TestRunUninstallPreflightsAllVersionedArtifactsBeforeDeleting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	active := []byte("active binary")
	previous := []byte("previous binary")
	activePath, err := wrapper.VersionedRealPath("github", "acme/tool", "v2.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	previousPath, err := wrapper.VersionedRealPath("github", "acme/tool", "v1.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	wrapperPath := filepath.Join(home, ".local", "bin", "tool")
	for path, contents := range map[string][]byte{
		activePath:   active,
		previousPath: previous,
		wrapperPath:  []byte("#!/bin/sh\n# zenget-managed wrapper\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Set("acme/tool", state.App{
		Version:     "v2.0.0",
		InstalledAt: time.Now().UTC(),
		InstallPath: wrapperPath,
		RealPath:    activePath,
		XXH3_64:     hashx.Bytes(active),
		ActiveArtifact: &state.Artifact{
			Version:  "v2.0.0",
			RealPath: activePath,
			XXH3_64:  hashx.Bytes(active),
		},
		PreviousArtifact: &state.Artifact{
			Version:  "v1.0.0",
			RealPath: previousPath,
			XXH3_64:  hashx.Bytes(previous),
		},
	})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(previousPath, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	err = runUninstallWithWriters("acme/tool", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("uninstall error = %v, want previous-artifact hash mismatch", err)
	}
	for _, path := range []string{activePath, previousPath, wrapperPath} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("path %q changed after preflight failure: %v", path, statErr)
		}
	}
	st, err = state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("acme/tool"); !ok {
		t.Fatal("state entry removed after preflight failure")
	}
}

func TestRunUninstallRefusesForeignVersionedWrapperWithoutDeletingArtifacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	binary := []byte("active binary")
	activePath, err := wrapper.VersionedRealPath("github", "acme/tool", "v2.0.0", "tool")
	if err != nil {
		t.Fatal(err)
	}
	wrapperPath := filepath.Join(home, ".local", "bin", "tool")
	if err := os.MkdirAll(filepath.Dir(activePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activePath, binary, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(wrapperPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapperPath, []byte("foreign executable"), 0755); err != nil {
		t.Fatal(err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Set("acme/tool", state.App{
		Version:     "v2.0.0",
		InstalledAt: time.Now().UTC(),
		InstallPath: wrapperPath,
		RealPath:    activePath,
		XXH3_64:     hashx.Bytes(binary),
		ActiveArtifact: &state.Artifact{
			Version:  "v2.0.0",
			RealPath: activePath,
			XXH3_64:  hashx.Bytes(binary),
		},
	})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	err = runUninstallWithWriters("acme/tool", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "foreign file") {
		t.Fatalf("uninstall error = %v, want foreign-wrapper error", err)
	}
	for _, path := range []string{activePath, wrapperPath} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("path %q changed after foreign-wrapper failure: %v", path, statErr)
		}
	}
}

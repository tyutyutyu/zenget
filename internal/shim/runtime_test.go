package shim

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/wrapper"
)

func TestEnsureCurrentCopiesExecutableAndReusesMatchingRuntime(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	source := filepath.Join(t.TempDir(), "zenget")
	if err := os.WriteFile(source, []byte("runtime-one"), 0755); err != nil {
		t.Fatal(err)
	}
	originalExecutable := currentExecutable
	currentExecutable = func() (string, error) { return source, nil }
	t.Cleanup(func() { currentExecutable = originalExecutable })

	path, err := EnsureCurrent()
	if err != nil {
		t.Fatalf("EnsureCurrent() error = %v", err)
	}
	wantPath, err := wrapper.ShimRuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	if path != wantPath {
		t.Fatalf("EnsureCurrent() path = %q, want %q", path, wantPath)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "runtime-one" {
		t.Fatalf("runtime contents = %q, want runtime-one", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("runtime permissions = %o, want 755", info.Mode().Perm())
	}

	if _, err := EnsureCurrent(); err != nil {
		t.Fatalf("EnsureCurrent() reuse error = %v", err)
	}
}

func TestEnsureCurrentReportsExecutableLookupAndSourceErrors(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	originalExecutable := currentExecutable
	t.Cleanup(func() { currentExecutable = originalExecutable })
	currentExecutable = func() (string, error) { return "", errors.New("executable unavailable") }
	if _, err := EnsureCurrent(); err == nil || !strings.Contains(err.Error(), "resolve current zenget executable") {
		t.Fatalf("EnsureCurrent() error = %v, want executable lookup error", err)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	currentExecutable = func() (string, error) { return missing, nil }
	if _, err := EnsureCurrent(); err == nil || !strings.Contains(err.Error(), "inspect current zenget executable") {
		t.Fatalf("EnsureCurrent() error = %v, want missing source error", err)
	}

	directory := filepath.Join(t.TempDir(), "source-directory")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	currentExecutable = func() (string, error) { return directory, nil }
	if _, err := EnsureCurrent(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("EnsureCurrent() error = %v, want non-regular source error", err)
	}
}

func TestEnsureCurrentRejectsSymlinkSourceAndDestination(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("target"), 0755); err != nil {
		t.Fatal(err)
	}
	sourceLink := filepath.Join(t.TempDir(), "source-link")
	if err := os.Symlink(target, sourceLink); err != nil {
		t.Fatal(err)
	}
	originalExecutable := currentExecutable
	currentExecutable = func() (string, error) { return sourceLink, nil }
	t.Cleanup(func() { currentExecutable = originalExecutable })
	if _, err := EnsureCurrent(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("EnsureCurrent() error = %v, want symlink rejection", err)
	}

	validSource := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(validSource, []byte("source"), 0755); err != nil {
		t.Fatal(err)
	}
	currentExecutable = func() (string, error) { return validSource, nil }
	runtimePath, err := RuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0755); err != nil {
		t.Fatal(err)
	}
	destinationTarget := filepath.Join(t.TempDir(), "destination")
	if err := os.WriteFile(destinationTarget, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(destinationTarget, runtimePath); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCurrent(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("EnsureCurrent() destination error = %v, want symlink rejection", err)
	}
}

func TestEnsureCurrentRenameFailurePreservesPreviousRuntime(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("new-runtime"), 0755); err != nil {
		t.Fatal(err)
	}
	originalExecutable := currentExecutable
	currentExecutable = func() (string, error) { return source, nil }
	t.Cleanup(func() { currentExecutable = originalExecutable })
	runtimePath, err := RuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath, []byte("old-runtime"), 0755); err != nil {
		t.Fatal(err)
	}
	originalRename := runtimeRename
	runtimeRename = func(_, _ string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { runtimeRename = originalRename })

	if _, err := EnsureCurrent(); err == nil || !strings.Contains(err.Error(), "replace shim runtime") {
		t.Fatalf("EnsureCurrent() error = %v, want replacement error", err)
	}
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old-runtime" {
		t.Fatalf("previous runtime = %q, want old-runtime", data)
	}
}

func TestEnsureCurrentRejectsNonRegularDestination(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("source"), 0755); err != nil {
		t.Fatal(err)
	}
	originalExecutable := currentExecutable
	currentExecutable = func() (string, error) { return source, nil }
	t.Cleanup(func() { currentExecutable = originalExecutable })
	runtimePath, err := RuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(runtimePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimePath, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCurrent(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("EnsureCurrent() error = %v, want non-regular destination error", err)
	}
}

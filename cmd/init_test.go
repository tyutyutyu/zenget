package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/manifest"
)

func TestRunInitCreatesAndRecreatesDeterministicManifest(t *testing.T) {
	directory := t.TempDir()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(workingDirectory) })

	previousForce := initForce
	initForce = false
	t.Cleanup(func() { initForce = previousForce })

	command, output, _ := newBufferedCommand()
	if err := runInit(command, nil); err != nil {
		t.Fatalf("initial init: %v", err)
	}
	first, err := os.ReadFile(defaultManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(defaultManifestFile)
	if err != nil {
		t.Fatalf("load initialized manifest: %v", err)
	}
	if loaded.SchemaVersion != manifest.SchemaVersion || loaded.SchemaURL != manifest.SchemaURL || loaded.Apps == nil || len(loaded.Apps) != 0 {
		t.Fatalf("initialized manifest = %#v", loaded)
	}
	if got, want := output.String(), "Initialized manifest zenget.json\n"; got != want {
		t.Fatalf("init output = %q, want %q", got, want)
	}

	secondCommand, _, _ := newBufferedCommand()
	if err := runInit(secondCommand, nil); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second init error = %v, want no-clobber error", err)
	}
	unchanged, err := os.ReadFile(defaultManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, unchanged) {
		t.Fatalf("manifest changed after refused init: %s", unchanged)
	}

	initForce = true
	forcedCommand, _, _ := newBufferedCommand()
	if err := runInit(forcedCommand, nil); err != nil {
		t.Fatalf("forced init: %v", err)
	}
	forced, err := os.ReadFile(defaultManifestFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, forced) {
		t.Fatalf("forced init is not deterministic: %s", forced)
	}
}

func TestRunInitHonorsOutputAndRejectsSymlinkOrRemotePaths(t *testing.T) {
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "portable.json")
	command, output, _ := newBufferedCommand()
	command.Flags().String("output", outputPath, "")

	previousForce := initForce
	initForce = false
	t.Cleanup(func() { initForce = previousForce })
	if err := runInit(command, nil); err != nil {
		t.Fatalf("custom output init: %v", err)
	}
	if _, err := manifest.Load(outputPath); err != nil {
		t.Fatalf("load custom output: %v", err)
	}
	if !strings.Contains(output.String(), outputPath) {
		t.Fatalf("custom init output = %q", output)
	}

	remoteCommand, _, _ := newBufferedCommand()
	remoteCommand.Flags().String("output", "https://example.test/manifest.json", "")
	if err := runInit(remoteCommand, nil); err == nil || !strings.Contains(err.Error(), "local path") {
		t.Fatalf("remote output error = %v, want local-path error", err)
	}

	targetPath := filepath.Join(directory, "target.json")
	if err := os.WriteFile(targetPath, []byte("keep\n"), 0644); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(directory, "link.json")
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	symlinkCommand, _, _ := newBufferedCommand()
	symlinkCommand.Flags().String("output", symlinkPath, "")
	initForce = true
	if err := runInit(symlinkCommand, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink output error = %v, want symlink error", err)
	}
	if got, err := os.ReadFile(targetPath); err != nil || string(got) != "keep\n" {
		t.Fatalf("symlink target = %q, err = %v", got, err)
	}
}

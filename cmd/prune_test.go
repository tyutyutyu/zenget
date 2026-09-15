package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/state"
)

func setupPruneEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

func savePruneFixture(t *testing.T, apps map[string]state.App) string {
	t.Helper()
	if err := (&state.State{Apps: apps}).Save(); err != nil {
		t.Fatalf("save prune fixture: %v", err)
	}
	path, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func configurePruneTest(t *testing.T, force, dryRun, interactive bool, input string) {
	t.Helper()
	previousForce, previousDryRun := pruneForce, pruneDryRun
	previousInput, previousTerminal := prunePromptReader, pruneIsTerminal
	pruneForce = force
	pruneDryRun = dryRun
	prunePromptReader = strings.NewReader(input)
	pruneIsTerminal = func() bool { return interactive }
	t.Cleanup(func() {
		pruneForce, pruneDryRun = previousForce, previousDryRun
		prunePromptReader, pruneIsTerminal = previousInput, previousTerminal
	})
}

func TestRunPruneRemovesOnlyConfirmedOrphans(t *testing.T) {
	setupPruneEnvironment(t)
	directory := t.TempDir()
	healthyWrapper := filepath.Join(directory, "healthy-wrapper")
	healthyReal := filepath.Join(directory, "healthy-real")
	partialWrapper := filepath.Join(directory, "partial-wrapper")
	for _, path := range []string{healthyWrapper, healthyReal, partialWrapper} {
		if err := os.WriteFile(path, []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	statePath := savePruneFixture(t, map[string]state.App{
		"acme/healthy": {InstallPath: healthyWrapper, RealPath: healthyReal},
		"acme/orphan":  {InstallPath: filepath.Join(directory, "orphan-wrapper"), RealPath: filepath.Join(directory, "orphan-real")},
		"acme/partial": {InstallPath: partialWrapper, RealPath: filepath.Join(directory, "partial-real")},
	})
	configurePruneTest(t, false, false, true, "yes\n")
	cmd, output, _ := newBufferedCommand()
	if err := runPrune(cmd, nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("acme/orphan"); ok {
		t.Fatal("confirmed orphan remains in registry")
	}
	for _, repository := range []string{"acme/healthy", "acme/partial"} {
		if _, ok := st.Get(repository); !ok {
			t.Fatalf("registry entry %s was removed", repository)
		}
	}
	for _, path := range []string{healthyWrapper, healthyReal, partialWrapper} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("prune removed or changed file %q: %v", path, err)
		}
	}
	text := output.String()
	for _, want := range []string{
		"Orphaned registry entry: acme/orphan",
		"Warning: acme/partial is partially missing (real binary)",
		"zenget ensure acme/partial",
		"Remove 1 orphaned registry entry? [y/N]",
		"Removed 1 orphaned registry entry.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prune output missing %q:\n%s", want, text)
		}
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file missing after prune: %v", err)
	}
}

func TestRunPruneForceSkipsPromptAndDryRunDoesNotSave(t *testing.T) {
	setupPruneEnvironment(t)
	directory := t.TempDir()
	orphan := state.App{InstallPath: filepath.Join(directory, "wrapper"), RealPath: filepath.Join(directory, "real")}
	statePath := savePruneFixture(t, map[string]state.App{"acme/orphan": orphan})
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	configurePruneTest(t, false, true, false, "yes\n")
	cmd, output, _ := newBufferedCommand()
	if err := runPrune(cmd, nil); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("dry-run changed state:\n%s\n---\n%s", before, after)
	}
	if !strings.Contains(output.String(), "Dry run: would remove 1 orphaned registry entry.") {
		t.Fatalf("dry-run output = %q", output)
	}

	configurePruneTest(t, true, false, false, "not read\n")
	cmd, output, _ = newBufferedCommand()
	if err := runPrune(cmd, nil); err != nil {
		t.Fatalf("force prune: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("acme/orphan"); ok {
		t.Fatal("force prune left orphan in registry")
	}
	if strings.Contains(output.String(), "[y/N]") {
		t.Fatalf("force prune prompted: %q", output)
	}
}

func TestRunPruneNonInteractiveAndCancelledLeaveStateUnchanged(t *testing.T) {
	setupPruneEnvironment(t)
	directory := t.TempDir()
	statePath := savePruneFixture(t, map[string]state.App{
		"acme/orphan": {InstallPath: filepath.Join(directory, "wrapper"), RealPath: filepath.Join(directory, "real")},
	})
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	configurePruneTest(t, false, false, false, "yes\n")
	cmd, output, _ := newBufferedCommand()
	err = runPrune(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "non-interactive") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("non-interactive error = %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("non-interactive prune changed state")
	}
	if !strings.Contains(output.String(), "Orphaned registry entry: acme/orphan") {
		t.Fatalf("non-interactive output = %q", output)
	}

	configurePruneTest(t, false, false, true, "n\n")
	cmd, output, _ = newBufferedCommand()
	if err := runPrune(cmd, nil); err != nil {
		t.Fatalf("cancelled prune: %v", err)
	}
	if !strings.Contains(output.String(), "Prune cancelled; registry unchanged.") {
		t.Fatalf("cancelled output = %q", output)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("acme/orphan"); !ok {
		t.Fatal("cancelled prune removed orphan")
	}
}

func TestRunPruneReportsEmptyCleanAndInspectionErrorRegistries(t *testing.T) {
	setupPruneEnvironment(t)
	savePruneFixture(t, map[string]state.App{})
	configurePruneTest(t, false, false, false, "")
	cmd, output, _ := newBufferedCommand()
	if err := runPrune(cmd, nil); err != nil {
		t.Fatalf("empty prune: %v", err)
	}
	if got := output.String(); got != "No applications in registry; nothing to prune.\n" {
		t.Fatalf("empty output = %q", got)
	}

	invalidPath := "\x00invalid"
	savePruneFixture(t, map[string]state.App{
		"acme/clean":   {InstallPath: invalidPath, RealPath: invalidPath},
		"acme/healthy": {InstallPath: "/dev/null", RealPath: "/dev/null"},
	})
	cmd, output, _ = newBufferedCommand()
	if err := runPrune(cmd, nil); err != nil {
		t.Fatalf("clean prune: %v", err)
	}
	text := output.String()
	if !strings.Contains(text, "Warning: cannot inspect acme/clean") || !strings.Contains(text, "No orphaned registry entries found.") {
		t.Fatalf("clean/inspection output = %q", text)
	}
}

func TestPruneCommandRejectsArguments(t *testing.T) {
	if err := pruneCmd.Args(pruneCmd, []string{"unexpected"}); err == nil {
		t.Fatal("prune accepted an argument")
	}
}

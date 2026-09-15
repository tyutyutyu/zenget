package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/config"
	"zenget/internal/hashx"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

func TestDoctorReportsHealthyModernAndLegacyAppsDeterministically(t *testing.T) {
	doctorFixtureEnvironment(t)

	modern := doctorModernApp(t, []byte("modern binary"))
	legacy := doctorLegacyApp(t, []byte("legacy binary"))
	writeDoctorState(t, map[string]state.App{
		"zeta/tool":   modern,
		"acme/legacy": legacy,
	})

	oldJSON := doctorJSON
	t.Cleanup(func() { doctorJSON = oldJSON })
	doctorJSON = false

	var first bytes.Buffer
	if err := runDoctor(doctorCommand(&first), nil); err != nil {
		t.Fatalf("doctor human report: %v", err)
	}
	var second bytes.Buffer
	if err := runDoctor(doctorCommand(&second), nil); err != nil {
		t.Fatalf("doctor human report (second run): %v", err)
	}
	if first.String() != second.String() {
		t.Fatalf("human report is not stable:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
	for _, want := range []string{"CONFIG: HEALTHY", "STATE: HEALTHY", "NAME\tVERSION\tSTATUS\tDETAILS", "HEALTHY", "Summary: 2 healthy, 0 unhealthy"} {
		if !strings.Contains(first.String(), want) {
			t.Errorf("human report missing %q:\n%s", want, first.String())
		}
	}
	if got, want := strings.Index(first.String(), "acme/legacy"), strings.Index(first.String(), "zeta/tool"); got < 0 || want < 0 || got > want {
		t.Fatalf("human applications are not sorted: %q", first.String())
	}

	doctorJSON = true
	var jsonFirst bytes.Buffer
	if err := runDoctor(doctorCommand(&jsonFirst), nil); err != nil {
		t.Fatalf("doctor JSON report: %v", err)
	}
	var jsonSecond bytes.Buffer
	if err := runDoctor(doctorCommand(&jsonSecond), nil); err != nil {
		t.Fatalf("doctor JSON report (second run): %v", err)
	}
	if jsonFirst.String() != jsonSecond.String() {
		t.Fatalf("JSON report is not stable:\nfirst:\n%s\nsecond:\n%s", jsonFirst.String(), jsonSecond.String())
	}
	if strings.Contains(jsonFirst.String(), "\033[") {
		t.Fatalf("JSON report contains ANSI escape codes: %q", jsonFirst.String())
	}

	var report doctorReport
	if err := json.Unmarshal(jsonFirst.Bytes(), &report); err != nil {
		t.Fatalf("parse doctor JSON: %v\n%s", err, jsonFirst.String())
	}
	if report.SchemaVersion != doctorSchemaVersion || !report.Healthy {
		t.Fatalf("report metadata = %#v, want schema %d and healthy", report, doctorSchemaVersion)
	}
	if !report.Config.Healthy || !report.State.Healthy || report.Summary != (doctorSummary{Healthy: 2, Unhealthy: 0}) {
		t.Fatalf("global/summary report = %#v", report)
	}
	if got, want := len(report.Apps), 2; got != want {
		t.Fatalf("app count = %d, want %d", got, want)
	}
	if report.Apps[0].Name != "acme/legacy" || report.Apps[1].Name != "zeta/tool" {
		t.Fatalf("JSON app order = %q, %q", report.Apps[0].Name, report.Apps[1].Name)
	}
	if report.Apps[0].RealPath != "" || report.Apps[0].BinaryPath != report.Apps[0].InstallPath {
		t.Fatalf("legacy paths = %#v", report.Apps[0])
	}
	for _, app := range report.Apps {
		if !app.Healthy || len(app.Problems) != 0 {
			t.Errorf("healthy app report = %#v", app)
		}
	}
}

func TestDoctorReportsRequestedRepositoriesOnlyAndUnknownIsNotInstalled(t *testing.T) {
	doctorFixtureEnvironment(t)
	writeDoctorState(t, map[string]state.App{
		"acme/other": doctorLegacyApp(t, []byte("not selected")),
		"zeta/tool":  doctorLegacyApp(t, []byte("zeta")),
		"acme/tool":  doctorLegacyApp(t, []byte("acme")),
	})

	oldJSON := doctorJSON
	t.Cleanup(func() { doctorJSON = oldJSON })
	doctorJSON = true
	var output bytes.Buffer
	err := runDoctor(doctorCommand(&output), []string{"zeta/tool", "missing/repo", "acme/tool", "acme/tool"})
	if err == nil {
		t.Fatal("doctor returned nil for an unknown requested repository")
	}
	if got, want := commandExitCode(err), 1; got != want {
		t.Fatalf("doctor exit code = %d, want %d (error %v)", got, want, err)
	}
	var report doctorReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("parse failure JSON report: %v\n%s", decodeErr, output.String())
	}
	if report.Healthy || report.Summary != (doctorSummary{Healthy: 2, Unhealthy: 1}) {
		t.Fatalf("requested report metadata = %#v", report)
	}
	if got, want := len(report.Apps), 3; got != want {
		t.Fatalf("requested app count = %d, want %d", got, want)
	}
	if got := []string{report.Apps[0].Name, report.Apps[1].Name, report.Apps[2].Name}; !equalStrings(got, []string{"acme/tool", "missing/repo", "zeta/tool"}) {
		t.Fatalf("requested app order = %q", got)
	}
	if !hasDoctorProblem(report.Apps[1].Problems, doctorProblemNotInstalled) {
		t.Fatalf("unknown repository problems = %#v", report.Apps[1].Problems)
	}
	if strings.Contains(output.String(), "acme/other") {
		t.Fatalf("unrequested application was included:\n%s", output.String())
	}
}

func TestDoctorRejectsMalformedRepositoryArguments(t *testing.T) {
	for _, repository := range []string{"badrepo", "org/", "/repo", "org/repo/extra", "org/repo with space"} {
		t.Run(repository, func(t *testing.T) {
			err := validateDoctorArgs(nil, []string{repository})
			if err == nil || !strings.Contains(err.Error(), "expected org/repo") {
				t.Fatalf("validateDoctorArgs(%q) = %v, want usage error", repository, err)
			}
		})
	}
}

func TestDoctorDistinguishesIntegrityAndStateProblemCodes(t *testing.T) {
	doctorFixtureEnvironment(t)
	apps := make(map[string]state.App)
	apps["invalid-key"] = doctorLegacyApp(t, []byte("invalid key"))
	apps["acme/invalid-state"] = state.App{}

	missingBinary := doctorModernApp(t, []byte("missing binary"))
	missingBinary.RealPath = filepath.Join(t.TempDir(), "missing")
	apps["acme/missing-binary"] = missingBinary

	nonRegular := doctorModernApp(t, []byte("non regular"))
	if err := os.Remove(nonRegular.RealPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nonRegular.RealPath, 0755); err != nil {
		t.Fatal(err)
	}
	apps["acme/non-regular"] = nonRegular

	symlinkBinary := doctorModernApp(t, []byte("symlink target"))
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("symlink target"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(symlinkBinary.RealPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, symlinkBinary.RealPath); err != nil {
		t.Fatal(err)
	}
	apps["acme/symlink-binary"] = symlinkBinary

	notExecutable := doctorModernApp(t, []byte("not executable"))
	if err := os.Chmod(notExecutable.RealPath, 0644); err != nil {
		t.Fatal(err)
	}
	apps["acme/not-executable"] = notExecutable

	hashMismatch := doctorModernApp(t, []byte("hash mismatch"))
	hashMismatch.XXH3_64 = hashx.Bytes([]byte("different contents"))
	apps["acme/hash-mismatch"] = hashMismatch

	missingWrapper := doctorModernApp(t, []byte("missing wrapper"))
	if err := os.Remove(missingWrapper.InstallPath); err != nil {
		t.Fatal(err)
	}
	apps["acme/missing-wrapper"] = missingWrapper

	nonRegularWrapper := doctorModernApp(t, []byte("non regular wrapper"))
	if err := os.RemoveAll(nonRegularWrapper.InstallPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nonRegularWrapper.InstallPath, 0755); err != nil {
		t.Fatal(err)
	}
	apps["acme/non-regular-wrapper"] = nonRegularWrapper

	symlinkWrapper := doctorModernApp(t, []byte("symlink wrapper"))
	wrapperTarget := filepath.Join(t.TempDir(), "wrapper-target")
	if err := os.WriteFile(wrapperTarget, []byte("#!/bin/sh\n# "+wrapper.Marker+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(symlinkWrapper.InstallPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wrapperTarget, symlinkWrapper.InstallPath); err != nil {
		t.Fatal(err)
	}
	apps["acme/symlink-wrapper"] = symlinkWrapper

	notExecutableWrapper := doctorModernApp(t, []byte("wrapper not executable"))
	if err := os.Chmod(notExecutableWrapper.InstallPath, 0644); err != nil {
		t.Fatal(err)
	}
	apps["acme/not-executable-wrapper"] = notExecutableWrapper

	foreignWrapper := doctorModernApp(t, []byte("foreign wrapper"))
	if err := os.WriteFile(foreignWrapper.InstallPath, []byte("#!/bin/sh\necho foreign\n"), 0755); err != nil {
		t.Fatal(err)
	}
	apps["acme/foreign-wrapper"] = foreignWrapper

	ioBinary := doctorModernApp(t, []byte("unreadable binary"))
	if err := os.Chmod(ioBinary.RealPath, 0111); err != nil {
		t.Fatal(err)
	}
	apps["acme/io-error"] = ioBinary

	writeDoctorState(t, apps)

	oldJSON := doctorJSON
	t.Cleanup(func() { doctorJSON = oldJSON })
	doctorJSON = true
	var output bytes.Buffer
	err := runDoctor(doctorCommand(&output), nil)
	if err == nil {
		t.Fatal("doctor returned nil for unhealthy applications")
	}
	if got := commandExitCode(err); got != 1 {
		t.Fatalf("doctor exit code = %d, want 1", got)
	}
	var report doctorReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("parse doctor JSON: %v\n%s", decodeErr, output.String())
	}

	wantCodes := []string{
		doctorProblemInvalidRepository,
		doctorProblemInvalidAppState,
		doctorProblemMissingBinary,
		doctorProblemBinaryNotRegular,
		doctorProblemBinaryNotExec,
		doctorProblemHashMismatch,
		doctorProblemMissingWrapper,
		doctorProblemWrapperNotRegular,
		doctorProblemWrapperNotExec,
		doctorProblemForeignWrapper,
		doctorProblemIOError,
	}
	gotCodes := make(map[string]bool)
	for _, app := range report.Apps {
		for _, problem := range app.Problems {
			gotCodes[problem.Code] = true
		}
	}
	for _, code := range wantCodes {
		if !gotCodes[code] {
			t.Errorf("report did not contain problem code %q:\n%s", code, output.String())
		}
	}
	if len(report.Apps) != len(apps) {
		t.Fatalf("diagnostics stopped early: got %d apps, want %d", len(report.Apps), len(apps))
	}
}

func TestDoctorContinuesAfterInvalidConfigAndPreservesAllFiles(t *testing.T) {
	doctorFixtureEnvironment(t)
	cfg := &config.Config{InstallDir: "~/tools"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config fixture: %v", err)
	}
	app := doctorModernApp(t, []byte("read-only binary"))
	writeDoctorState(t, map[string]state.App{"acme/widget": app})

	configPath := mustDoctorPath(t, config.Path)
	statePath := mustDoctorPath(t, state.Path)
	snapshots := map[string]doctorFileSnapshot{
		configPath:      snapshotDoctorFile(t, configPath),
		statePath:       snapshotDoctorFile(t, statePath),
		app.RealPath:    snapshotDoctorFile(t, app.RealPath),
		app.InstallPath: snapshotDoctorFile(t, app.InstallPath),
	}

	if err := os.WriteFile(configPath, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	corruptConfig := snapshotDoctorFile(t, configPath)

	oldJSON := doctorJSON
	t.Cleanup(func() { doctorJSON = oldJSON })
	doctorJSON = true
	var output bytes.Buffer
	err := runDoctor(doctorCommand(&output), nil)
	if err == nil || commandExitCode(err) != 1 {
		t.Fatalf("doctor error = %v, want exit 1", err)
	}
	var report doctorReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("parse JSON report: %v\n%s", decodeErr, output.String())
	}
	if report.Config.Healthy || !hasDoctorProblem(report.Config.Problems, doctorProblemInvalidConfig) {
		t.Fatalf("config diagnostic = %#v", report.Config)
	}
	if !report.State.Healthy || len(report.Apps) != 1 || !report.Apps[0].Healthy {
		t.Fatalf("state/app diagnostics did not continue: %#v", report)
	}

	assertDoctorSnapshotUnchanged(t, corruptConfig, configPath)
	for path, before := range snapshots {
		if path == configPath {
			continue
		}
		assertDoctorSnapshotUnchanged(t, before, path)
	}
}

func TestDoctorReportsCorruptStateWithoutInspectingApplications(t *testing.T) {
	doctorFixtureEnvironment(t)
	statePath := mustDoctorPath(t, state.Path)
	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	before := snapshotDoctorFile(t, statePath)

	oldJSON := doctorJSON
	t.Cleanup(func() { doctorJSON = oldJSON })
	doctorJSON = true
	var output bytes.Buffer
	err := runDoctor(doctorCommand(&output), []string{"acme/missing"})
	if err == nil || commandExitCode(err) != 1 {
		t.Fatalf("doctor error = %v, want exit 1", err)
	}
	var report doctorReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("parse JSON report: %v\n%s", decodeErr, output.String())
	}
	if report.State.Healthy || !hasDoctorProblem(report.State.Problems, doctorProblemInvalidState) {
		t.Fatalf("state diagnostic = %#v", report.State)
	}
	if len(report.Apps) != 0 || report.Summary != (doctorSummary{}) {
		t.Fatalf("applications were inspected with invalid state: %#v", report)
	}
	assertDoctorSnapshotUnchanged(t, before, statePath)
}

func TestDoctorMissingConfigAndStateAreHealthy(t *testing.T) {
	doctorFixtureEnvironment(t)

	oldJSON := doctorJSON
	t.Cleanup(func() { doctorJSON = oldJSON })
	doctorJSON = true
	var output bytes.Buffer
	if err := runDoctor(doctorCommand(&output), nil); err != nil {
		t.Fatalf("doctor with missing stores: %v", err)
	}
	var report doctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("parse JSON report: %v\n%s", err, output.String())
	}
	if !report.Healthy || !report.Config.Healthy || !report.State.Healthy || len(report.Apps) != 0 || report.Summary != (doctorSummary{}) {
		t.Fatalf("missing-store report = %#v", report)
	}
}

func TestDoctorReportsInvalidInstallDirectoryAndEnvironmentErrors(t *testing.T) {
	doctorFixtureEnvironment(t)
	cfg := &config.Config{InstallDir: "relative/tools"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config fixture: %v", err)
	}

	oldJSON := doctorJSON
	t.Cleanup(func() { doctorJSON = oldJSON })
	doctorJSON = true
	var output bytes.Buffer
	err := runDoctor(doctorCommand(&output), nil)
	if err == nil || commandExitCode(err) != 1 {
		t.Fatalf("invalid config doctor error = %v, want exit 1", err)
	}
	var report doctorReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("parse invalid-config JSON: %v", decodeErr)
	}
	if report.Config.Healthy || !hasDoctorProblem(report.Config.Problems, doctorProblemInvalidConfig) {
		t.Fatalf("invalid install directory diagnostic = %#v", report.Config)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	output.Reset()
	err = runDoctor(doctorCommand(&output), nil)
	if err == nil || commandExitCode(err) != 1 {
		t.Fatalf("environment doctor error = %v, want exit 1", err)
	}
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("parse environment JSON: %v", decodeErr)
	}
	if report.Config.Healthy || report.State.Healthy ||
		!hasDoctorProblem(report.Config.Problems, doctorProblemInvalidConfig) ||
		!hasDoctorProblem(report.State.Problems, doctorProblemInvalidState) {
		t.Fatalf("environment diagnostics = %#v", report)
	}
}

func doctorFixtureEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

func doctorCommand(output *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(output)
	cmd.SetErr(io.Discard)
	return cmd
}

func doctorModernApp(t *testing.T, contents []byte) state.App {
	t.Helper()
	root := t.TempDir()
	realPath := filepath.Join(root, "real-bin")
	installPath := filepath.Join(root, "wrapper")
	writeDoctorExecutable(t, realPath, contents, 0755)
	writeDoctorExecutable(t, installPath, []byte("#!/bin/sh\n# "+wrapper.Marker+"\n"), 0755)
	return state.App{
		Version:     "v1.0.0",
		InstalledAt: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		InstallPath: installPath,
		RealPath:    realPath,
		XXH3_64:     hashx.Bytes(contents),
	}
}

func doctorLegacyApp(t *testing.T, contents []byte) state.App {
	t.Helper()
	root := t.TempDir()
	installPath := filepath.Join(root, "legacy-bin")
	writeDoctorExecutable(t, installPath, contents, 0755)
	return state.App{
		Version:     "v1.0.0",
		InstalledAt: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		InstallPath: installPath,
		XXH3_64:     hashx.Bytes(contents),
	}
}

func writeDoctorExecutable(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func writeDoctorState(t *testing.T, apps map[string]state.App) {
	t.Helper()
	st := &state.State{Apps: apps}
	if err := st.Save(); err != nil {
		t.Fatalf("save doctor state: %v", err)
	}
}

type doctorFileSnapshot struct {
	Contents []byte
	ModTime  time.Time
}

func snapshotDoctorFile(t *testing.T, path string) doctorFileSnapshot {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot %q: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot %q: %v", path, err)
	}
	return doctorFileSnapshot{Contents: contents, ModTime: info.ModTime()}
}

func assertDoctorSnapshotUnchanged(t *testing.T, before doctorFileSnapshot, path string) {
	t.Helper()
	after := snapshotDoctorFile(t, path)
	if !bytes.Equal(after.Contents, before.Contents) {
		t.Errorf("%s contents changed", path)
	}
	if !after.ModTime.Equal(before.ModTime) {
		t.Errorf("%s modification time changed from %s to %s", path, before.ModTime, after.ModTime)
	}
}

func mustDoctorPath(t *testing.T, pathFunc func() (string, error)) string {
	t.Helper()
	path, err := pathFunc()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	return path
}

func hasDoctorProblem(problems []doctorProblem, code string) bool {
	for _, problem := range problems {
		if problem.Code == code {
			return true
		}
	}
	return false
}

func equalStrings(got, want []string) bool {
	return slices.Equal(got, want)
}

func TestDoctorProblemDetailsAndRepositoryOrdering(t *testing.T) {
	problems := []doctorProblem{{Code: "first", Message: "one"}, {Code: "second", Message: "two"}}
	if got, want := doctorProblemDetails(problems), "first: one; second: two"; got != want {
		t.Errorf("doctorProblemDetails() = %q, want %q", got, want)
	}
	if got := doctorProblemDetails(nil); got != "ok" {
		t.Errorf("doctorProblemDetails(nil) = %q, want ok", got)
	}

	registry := &state.State{Apps: map[string]state.App{
		"zeta/repo": {},
		"acme/repo": {},
	}}
	if got := doctorRepositories(registry, []string{"zeta/repo", "acme/repo", "acme/repo"}); !equalStrings(got, []string{"acme/repo", "zeta/repo"}) {
		t.Errorf("doctorRepositories() = %q", got)
	}
	if got := doctorRepositories(registry, nil); !equalStrings(got, []string{"acme/repo", "zeta/repo"}) {
		t.Errorf("doctorRepositories(all) = %q", got)
	}
	if got := doctorRepositories(nil, []string{"acme/repo"}); !equalStrings(got, []string{"acme/repo"}) {
		t.Errorf("doctorRepositories(nil) = %q", got)
	}

	var output bytes.Buffer
	if err := writeDoctorHuman(&output, doctorReport{Config: newDoctorCheck(), State: newDoctorCheck(), Apps: []doctorApplication{}, Summary: doctorSummary{}}); err != nil {
		t.Fatalf("write empty human report: %v", err)
	}
	if !strings.Contains(output.String(), "Summary: 0 healthy, 0 unhealthy") {
		t.Errorf("empty human report = %q", output.String())
	}
}

func TestDoctorExitErrorImplementsCommandExitCode(t *testing.T) {
	err := &doctorExitError{message: "failure"}
	if err.Error() != "failure" || err.ExitCode() != 1 || commandExitCode(err) != 1 {
		t.Fatalf("doctor exit error = %v, code %d", err, commandExitCode(err))
	}
}

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/config"
	"zenget/internal/hashx"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

const doctorSchemaVersion = 1

const (
	doctorProblemInvalidConfig     = "invalid_config"
	doctorProblemInvalidState      = "invalid_state"
	doctorProblemInvalidRepository = "invalid_repository"
	doctorProblemInvalidAppState   = "invalid_app_state"
	doctorProblemNotInstalled      = "not_installed"
	doctorProblemMissingBinary     = "missing_binary"
	doctorProblemBinaryNotRegular  = "binary_not_regular"
	doctorProblemBinaryNotExec     = "binary_not_executable"
	doctorProblemHashMismatch      = "hash_mismatch"
	doctorProblemMissingWrapper    = "missing_wrapper"
	doctorProblemWrapperNotRegular = "wrapper_not_regular"
	doctorProblemWrapperNotExec    = "wrapper_not_executable"
	doctorProblemForeignWrapper    = "foreign_wrapper"
	doctorProblemIOError           = "io_error"
)

var doctorJSON bool

var doctorCmd = &cobra.Command{
	Use:   "doctor [org/repo...]",
	Short: "Diagnose local configuration and installed applications",
	Args:  validateDoctorArgs,
	RunE:  runDoctor,
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "print the diagnostic report as JSON")
	rootCmd.AddCommand(doctorCmd)
}

func validateDoctorArgs(_ *cobra.Command, args []string) error {
	for _, repository := range args {
		if !repoPattern.MatchString(repository) {
			return fmt.Errorf("invalid repository %q: expected org/repo", repository)
		}
	}
	return nil
}

func runDoctor(cmd *cobra.Command, args []string) error {
	if err := validateDoctorArgs(cmd, args); err != nil {
		return err
	}

	report := diagnoseDoctor(args)
	var err error
	if doctorJSON {
		err = writeDoctorJSON(cmd.OutOrStdout(), report)
	} else {
		err = writeDoctorHuman(cmd.OutOrStdout(), report)
	}
	if err != nil {
		return err
	}
	if report.Healthy {
		return nil
	}

	// The report is the useful command output. Keep Cobra from appending an
	// error or usage block after it, especially when stdout contains JSON.
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	return &doctorExitError{message: "doctor found unhealthy diagnostics"}
}

type doctorExitError struct {
	message string
}

func (e *doctorExitError) Error() string {
	return e.message
}

func (e *doctorExitError) ExitCode() int {
	return 1
}

type doctorProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type doctorCheck struct {
	Path     string          `json:"path"`
	Healthy  bool            `json:"healthy"`
	Problems []doctorProblem `json:"problems"`
}

type doctorApplication struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	InstallPath string          `json:"install_path"`
	RealPath    string          `json:"real_path"`
	BinaryPath  string          `json:"binary_path"`
	Healthy     bool            `json:"healthy"`
	Problems    []doctorProblem `json:"problems"`
}

type doctorSummary struct {
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
}

type doctorReport struct {
	SchemaVersion int                 `json:"schema_version"`
	Healthy       bool                `json:"healthy"`
	Config        doctorCheck         `json:"config"`
	State         doctorCheck         `json:"state"`
	Apps          []doctorApplication `json:"apps"`
	Summary       doctorSummary       `json:"summary"`
}

func diagnoseDoctor(requested []string) doctorReport {
	report := doctorReport{
		SchemaVersion: doctorSchemaVersion,
		Config:        diagnoseDoctorConfig(),
		Apps:          make([]doctorApplication, 0),
	}

	var registry *state.State
	report.State, registry = diagnoseDoctorState()
	if registry != nil {
		for _, repository := range doctorRepositories(registry, requested) {
			report.Apps = append(report.Apps, diagnoseDoctorApplication(repository, registry))
		}
	}

	for _, app := range report.Apps {
		if app.Healthy {
			report.Summary.Healthy++
		} else {
			report.Summary.Unhealthy++
		}
	}
	report.Healthy = report.Config.Healthy && report.State.Healthy && report.Summary.Unhealthy == 0
	return report
}

func diagnoseDoctorConfig() doctorCheck {
	check := newDoctorCheck()

	path, err := config.Path()
	if err != nil {
		addDoctorProblem(&check.Problems, doctorProblemInvalidConfig,
			fmt.Sprintf("cannot locate configuration: %v", err))
		check.Healthy = false
		return check
	}
	check.Path = path

	cfg, err := config.Load()
	if err != nil {
		addDoctorProblem(&check.Problems, doctorProblemInvalidConfig,
			fmt.Sprintf("configuration is unreadable or invalid: %v", err))
		check.Healthy = false
		return check
	}
	if cfg == nil {
		addDoctorProblem(&check.Problems, doctorProblemInvalidConfig, "configuration is empty")
		check.Healthy = false
		return check
	}

	if err := config.ValidateInstallDir(cfg.InstallDir); err != nil {
		addDoctorProblem(&check.Problems, doctorProblemInvalidConfig,
			fmt.Sprintf("install_dir is invalid: %v", err))
	} else if cfg.InstallDir != "" {
		if _, err := cfg.InstallDirectory(); err != nil {
			addDoctorProblem(&check.Problems, doctorProblemInvalidConfig,
				fmt.Sprintf("install_dir cannot be resolved: %v", err))
		}
	}

	check.Healthy = len(check.Problems) == 0
	return check
}

func diagnoseDoctorState() (doctorCheck, *state.State) {
	check := newDoctorCheck()

	path, err := state.Path()
	if err != nil {
		addDoctorProblem(&check.Problems, doctorProblemInvalidState,
			fmt.Sprintf("cannot locate state registry: %v", err))
		check.Healthy = false
		return check, nil
	}
	check.Path = path

	registry, err := state.Load()
	if err != nil {
		addDoctorProblem(&check.Problems, doctorProblemInvalidState,
			fmt.Sprintf("state registry is unreadable or invalid: %v", err))
		check.Healthy = false
		return check, nil
	}

	check.Healthy = true
	return check, registry
}

func newDoctorCheck() doctorCheck {
	return doctorCheck{
		Healthy:  true,
		Problems: make([]doctorProblem, 0),
	}
}

func doctorRepositories(registry *state.State, requested []string) []string {
	repositories := make([]string, 0)
	if len(requested) == 0 {
		if registry != nil {
			repositories = make([]string, 0, len(registry.Apps))
			for repository := range registry.Apps {
				repositories = append(repositories, repository)
			}
		}
	} else {
		repositories = append(repositories, requested...)
	}

	sort.Strings(repositories)
	if len(repositories) < 2 {
		return repositories
	}

	unique := repositories[:1]
	for _, repository := range repositories[1:] {
		if repository != unique[len(unique)-1] {
			unique = append(unique, repository)
		}
	}
	return unique
}

func diagnoseDoctorApplication(repository string, registry *state.State) doctorApplication {
	result := doctorApplication{
		Name:     repository,
		Healthy:  true,
		Problems: make([]doctorProblem, 0),
	}

	app, ok := registry.Get(repository)
	if !ok {
		addDoctorProblem(&result.Problems, doctorProblemNotInstalled,
			fmt.Sprintf("%s is not installed", repository))
		result.Healthy = false
		return result
	}

	result.Version = app.Version
	result.InstallPath = app.InstallPath
	result.RealPath = app.RealPath
	result.BinaryPath = app.RealPath
	if result.BinaryPath == "" {
		result.BinaryPath = app.InstallPath
	}

	if !repoPattern.MatchString(repository) {
		addDoctorProblem(&result.Problems, doctorProblemInvalidRepository,
			fmt.Sprintf("%q is not a valid org/repo repository key", repository))
	}
	if strings.TrimSpace(app.Version) == "" {
		addDoctorProblem(&result.Problems, doctorProblemInvalidAppState, "version is empty")
	}
	if app.InstallPath == "" {
		addDoctorProblem(&result.Problems, doctorProblemInvalidAppState, "install path is empty")
	}
	if strings.TrimSpace(app.XXH3_64) == "" {
		addDoctorProblem(&result.Problems, doctorProblemInvalidAppState, "XXH3-64 hash is empty")
	}

	if result.BinaryPath != "" {
		diagnoseDoctorBinary(result.BinaryPath, app.XXH3_64, &result.Problems)
	}
	if app.RealPath != "" && app.InstallPath != "" {
		diagnoseDoctorWrapper(app.InstallPath, &result.Problems)
	}

	result.Healthy = len(result.Problems) == 0
	return result
}

func diagnoseDoctorBinary(path, expectedHash string, problems *[]doctorProblem) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			addDoctorProblem(problems, doctorProblemMissingBinary,
				fmt.Sprintf("binary does not exist: %s", path))
		} else {
			addDoctorProblem(problems, doctorProblemIOError,
				fmt.Sprintf("cannot inspect binary %q: %v", path, err))
		}
		return
	}

	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		addDoctorProblem(problems, doctorProblemBinaryNotRegular,
			fmt.Sprintf("binary is not a regular file: %s", path))
		return
	}
	if info.Mode().Perm()&0111 == 0 {
		addDoctorProblem(problems, doctorProblemBinaryNotExec,
			fmt.Sprintf("binary is not executable: %s", path))
	}
	if info.Mode().Perm()&0444 == 0 {
		addDoctorProblem(problems, doctorProblemIOError,
			fmt.Sprintf("binary is not readable: %s", path))
		return
	}

	file, err := os.Open(path)
	if err != nil {
		addDoctorProblem(problems, doctorProblemIOError,
			fmt.Sprintf("cannot read binary %q: %v", path, err))
		return
	}
	if err := file.Close(); err != nil {
		addDoctorProblem(problems, doctorProblemIOError,
			fmt.Sprintf("cannot close binary %q: %v", path, err))
		return
	}

	if expectedHash == "" {
		return
	}
	actualHash, err := hashx.File(path)
	if err != nil {
		addDoctorProblem(problems, doctorProblemIOError,
			fmt.Sprintf("cannot hash binary %q: %v", path, err))
		return
	}
	if actualHash != expectedHash {
		addDoctorProblem(problems, doctorProblemHashMismatch,
			fmt.Sprintf("binary hash mismatch: expected %s, got %s", expectedHash, actualHash))
	}
}

func diagnoseDoctorWrapper(path string, problems *[]doctorProblem) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			addDoctorProblem(problems, doctorProblemMissingWrapper,
				fmt.Sprintf("wrapper does not exist: %s", path))
		} else {
			addDoctorProblem(problems, doctorProblemIOError,
				fmt.Sprintf("cannot inspect wrapper %q: %v", path, err))
		}
		return
	}

	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		addDoctorProblem(problems, doctorProblemWrapperNotRegular,
			fmt.Sprintf("wrapper is not a regular file: %s", path))
		return
	}
	if info.Mode().Perm()&0111 == 0 {
		addDoctorProblem(problems, doctorProblemWrapperNotExec,
			fmt.Sprintf("wrapper is not executable: %s", path))
	}
	if info.Mode().Perm()&0444 == 0 {
		addDoctorProblem(problems, doctorProblemIOError,
			fmt.Sprintf("wrapper is not readable: %s", path))
		return
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		addDoctorProblem(problems, doctorProblemIOError,
			fmt.Sprintf("cannot read wrapper %q: %v", path, err))
		return
	}
	if !wrapper.IsMarkerLine(string(contents)) {
		addDoctorProblem(problems, doctorProblemForeignWrapper,
			fmt.Sprintf("wrapper does not contain the zenget marker: %s", path))
	}
}

func addDoctorProblem(problems *[]doctorProblem, code, message string) {
	*problems = append(*problems, doctorProblem{Code: code, Message: message})
}

func writeDoctorJSON(out io.Writer, report doctorReport) error {
	if out == nil {
		out = io.Discard
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal doctor report: %w", err)
	}
	_, err = fmt.Fprintf(out, "%s\n", data)
	return err
}

func writeDoctorHuman(out io.Writer, report doctorReport) error {
	if out == nil {
		out = io.Discard
	}

	if err := writeDoctorGlobalStatus(out, "CONFIG", report.Config); err != nil {
		return err
	}
	if err := writeDoctorGlobalStatus(out, "STATE", report.State); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "NAME\tVERSION\tSTATUS\tDETAILS"); err != nil {
		return err
	}
	for _, app := range report.Apps {
		status := "HEALTHY"
		details := "ok"
		if !app.Healthy {
			status = "UNHEALTHY"
			details = doctorProblemDetails(app.Problems)
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", app.Name, app.Version, status, details); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "Summary: %d healthy, %d unhealthy\n", report.Summary.Healthy, report.Summary.Unhealthy)
	return err
}

func writeDoctorGlobalStatus(out io.Writer, name string, check doctorCheck) error {
	status := "HEALTHY"
	details := "ok"
	if !check.Healthy {
		status = "UNHEALTHY"
		details = doctorProblemDetails(check.Problems)
	}
	_, err := fmt.Fprintf(out, "%s: %s (%s)\n", name, status, details)
	return err
}

func doctorProblemDetails(problems []doctorProblem) string {
	if len(problems) == 0 {
		return "ok"
	}

	details := make([]string, 0, len(problems))
	for _, problem := range problems {
		details = append(details, problem.Code+": "+problem.Message)
	}
	return strings.Join(details, "; ")
}

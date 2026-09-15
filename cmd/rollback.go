package cmd

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/hashx"
	"zenget/internal/install"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback <org/repo>",
	Short: "Switch to the previously installed version without downloading",
	Args:  cobra.ExactArgs(1),
	RunE:  runRollback,
}

func init() {
	rootCmd.AddCommand(rollbackCmd)
}

func runRollback(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || !repoPattern.MatchString(args[0]) {
		return fmt.Errorf("invalid repository %q: expected org/repo", firstArg(args))
	}
	return runRollbackWithWriters(args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// runRollbackWithWriters switches a registered application to its retained
// previous artifact without creating a GitHub client or making network calls.
func runRollbackWithWriters(repository string, out, errOut io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}

	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	app, ok := st.Get(repository)
	if !ok {
		return fmt.Errorf("%s is not installed", repository)
	}

	active := app.Active()
	previous := cloneRollbackArtifact(app.PreviousArtifact)
	if active == nil || previous == nil {
		return fmt.Errorf("rollback for %s requires an active and previous artifact", repository)
	}
	if err := validateRollbackArtifact("active", active); err != nil {
		return err
	}
	if err := validateRollbackArtifact("previous", previous); err != nil {
		return err
	}
	if active.RealPath == previous.RealPath {
		return fmt.Errorf("rollback for %s requires distinct active and previous artifacts", repository)
	}

	usageLog, err := validateRollbackWrapper(app.InstallPath, active.RealPath)
	if err != nil {
		return err
	}

	transaction, err := install.PrepareWrapperUpdate(app.InstallPath, repository, previous.RealPath, usageLog)
	if err != nil {
		return err
	}

	updated := app
	updated.Version = previous.Version
	updated.RealPath = previous.RealPath
	updated.XXH3_64 = previous.XXH3_64
	updated.ActiveArtifact = cloneRollbackArtifact(previous)
	updated.PreviousArtifact = cloneRollbackArtifact(active)
	st.Set(repository, updated)
	if err := persistState(st); err != nil {
		if rollbackErr := transaction.Rollback(); rollbackErr != nil {
			return fmt.Errorf("save state: %w (rollback failed: %v)", err, rollbackErr)
		}
		return fmt.Errorf("save state: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit rollback: %w", err)
	}
	for _, warning := range transaction.CleanupWarnings() {
		if _, err := fmt.Fprintf(errOut, "warning: %v\n", warning); err != nil {
			return err
		}
	}

	_, err = fmt.Fprintf(out, "Rolled back %s %s -> %s\n", repository, active.Version, previous.Version)
	return err
}

func cloneRollbackArtifact(artifact *state.Artifact) *state.Artifact {
	if artifact == nil {
		return nil
	}
	copy := *artifact
	return &copy
}

func validateRollbackArtifact(label string, artifact *state.Artifact) error {
	if artifact == nil || artifact.Version == "" || artifact.RealPath == "" || artifact.XXH3_64 == "" {
		return fmt.Errorf("rollback %s artifact metadata is incomplete", label)
	}
	managed, err := wrapper.IsManagedArtifactPath(artifact.RealPath)
	if err != nil {
		return fmt.Errorf("validate rollback %s artifact %q: %w", label, artifact.RealPath, err)
	}
	if !managed {
		return fmt.Errorf("refusing to rollback to %s artifact outside zenget data directory: %q", label, artifact.RealPath)
	}
	info, err := os.Lstat(artifact.RealPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("rollback %s artifact %q is missing", label, artifact.RealPath)
		}
		return fmt.Errorf("check rollback %s artifact %q: %w", label, artifact.RealPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("rollback %s artifact %q is not a regular file", label, artifact.RealPath)
	}
	if info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("rollback %s artifact %q is not executable", label, artifact.RealPath)
	}
	hash, err := hashx.File(artifact.RealPath)
	if err != nil {
		return fmt.Errorf("hash rollback %s artifact %q: %w", label, artifact.RealPath, err)
	}
	if hash != artifact.XXH3_64 {
		return fmt.Errorf("rollback %s artifact %q has a hash mismatch", label, artifact.RealPath)
	}
	return nil
}

func validateRollbackWrapper(wrapperPath, activePath string) (string, error) {
	if wrapperPath == "" {
		return "", fmt.Errorf("rollback wrapper path is missing")
	}
	exists, err := preflightWrapper(wrapperPath)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("rollback wrapper %q is missing", wrapperPath)
	}
	data, err := os.ReadFile(wrapperPath)
	if err != nil {
		return "", fmt.Errorf("read rollback wrapper %q: %w", wrapperPath, err)
	}
	realBin, ok := wrapperAssignment(string(data), "real_bin")
	if !ok || realBin != activePath {
		return "", fmt.Errorf("rollback wrapper %q does not target active artifact %q", wrapperPath, activePath)
	}
	usageLog, ok := wrapperAssignment(string(data), "usage_log")
	if !ok || usageLog == "" {
		return "", fmt.Errorf("rollback wrapper %q has no usage log", wrapperPath)
	}
	return usageLog, nil
}

func wrapperAssignment(script, name string) (string, bool) {
	prefix := name + "="
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value, err := strconv.Unquote(strings.TrimPrefix(line, prefix))
		if err != nil {
			return "", false
		}
		return value, true
	}
	return "", false
}

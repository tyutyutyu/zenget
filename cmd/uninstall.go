package cmd

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/hashx"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

var uninstallCmd = &cobra.Command{
	Use:     "uninstall <name|org/repo>",
	Aliases: []string{"rm", "un"},
	Short:   "Remove an installed binary and its registry entry",
	Args:    cobra.ExactArgs(1),
	RunE:    runUninstall,
}

func init() {
	rootCmd.AddCommand(uninstallCmd)
}

func runUninstall(cmd *cobra.Command, args []string) error {
	repository := firstArg(args)
	if strings.Contains(repository, "/") {
		if !repoPattern.MatchString(repository) {
			return fmt.Errorf("invalid repository %q: expected org/repo", repository)
		}
		return runUninstallWithWriters(repository, cmd.OutOrStdout(), cmd.ErrOrStderr())
	}

	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	repository, err = resolveUninstallRepository(repository, st)
	if err != nil {
		return err
	}
	return runUninstallWithWriters(repository, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

func resolveUninstallRepository(name string, st *state.State) (string, error) {
	var matches []string
	for repository := range st.Apps {
		separator := strings.LastIndex(repository, "/")
		if separator >= 0 && repository[separator+1:] == name {
			matches = append(matches, repository)
		}
	}

	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%s is not installed (invalid repository %q unless it matches an installed short name)", name, name)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%q matches multiple installed repositories: %s", name, strings.Join(matches, ", "))
	}
}

// runUninstallWithWriters removes the binary and registry entry for
// repository, writing status messages to out and warnings to errOut.
func runUninstallWithWriters(repository string, out, errOut io.Writer) error {
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

	artifactPaths := app.ArtifactPaths()
	legacyBinary := len(artifactPaths) == 0 && app.RealPath == ""
	binaryPath := app.InstallPath
	if len(artifactPaths) > 0 {
		binaryPath = artifactPaths[0].RealPath
	}
	if !legacyBinary && binaryPath == "" {
		return fmt.Errorf("application %s has no active artifact path", repository)
	}

	// Validate every owned path before removing any of them. In particular, a
	// modified or foreign previous artifact must not allow the active artifact
	// to be deleted first.
	artifactExists := make([]string, 0, len(artifactPaths))
	if legacyBinary {
		exists, err := preflightLegacyBinary(app.InstallPath, app.XXH3_64)
		if err != nil {
			return err
		}
		if exists {
			artifactExists = append(artifactExists, app.InstallPath)
		}
	} else {
		seen := make(map[string]struct{}, len(artifactPaths))
		for _, artifact := range artifactPaths {
			if artifact == nil || artifact.RealPath == "" {
				return fmt.Errorf("application %s has incomplete artifact metadata", repository)
			}
			if _, ok := seen[artifact.RealPath]; ok {
				continue
			}
			seen[artifact.RealPath] = struct{}{}
			exists, err := preflightManagedArtifact(artifact)
			if err != nil {
				return err
			}
			if exists {
				artifactExists = append(artifactExists, artifact.RealPath)
			}
		}
	}

	wrapperPath := ""
	wrapperExists := false
	if !legacyBinary && app.InstallPath != "" {
		wrapperPath = app.InstallPath
		wrapperExists, err = preflightWrapper(wrapperPath)
		if err != nil {
			return err
		}
	}

	if len(artifactExists) == 0 && !wrapperExists {
		if _, err := fmt.Fprintf(errOut, "warning: %s not found on disk, removing registry entry\n", binaryPath); err != nil {
			return err
		}
	}
	for _, artifactPath := range artifactExists {
		if err := os.Remove(artifactPath); err != nil {
			return fmt.Errorf("remove binary %q: %w", artifactPath, err)
		}
	}
	if wrapperExists {
		if err := os.Remove(wrapperPath); err != nil {
			return fmt.Errorf("remove wrapper %q: %w", wrapperPath, err)
		}
	}

	st.Delete(repository)
	if err := st.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	_, err = fmt.Fprintf(out, "Removed %s (%s)\n", repository, binaryPath)
	return err
}

func preflightLegacyBinary(binaryPath, expectedHash string) (bool, error) {
	info, err := os.Lstat(binaryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("check install path %q: %w", binaryPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("hash installed binary %q: not a regular file; refusing to remove", binaryPath)
	}
	hash, err := hashx.File(binaryPath)
	if err != nil {
		return false, fmt.Errorf("hash installed binary %q: %w", binaryPath, err)
	}
	if hash != expectedHash {
		return false, fmt.Errorf("%s was modified after install (hash mismatch); refusing to remove", binaryPath)
	}
	return true, nil
}

func preflightManagedArtifact(artifact *state.Artifact) (bool, error) {
	managed, err := wrapper.IsManagedArtifactPath(artifact.RealPath)
	if err != nil {
		return false, fmt.Errorf("validate artifact %q: %w", artifact.RealPath, err)
	}
	if !managed {
		return false, fmt.Errorf("refusing to remove artifact outside zenget data directory: %q", artifact.RealPath)
	}
	if artifact.XXH3_64 == "" {
		return false, fmt.Errorf("cannot remove artifact %q: hash is missing", artifact.RealPath)
	}
	info, err := os.Lstat(artifact.RealPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("check artifact %q: %w", artifact.RealPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("artifact %q is not a regular file; refusing to remove", artifact.RealPath)
	}
	hash, err := hashx.File(artifact.RealPath)
	if err != nil {
		return false, fmt.Errorf("hash artifact %q: %w", artifact.RealPath, err)
	}
	if hash != artifact.XXH3_64 {
		return false, fmt.Errorf("%s was modified after install (hash mismatch); refusing to remove", artifact.RealPath)
	}
	return true, nil
}

func preflightWrapper(wrapperPath string) (bool, error) {
	info, err := os.Lstat(wrapperPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("check wrapper %q: %w", wrapperPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("wrapper %q is not a regular file; refusing to remove", wrapperPath)
	}
	data, err := os.ReadFile(wrapperPath)
	if err != nil {
		return false, fmt.Errorf("read wrapper %q: %w", wrapperPath, err)
	}
	if !wrapper.IsMarkerLine(string(data)) {
		return false, fmt.Errorf("not removing foreign file %q", wrapperPath)
	}
	return true, nil
}

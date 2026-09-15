package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/github"
	"zenget/internal/hashx"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

var ensureCmd = &cobra.Command{
	Use:   "ensure [org/repo...]",
	Short: "Check and restore installed applications",
	Args:  cobra.ArbitraryArgs,
	RunE:  runEnsure,
}

func init() {
	rootCmd.AddCommand(ensureCmd)
}

func runEnsure(cmd *cobra.Command, args []string) error {
	return runEnsureWithClient(cmd.Context(), github.New(), args, cmd.OutOrStdout())
}

// runEnsureWithClient checks selected registry entries and restores unhealthy
// applications from their recorded release tag. A failed application does not
// prevent the remaining entries from being checked.
func runEnsureWithClient(ctx context.Context, client *github.Client, requested []string, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}

	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	repositories := requested
	if len(repositories) == 0 {
		repositories = make([]string, 0, len(st.Apps))
		for repository := range st.Apps {
			repositories = append(repositories, repository)
		}
		sort.Strings(repositories)
	}

	var restored, healthy, failed int
	for _, repository := range repositories {
		if !repoPattern.MatchString(repository) {
			failed++
			if err := writeEnsureStatus(out, "FAILED", repository, fmt.Errorf("invalid repository %q: expected org/repo", repository)); err != nil {
				return err
			}
			continue
		}

		app, ok := st.Get(repository)
		if !ok {
			failed++
			if err := writeEnsureStatus(out, "FAILED", repository, fmt.Errorf("%s is not registered", repository)); err != nil {
				return err
			}
			continue
		}

		if appHealthy(app) {
			healthy++
			if _, err := fmt.Fprintf(out, "HEALTHY %s (%s)\n", repository, app.Version); err != nil {
				return err
			}
			continue
		}

		if err := ensureApplication(ctx, client, repository, app, out); err != nil {
			failed++
			if err := writeEnsureStatus(out, "FAILED", repository, err); err != nil {
				return err
			}
			continue
		}
		restored++
		if _, err := fmt.Fprintf(out, "RESTORED %s (%s)\n", repository, app.Version); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(out, "Ensure summary: %d restored, %d healthy, %d failed\n", restored, healthy, failed); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("ensure failed for %d application(s)", failed)
	}
	return nil
}

func writeEnsureStatus(out io.Writer, status, repository string, err error) error {
	_, writeErr := fmt.Fprintf(out, "%s %s: %v\n", status, repository, err)
	return writeErr
}

func appHealthy(app state.App) bool {
	if app.RealPath == "" || app.XXH3_64 == "" || app.InstallPath == "" {
		return false
	}
	if _, err := os.Stat(app.RealPath); err != nil {
		return false
	}
	hash, err := hashx.File(app.RealPath)
	if err != nil || hash != app.XXH3_64 {
		return false
	}
	wrapperContents, err := os.ReadFile(app.InstallPath)
	return err == nil && wrapper.IsMarkerLine(string(wrapperContents))
}

func ensureApplication(ctx context.Context, client *github.Client, repository string, app state.App, out io.Writer) error {
	targetName := path.Base(repository)
	if app.InstallPath != "" {
		candidate := path.Base(app.InstallPath)
		if validTargetName(candidate) {
			targetName = candidate
		}
	}
	return runInstallWithOptions(ctx, client, repository, installOptions{
		tag:        app.Version,
		targetName: targetName,
		input:      strings.NewReader(""),
		force:      true,
		exactTag:   true,
	}, out)
}

package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/state"
)

var (
	pruneForce        bool
	pruneDryRun       bool
	prunePromptReader io.Reader
	pruneIsTerminal   = stdinIsTerminal
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove orphaned registry entries",
	Args:  cobra.NoArgs,
	RunE:  runPrune,
}

func init() {
	pruneCmd.Flags().BoolVarP(&pruneForce, "force", "f", false, "Remove orphaned entries without confirmation")
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "List orphaned entries without changing the registry")
	rootCmd.AddCommand(pruneCmd)
}

type pruneEntry struct {
	repository   string
	wrapperFound bool
	realFound    bool
	inspectError error
}

func runPrune(cmd *cobra.Command, args []string) error {
	output := cmd.OutOrStdout()
	if output == nil {
		output = io.Discard
	}
	input := prunePromptReader
	if input == nil {
		input = os.Stdin
	}
	return runPruneWithOptions(pruneForce, pruneDryRun, pruneIsTerminal(), input, output)
}

func runPruneWithOptions(force, dryRun, interactive bool, input io.Reader, output io.Writer) error {
	if output == nil {
		output = io.Discard
	}
	if input == nil {
		input = strings.NewReader("")
	}

	registry, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	entries := inspectPruneEntries(registry)
	orphans := make([]pruneEntry, 0)
	for _, entry := range entries {
		if entry.inspectError != nil {
			if _, err := fmt.Fprintf(output, "Warning: cannot inspect %s: %v; keeping registry entry\n", entry.repository, entry.inspectError); err != nil {
				return err
			}
			continue
		}
		if !entry.wrapperFound && !entry.realFound {
			orphans = append(orphans, entry)
			if _, err := fmt.Fprintf(output, "Orphaned registry entry: %s (wrapper and real binary are missing)\n", entry.repository); err != nil {
				return err
			}
			continue
		}
		if entry.wrapperFound != entry.realFound {
			missing := "wrapper"
			if entry.wrapperFound {
				missing = "real binary"
			}
			if _, err := fmt.Fprintf(output, "Warning: %s is partially missing (%s); keeping registry entry; run `zenget ensure %s` to restore\n", entry.repository, missing, entry.repository); err != nil {
				return err
			}
		}
	}

	if len(orphans) == 0 {
		if len(entries) == 0 {
			_, err = fmt.Fprintln(output, "No applications in registry; nothing to prune.")
		} else {
			_, err = fmt.Fprintln(output, "No orphaned registry entries found.")
		}
		return err
	}
	if dryRun {
		_, err := fmt.Fprintf(output, "Dry run: would remove %d orphaned registry entr%s.\n", len(orphans), prunePluralSuffix(len(orphans)))
		return err
	}

	if !force {
		if !interactive {
			return fmt.Errorf("refusing to prune %d orphaned registry entr%s in a non-interactive session; use --force", len(orphans), prunePluralSuffix(len(orphans)))
		}
		confirmed, err := confirmPrune(input, output, len(orphans))
		if err != nil {
			return err
		}
		if !confirmed {
			_, err := fmt.Fprintln(output, "Prune cancelled; registry unchanged.")
			return err
		}
	}

	for _, entry := range orphans {
		registry.Delete(entry.repository)
	}
	if err := registry.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	_, err = fmt.Fprintf(output, "Removed %d orphaned registry entr%s.\n", len(orphans), prunePluralSuffix(len(orphans)))
	return err
}

func inspectPruneEntries(registry *state.State) []pruneEntry {
	if registry == nil {
		return nil
	}
	repositories := make([]string, 0, len(registry.Apps))
	for repository := range registry.Apps {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	entries := make([]pruneEntry, 0, len(repositories))
	for _, repository := range repositories {
		app := registry.Apps[repository]
		wrapperFound, wrapperErr := prunePathExists(app.InstallPath)
		realFound, realErr := prunePathExists(app.RealPath)
		entry := pruneEntry{
			repository:   repository,
			wrapperFound: wrapperFound,
			realFound:    realFound,
			inspectError: errors.Join(wrapperErr, realErr),
		}
		entries = append(entries, entry)
	}
	return entries
}

func prunePathExists(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func confirmPrune(input io.Reader, output io.Writer, count int) (bool, error) {
	if _, err := fmt.Fprintf(output, "Remove %d orphaned registry entr%s? [y/N] ", count, prunePluralSuffix(count)); err != nil {
		return false, err
	}
	answer, readErr := bufio.NewReader(input).ReadString('\n')
	if readErr != nil && readErr != io.EOF {
		return false, fmt.Errorf("read prune confirmation: %w", readErr)
	}
	if _, err := fmt.Fprintln(output); err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func prunePluralSuffix(count int) string {
	if count == 1 {
		return "y"
	}
	return "ies"
}

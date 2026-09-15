package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"zenget/internal/state"
)

var pinCmd = &cobra.Command{
	Use:   "pin <org/repo>",
	Short: "Prevent implicit upgrades for an installed application",
	Args:  cobra.ExactArgs(1),
	RunE:  runPin,
}

func init() {
	rootCmd.AddCommand(pinCmd)
}

func runPin(cmd *cobra.Command, args []string) error {
	repository := firstArg(args)
	if !repoPattern.MatchString(repository) {
		return fmt.Errorf("invalid repository %q: expected org/repo", repository)
	}
	return setPinned(repository, true, cmd.OutOrStdout())
}

func setPinned(repository string, pinned bool, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}

	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	app, ok := st.Get(repository)
	if !ok {
		return fmt.Errorf("%s is not installed", repository)
	}
	if app.Pinned == pinned {
		if pinned {
			_, err = fmt.Fprintf(out, "%s is already pinned at %s\n", repository, app.Version)
		} else {
			_, err = fmt.Fprintf(out, "%s is already unpinned\n", repository)
		}
		return err
	}

	app.Pinned = pinned
	st.Set(repository, app)
	if err := st.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if pinned {
		_, err = fmt.Fprintf(out, "Pinned %s at %s\n", repository, app.Version)
	} else {
		_, err = fmt.Fprintf(out, "Unpinned %s\n", repository)
	}
	return err
}

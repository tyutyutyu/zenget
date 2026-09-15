package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var unpinCmd = &cobra.Command{
	Use:   "unpin <org/repo>",
	Short: "Allow implicit upgrades for a pinned application",
	Args:  cobra.ExactArgs(1),
	RunE:  runUnpin,
}

func init() {
	rootCmd.AddCommand(unpinCmd)
}

func runUnpin(cmd *cobra.Command, args []string) error {
	repository := firstArg(args)
	if !repoPattern.MatchString(repository) {
		return fmt.Errorf("invalid repository %q: expected org/repo", repository)
	}
	return setPinned(repository, false, cmd.OutOrStdout())
}

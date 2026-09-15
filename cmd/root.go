package cmd

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "zenget",
	Short: "Install single-binary GitHub releases into ~/.local/bin",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return validateColorMode(colorFlag)
	},
}

var noManifestDiscovery bool

func init() {
	rootCmd.PersistentFlags().BoolVar(&noManifestDiscovery, "no-manifest-discovery", false,
		"disable automatic zenget.json discovery for commands that support it")
}

// Execute runs the root command and exits with the status carried by typed
// command errors, or status 1 for all other errors.
func Execute() {
	if code := commandExitCode(rootCmd.Execute()); code != 0 {
		os.Exit(code)
	}
}

type exitCoder interface {
	ExitCode() int
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var status exitCoder
	if errors.As(err, &status) {
		return status.ExitCode()
	}
	return 1
}

package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

var whichJSON bool

var whichCmd = &cobra.Command{
	Use:   "which <target>",
	Short: "Show the artifact selected for a target",
	Args:  cobra.ExactArgs(1),
	RunE:  runWhich,
}

func init() {
	whichCmd.Flags().BoolVar(&whichJSON, "json", false, "print the selection as JSON")
	rootCmd.AddCommand(whichCmd)
}

func runWhich(cmd *cobra.Command, args []string) error {
	selection, err := ResolveTarget(args[0], TargetResolveOptions{NoManifestDiscovery: noManifestDiscovery})
	if err != nil {
		return err
	}
	if whichJSON {
		return writeJSON(cmd.OutOrStdout(), selection)
	}
	return printTargetSelection(cmd.OutOrStdout(), selection)
}

func printTargetSelection(out io.Writer, selection TargetSelection) error {
	if out == nil {
		out = io.Discard
	}
	manifestPath := selection.ManifestPath
	if manifestPath == "" {
		manifestPath = "(none)"
	}
	artifactPath := selection.ArtifactPath
	if artifactPath == "" {
		artifactPath = "(not available)"
	}
	_, err := fmt.Fprintf(out,
		"Target: %s\nSelection: %s\nManifest: %s\nProvider: %s\nRepository: %s\nTag: %s\nArtifact: %s\nIntegrity: %s\nCache integrity: %s\n",
		selection.Target, selection.Source, manifestPath, selection.Provider,
		selection.Repository, selection.Tag, artifactPath, selection.Integrity,
		selection.CacheIntegrity,
	)
	if err != nil {
		return err
	}
	if selection.Reason == "" {
		return nil
	}
	_, err = fmt.Fprintf(out, "Reason: %s\n", selection.Reason)
	return err
}

package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	manifestfile "zenget/internal/manifest"
)

const defaultManifestFile = "zenget.json"

var initForce bool

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create an empty portable manifest",
	Args:  cobra.NoArgs,
	RunE:  runInit,
}

func init() {
	initCmd.Flags().StringP("output", "o", defaultManifestFile, "Manifest path to create")
	initCmd.Flags().BoolVar(&initForce, "force", false, "Allow replacing an existing manifest")
	rootCmd.AddCommand(initCmd)
}

func runInit(cmd *cobra.Command, args []string) error {
	output, err := getOptionalStringFlag(cmd, "output")
	if err != nil {
		return fmt.Errorf("read --output flag: %w", err)
	}
	if output == "" {
		output = defaultManifestFile
	}
	if !isLocalManifestPath(output) {
		return fmt.Errorf("manifest output must be a local path")
	}
	data, err := manifestfile.Marshal(manifestfile.New())
	if err != nil {
		return fmt.Errorf("encode empty manifest: %w", err)
	}
	if err := manifestfile.WriteFile(output, data, initForce); err != nil {
		return err
	}
	writer := cmd.OutOrStdout()
	if writer == nil {
		writer = io.Discard
	}
	_, err = fmt.Fprintf(writer, "Initialized manifest %s\n", output)
	return err
}

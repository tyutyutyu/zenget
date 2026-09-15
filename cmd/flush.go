package cmd

import (
	"context"
	"net/http"

	"github.com/spf13/cobra"

	"zenget/internal/config"
	"zenget/internal/upload"
)

var flushCmd = &cobra.Command{
	Use:    "flush",
	Short:  "Flush usage events to the collector",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runFlush,
}

func init() {
	rootCmd.AddCommand(flushCmd)
}

func runFlush(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	if !cfg.UploadEnabled() {
		return nil
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := upload.MaybeUpload(ctx, cfg, http.DefaultClient, nil); err != nil {
		_ = upload.LogError(err)
	}
	return nil
}

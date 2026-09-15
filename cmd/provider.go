package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"zenget/internal/github"
	"zenget/internal/provider"
)

func newCommandProvider(cmd *cobra.Command) (provider.Provider, error) {
	baseURL, err := getOptionalStringFlag(cmd, "api-base-url")
	if err != nil {
		return nil, fmt.Errorf("read --api-base-url flag: %w", err)
	}
	forge, err := getOptionalStringFlag(cmd, "forge")
	if err != nil {
		return nil, fmt.Errorf("read --forge flag: %w", err)
	}
	client, err := github.NewConfigured(baseURL, forge)
	if err != nil {
		return nil, fmt.Errorf("configure release provider: %w", err)
	}
	return client, nil
}

package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/config"
	"zenget/internal/limits"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show or change zenget configuration",
	Long:  "Show or change zenget configuration. checksum_policy is if-present or required (default if-present; there is no off mode). lazy_install is disabled by default and enables trusted project-shim cache-miss downloads. Resource limits use finite positive int64 values; byte limits accept B, KiB, MiB, or GiB suffixes.",
	Args:  cobra.NoArgs,
	RunE:  runConfig,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Args:  cobra.ExactArgs(2),
	RunE:  runConfigSet,
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get one configuration value",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigGet,
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configuration values",
	Args:  cobra.NoArgs,
	RunE:  runConfigList,
}

func init() {
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
	rootCmd.AddCommand(configCmd)
}

func runConfig(cmd *cobra.Command, args []string) error {
	return printConfig(cmd.OutOrStdout())
}

// printConfig writes the current configuration to out.
func printConfig(out io.Writer) error {
	if out == nil {
		out = io.Discard
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	tracking := "false"
	if cfg.TrackingEnabled() {
		tracking = "true"
	}
	upload := "false"
	if cfg.UploadEnabled() {
		upload = "true"
	}
	installDir, err := cfg.InstallDirectory()
	if err != nil {
		return fmt.Errorf("resolve install_dir: %w", err)
	}
	if _, err := fmt.Fprintf(out, "usage_tracking=%s\nusage_upload=%s\ninstall_dir=%s\nchecksum_policy=%s\nlazy_install=%t\n", tracking, upload, installDir, cfg.EffectiveChecksumPolicy(), cfg.LazyInstall); err != nil {
		return err
	}
	for _, key := range limits.Keys() {
		value, ok := cfg.Limit(key)
		if !ok {
			return fmt.Errorf("unknown config limit key %q", key)
		}
		if _, err := fmt.Fprintf(out, "%s=%d\n", key, value); err != nil {
			return err
		}
	}
	return nil
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	return setConfigValue(cmd.OutOrStdout(), args[0], args[1])
}

func runConfigGet(cmd *cobra.Command, args []string) error {
	return getConfigValue(cmd.OutOrStdout(), args[0])
}

func runConfigList(cmd *cobra.Command, args []string) error {
	return printConfig(cmd.OutOrStdout())
}

// setConfigValue persists a single configuration value.
func setConfigValue(out io.Writer, key, value string) error {
	if out == nil {
		out = io.Discard
	}

	key = strings.ToLower(strings.TrimSpace(key))
	value = strings.TrimSpace(value)
	if limits.IsKey(key) {
		parsed, err := limits.ParseForKey(key, value)
		if err != nil {
			return fmt.Errorf("invalid value %q for %s: %w", value, key, err)
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if err := cfg.SetLimit(key, parsed); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}

		_, err = fmt.Fprintf(out, "%s=%d\n", key, parsed)
		return err
	}

	switch key {
	case "checksum_policy":
		if _, err := config.ParseChecksumPolicy(value); err != nil {
			return fmt.Errorf("invalid value %q for %s: %w", value, key, err)
		}
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if err := cfg.SetChecksumPolicy(value); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		_, err = fmt.Fprintf(out, "%s=%s\n", key, cfg.ChecksumPolicy)
		return err
	case "usage_tracking", "usage_upload":
		enabled, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("invalid value %q for %s: %w", value, key, err)
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		switch key {
		case "usage_tracking":
			cfg.UsageTracking = enabled
		case "usage_upload":
			cfg.UsageUpload = enabled
		}
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}

		_, err = fmt.Fprintf(out, "%s=%s\n", key, value)
		return err
	case "lazy_install":
		enabled, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("invalid value %q for %s: %w", value, key, err)
		}
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		cfg.LazyInstall = enabled
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		_, err = fmt.Fprintf(out, "%s=%s\n", key, value)
		return err
	case "install_dir":
		if err := config.ValidateInstallDir(value); err != nil {
			return err
		}

		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		cfg.InstallDir = value
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}

		expanded, err := config.ExpandInstallDir(value)
		if err != nil {
			return fmt.Errorf("resolve install_dir: %w", err)
		}
		_, err = fmt.Fprintf(out, "%s=%s\n", key, expanded)
		return err
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
}

func getConfigValue(out io.Writer, key string) error {
	if out == nil {
		out = io.Discard
	}
	key = strings.ToLower(strings.TrimSpace(key))
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	var value string
	switch key {
	case "checksum_policy":
		value = string(cfg.EffectiveChecksumPolicy())
	case "usage_tracking":
		value = fmt.Sprintf("%t", cfg.UsageTracking)
	case "usage_upload":
		value = fmt.Sprintf("%t", cfg.UsageUpload)
	case "lazy_install":
		value = fmt.Sprintf("%t", cfg.LazyInstall)
	case "install_dir":
		value, err = cfg.InstallDirectory()
		if err != nil {
			return fmt.Errorf("resolve install_dir: %w", err)
		}
	default:
		if parsed, ok := cfg.Limit(key); ok {
			value = fmt.Sprintf("%d", parsed)
		} else {
			return fmt.Errorf("unknown config key %q", key)
		}
	}
	_, err = fmt.Fprintf(out, "%s=%s\n", key, value)
	return err
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("expected true or false")
	}
}

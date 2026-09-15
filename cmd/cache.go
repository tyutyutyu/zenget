package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/artifactcache"
	"zenget/internal/install"
	"zenget/internal/projectmanifest"
	"zenget/internal/state"
	"zenget/internal/wrapper"
)

var (
	cacheListJSON   bool
	cacheGCJSON     bool
	cacheGCDryRun   bool
	cacheGCMaxAge   time.Duration
	cacheGCMaxBytes int64
)

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Inspect and clean the local artifact cache",
}

var cacheListCmd = &cobra.Command{
	Use:   "list",
	Short: "List cached extracted artifacts",
	Args:  cobra.NoArgs,
	RunE:  runCacheList,
}

var cacheGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Remove unused or damaged cached artifacts",
	Args:  cobra.NoArgs,
	RunE:  runCacheGC,
}

func init() {
	cacheListCmd.Flags().BoolVar(&cacheListJSON, "json", false, "print cache entries as JSON")
	cacheListCmd.Flags().StringArray("manifest", nil, "protect entries referenced by this manifest (repeatable)")

	cacheGCCmd.Flags().BoolVar(&cacheGCDryRun, "dry-run", false, "show the collection plan without changing the cache")
	cacheGCCmd.Flags().StringArray("manifest", nil, "protect entries referenced by this manifest (repeatable)")
	cacheGCCmd.Flags().DurationVar(&cacheGCMaxAge, "max-age", 0, "remove unprotected entries not used within this duration")
	cacheGCCmd.Flags().Int64Var(&cacheGCMaxBytes, "max-bytes", 0, "remove least-recently-used unprotected entries until this budget remains")
	cacheGCCmd.Flags().BoolVar(&cacheGCJSON, "json", false, "print the collection plan as JSON")

	cacheCmd.AddCommand(cacheListCmd, cacheGCCmd)
	rootCmd.AddCommand(cacheCmd)
}

func runCacheList(cmd *cobra.Command, _ []string) error {
	cache, err := artifactcache.New()
	if err != nil {
		return err
	}
	protection, err := cacheProtection(cmd)
	if err != nil {
		return err
	}
	entries, err := cache.List(protection)
	if err != nil {
		return err
	}
	if cacheListJSON {
		return writeJSON(cmd.OutOrStdout(), entries)
	}
	return printCacheEntries(cmd.OutOrStdout(), entries)
}

func runCacheGC(cmd *cobra.Command, _ []string) error {
	cache, err := artifactcache.New()
	if err != nil {
		return err
	}
	protection, err := cacheProtection(cmd)
	if err != nil {
		return err
	}
	maxAgeSet := cmd.Flags().Changed("max-age")
	maxBytesSet := cmd.Flags().Changed("max-bytes")
	plan, err := cache.GC(cmd.Context(), artifactcache.GCOptions{
		Protection: protection,
		MaxAge:     cacheGCMaxAge, MaxAgeSet: maxAgeSet,
		MaxBytes: cacheGCMaxBytes, MaxBytesSet: maxBytesSet,
		DryRun: cacheGCDryRun,
	})
	if err != nil {
		return err
	}
	if cacheGCJSON {
		return writeJSON(cmd.OutOrStdout(), plan)
	}
	return printCacheGCPlan(cmd.OutOrStdout(), plan)
}

func cacheProtection(cmd *cobra.Command) (artifactcache.Protection, error) {
	st, err := state.Load()
	if err != nil {
		return artifactcache.Protection{}, fmt.Errorf("load state: %w", err)
	}
	protection := protectionFromState(st)
	manifestPaths, err := cmd.Flags().GetStringArray("manifest")
	if err != nil {
		return artifactcache.Protection{}, fmt.Errorf("read --manifest flag: %w", err)
	}
	if len(manifestPaths) > 0 {
		for _, path := range manifestPaths {
			resolution, err := projectmanifest.Resolve(projectmanifest.Options{ExplicitPath: path})
			if err != nil {
				return artifactcache.Protection{}, err
			}
			protection.Manifest = append(protection.Manifest, protectionFromManifest(resolution.Manifest)...)
		}
		return protection, nil
	}

	resolution, err := projectmanifest.Resolve(projectmanifest.Options{NoDiscovery: noManifestDiscovery})
	if err != nil {
		var notFound *projectmanifest.NoManifestError
		if errors.As(err, &notFound) {
			return protection, nil
		}
		return artifactcache.Protection{}, err
	}
	protection.Manifest = append(protection.Manifest, protectionFromManifest(resolution.Manifest)...)
	return protection, nil
}

func protectionFromState(st *state.State) artifactcache.Protection {
	protection := artifactcache.Protection{}
	if st == nil {
		return protection
	}
	for repository, app := range st.Apps {
		provider := app.Provider
		if provider == "" {
			provider = wrapper.DefaultProvider
		}
		goos, goarch := app.PlatformOS, app.PlatformArch
		if goos == "" || goarch == "" {
			goos, goarch = runtime.GOOS, runtime.GOARCH
		}
		libc := install.LibcIdentity(goos)
		for _, artifact := range app.ArtifactPaths() {
			if artifact == nil {
				continue
			}
			version := artifact.Version
			if version == "" {
				version = app.Version
			}
			if version == "" {
				continue
			}
			protection.Active = append(protection.Active, artifactcache.Key{
				Provider: provider, Repository: repository, Tag: version,
				PlatformOS: goos, PlatformArch: goarch, PlatformLibc: libc,
			})
		}
	}
	return protection
}

func protectionFromManifest(file state.Manifest) []artifactcache.Key {
	keys := make([]artifactcache.Key, 0, len(file.Apps))
	for _, app := range file.Apps {
		goos, goarch := app.PlatformOS, app.PlatformArch
		if goos == "" {
			goos, goarch = runtime.GOOS, runtime.GOARCH
		}
		keys = append(keys, artifactcache.Key{
			Provider: app.Provider, Repository: app.Repository, Tag: app.Tag,
			PlatformOS: goos, PlatformArch: goarch, PlatformLibc: install.LibcIdentity(goos),
			AssetName: app.Asset, ArchiveBinary: app.ArchiveBinary, TargetName: app.TargetName,
		})
	}
	return keys
}

func printCacheEntries(out io.Writer, entries []artifactcache.Inspection) error {
	if out == nil {
		out = io.Discard
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(out, "No cached artifacts.")
		return err
	}
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "IDENTITY\tSIZE\tINTEGRITY\tPROTECTION"); err != nil {
		return err
	}
	for _, entry := range entries {
		protection := "-"
		switch {
		case entry.Active && entry.Manifest:
			protection = "active,manifest"
		case entry.Active:
			protection = "active"
		case entry.Manifest:
			protection = "manifest"
		case entry.Protected:
			protection = "protected"
		}
		if _, err := fmt.Fprintf(writer, "%s\t%d\t%s\t%s\n", entry.Identity, entry.Size, entry.Integrity, protection); err != nil {
			return err
		}
		if entry.Reason != "" {
			if _, err := fmt.Fprintf(writer, "  reason: %s\t\t\t\n", entry.Reason); err != nil {
				return err
			}
		}
	}
	return writer.Flush()
}

func printCacheGCPlan(out io.Writer, plan artifactcache.GCPlan) error {
	if out == nil {
		out = io.Discard
	}
	if plan.DryRun {
		if _, err := fmt.Fprintln(out, "Cache GC dry-run; no files were changed."); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(out, "Cache GC completed."); err != nil {
		return err
	}
	for _, entry := range plan.DeleteEntries {
		if _, err := fmt.Fprintf(out, "remove artifact: %s (%d bytes)\n", entry.Identity, entry.Size); err != nil {
			return err
		}
	}
	for _, path := range plan.DeleteStaging {
		if _, err := fmt.Fprintf(out, "remove staging: %s\n", path); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "Artifacts: %d, staging: %d, reclaimed: %d bytes, remaining: %d bytes\n",
		len(plan.DeleteEntries), len(plan.DeleteStaging), plan.ReclaimedBytes, plan.RemainingBytes)
	return err
}

func writeJSON(out io.Writer, value any) error {
	if out == nil {
		out = io.Discard
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	_, err = fmt.Fprintf(out, "%s\n", data)
	return err
}

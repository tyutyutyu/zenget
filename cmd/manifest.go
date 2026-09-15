package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/appcfg"
	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/install"
	"zenget/internal/lockfile"
	manifestfile "zenget/internal/manifest"
	"zenget/internal/projectmanifest"
	"zenget/internal/state"
)

const manifestProviderGitHub = manifestfile.ProviderGitHub

var (
	exportForce           bool
	applyDryRun           bool
	applyLockPath         string
	manifestPathJSON      bool
	manifestClientFactory = newManifestClient
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export portable installation intentions",
	Args:  cobra.NoArgs,
	RunE:  runExport,
}

var applyCmd = &cobra.Command{
	Use:   "apply [manifest-file]",
	Short: "Apply a local installation manifest",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runApply,
}

var manifestCmd = &cobra.Command{
	Use:   "manifest",
	Short: "Inspect portable manifests",
}

var manifestPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Show the selected project manifest path",
	Args:  cobra.NoArgs,
	RunE:  runManifestPath,
}

func init() {
	exportCmd.Flags().StringP("output", "o", "", "Write the manifest to a local file instead of stdout")
	exportCmd.Flags().BoolVar(&exportForce, "force", false, "Allow replacing an existing output file")
	applyCmd.Flags().BoolVar(&applyDryRun, "dry-run", false, "Resolve and print the plan without changing files")
	applyCmd.Flags().StringVar(&applyLockPath, "lock", "", "Require and verify a local artifact lockfile")
	applyCmd.Flags().String("manifest", "", "Manifest path override; defaults to project discovery")
	manifestPathCmd.Flags().String("manifest", "", "Manifest path override; defaults to project discovery")
	manifestPathCmd.Flags().BoolVar(&manifestPathJSON, "json", false, "print path diagnostics as JSON")
	manifestCmd.AddCommand(manifestPathCmd)
	rootCmd.AddCommand(exportCmd)
	rootCmd.AddCommand(applyCmd)
	rootCmd.AddCommand(manifestCmd)
}

func runExport(cmd *cobra.Command, args []string) error {
	output, err := getOptionalStringFlag(cmd, "output")
	if err != nil {
		return fmt.Errorf("read --output flag: %w", err)
	}
	if output != "" && !isLocalManifestPath(output) {
		return fmt.Errorf("manifest output must be a local path")
	}

	manifest, err := buildManifest()
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}
	data, err := marshalManifest(manifest)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}

	if output == "" {
		writer := cmd.OutOrStdout()
		if writer == nil {
			writer = io.Discard
		}
		if _, err := writer.Write(data); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}
		return nil
	}
	if err := writeManifestFile(output, data, exportForce); err != nil {
		return err
	}
	return nil
}

func buildManifest() (state.Manifest, error) {
	registry, err := state.Load()
	if err != nil {
		return state.Manifest{}, fmt.Errorf("load state: %w", err)
	}

	repositories := make([]string, 0, len(registry.Apps))
	for repository := range registry.Apps {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	manifest := state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		SchemaURL:     manifestfile.SchemaURL,
		Apps:          make([]state.ManifestApp, 0, len(repositories)),
	}
	for _, repository := range repositories {
		app := registry.Apps[repository]
		configuration, err := appcfg.Load(repository)
		if err != nil {
			return state.Manifest{}, fmt.Errorf("load app config for %s: %w", repository, err)
		}
		providerName := app.Provider
		if providerName == "" {
			providerName = manifestProviderGitHub
		}
		manifest.Apps = append(manifest.Apps, state.ManifestApp{
			Repository:    repository,
			Provider:      providerName,
			Tag:           app.Version,
			Asset:         configuration.Asset,
			PlatformOS:    app.PlatformOS,
			PlatformArch:  app.PlatformArch,
			ArchiveBinary: configuration.ArchiveBinary,
			TargetName:    configuration.TargetName,
		})
	}
	if err := validateManifest(manifest); err != nil {
		return state.Manifest{}, err
	}
	return manifest, nil
}

func marshalManifest(manifest state.Manifest) ([]byte, error) {
	return manifestfile.Marshal(manifest)
}

func writeManifestFile(output string, data []byte, overwrite bool) error {
	return manifestfile.WriteFile(output, data, overwrite)
}

func runApply(cmd *cobra.Command, args []string) error {
	explicitPath, err := getOptionalStringFlag(cmd, "manifest")
	if err != nil {
		return fmt.Errorf("read --manifest flag: %w", err)
	}
	if len(args) == 1 {
		if explicitPath != "" {
			return fmt.Errorf("manifest path provided both as an argument and with --manifest")
		}
		explicitPath = args[0]
	}
	resolution, err := resolveProjectManifest(explicitPath)
	if err != nil {
		return err
	}
	manifest := resolution.Manifest
	lockPath, err := getOptionalStringFlag(cmd, "lock")
	if err != nil {
		return fmt.Errorf("read --lock flag: %w", err)
	}
	var locked *lockfile.Lockfile
	if lockPath != "" {
		if !isLocalManifestPath(lockPath) {
			return fmt.Errorf("lockfile must be a local path")
		}
		loaded, err := lockfile.LoadFile(lockPath)
		if err != nil {
			return err
		}
		locked = &loaded
	}
	checksumPolicy, err := resolveInstallChecksumPolicy(installOptions{})
	if err != nil {
		return err
	}
	client, err := manifestClientFactory(manifestProvider(manifest))
	if err != nil {
		return err
	}
	registry, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	plan := buildApplyPlan(cmd.Context(), client, registry, manifest)
	for i := range plan {
		plan[i].checksumPolicy = checksumPolicy
	}
	writer := cmd.OutOrStdout()
	if writer == nil {
		writer = io.Discard
	}
	if locked != nil {
		if err := validateLockAgainstPlan(manifest, plan, *locked); err != nil {
			if printErr := printApplyPlan(writer, plan); printErr != nil {
				return printErr
			}
			return err
		}
		if applyDryRun {
			if err := printApplyPlan(writer, plan); err != nil {
				return err
			}
			if err := printApplyChecksumPolicyPending(writer, plan); err != nil {
				return err
			}
			if err := printLockedContentPending(writer, plan); err != nil {
				return err
			}
			return applyPlanError(plan)
		}
		staged, err := preflightLockedAssets(cmd.Context(), client, plan, *locked)
		if err != nil {
			return err
		}
		defer cleanupStagedAssets(staged)
		return executeApplyPlanWithStaged(cmd.Context(), client, plan, writer, staged)
	}
	if applyDryRun {
		if err := printApplyPlan(writer, plan); err != nil {
			return err
		}
		if err := printApplyChecksumPolicyPending(writer, plan); err != nil {
			return err
		}
		return applyPlanError(plan)
	}
	return executeApplyPlan(cmd.Context(), client, plan, writer)
}

func loadManifest(manifestPath string) (state.Manifest, error) {
	return manifestfile.Load(manifestPath)
}

func resolveProjectManifest(explicitPath string) (projectmanifest.Resolution, error) {
	return projectmanifest.Resolve(projectmanifest.Options{
		ExplicitPath: explicitPath,
		NoDiscovery:  noManifestDiscovery,
	})
}

func validateManifest(manifest state.Manifest) error {
	return manifestfile.Validate(manifest)
}

func isLocalManifestPath(value string) bool {
	return manifestfile.IsLocalPath(value)
}

type manifestPathReport struct {
	Path          string `json:"path"`
	Source        string `json:"source"`
	SchemaVersion int    `json:"schema_version"`
}

func runManifestPath(cmd *cobra.Command, args []string) error {
	explicitPath, err := getOptionalStringFlag(cmd, "manifest")
	if err != nil {
		return fmt.Errorf("read --manifest flag: %w", err)
	}
	resolution, err := resolveProjectManifest(explicitPath)
	if err != nil {
		var notFound *projectmanifest.NoManifestError
		if errors.As(err, &notFound) {
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
		}
		return err
	}
	report := manifestPathReport{
		Path:          resolution.Path,
		Source:        string(resolution.Source),
		SchemaVersion: resolution.Manifest.SchemaVersion,
	}
	writer := cmd.OutOrStdout()
	if writer == nil {
		writer = io.Discard
	}
	if manifestPathJSON {
		if err := json.NewEncoder(writer).Encode(report); err != nil {
			return fmt.Errorf("write manifest path: %w", err)
		}
		return nil
	}
	_, err = fmt.Fprintf(writer, "Manifest path: %s\nSource: %s\nSchema version: %d\n", report.Path, report.Source, report.SchemaVersion)
	return err
}

func manifestProvider(manifest state.Manifest) string {
	if len(manifest.Apps) == 0 {
		return manifestProviderGitHub
	}
	return manifest.Apps[0].Provider
}

func newManifestClient(providerName string) (*github.Client, error) {
	if providerName != manifestProviderGitHub {
		return nil, fmt.Errorf("unsupported provider %q", providerName)
	}
	return github.New(), nil
}

type applyStatus string

const (
	applyInstall   applyStatus = "install"
	applyRestore   applyStatus = "restore"
	applyUnchanged applyStatus = "unchanged"
	applyFailed    applyStatus = "failed"
)

type applyPlanEntry struct {
	manifest       state.ManifestApp
	release        *github.Release
	asset          github.Asset
	checksumPolicy config.ChecksumPolicy
	goos           string
	goarch         string
	status         applyStatus
	reason         string
}

func buildApplyPlan(ctx context.Context, client *github.Client, registry *state.State, manifest state.Manifest) []applyPlanEntry {
	if ctx == nil {
		ctx = context.Background()
	}
	plan := make([]applyPlanEntry, len(manifest.Apps))
	for index, entry := range manifest.Apps {
		plan[index].manifest = entry
		plan[index].status = applyFailed
		if err := ctx.Err(); err != nil {
			plan[index].reason = err.Error()
			continue
		}
		goos, goarch := entry.PlatformOS, entry.PlatformArch
		if goos == "" {
			goos, goarch = runtime.GOOS, runtime.GOARCH
		}
		plan[index].goos = goos
		plan[index].goarch = goarch

		configuration, err := appcfg.Load(entry.Repository)
		if err != nil {
			plan[index].reason = fmt.Sprintf("load app config: %v", err)
			continue
		}
		release, err := client.ReleaseByTag(ctx, repositoryParts(entry.Repository), repositoryName(entry.Repository), entry.Tag)
		if err != nil {
			plan[index].reason = fmt.Sprintf("resolve release: %v", err)
			continue
		}
		if release == nil || release.TagName != entry.Tag {
			plan[index].reason = fmt.Sprintf("provider returned tag %q for requested tag %q", releaseTag(release), entry.Tag)
			continue
		}
		asset, err := resolveManifestAsset(release, entry, goos, goarch)
		if err != nil {
			plan[index].reason = fmt.Sprintf("resolve asset: %v", err)
			continue
		}
		plan[index].release = release
		plan[index].asset = asset
		if app, ok := registry.Get(entry.Repository); !ok {
			plan[index].status = applyInstall
		} else if app.Version == entry.Tag && appHealthy(app) && applyChoicesMatch(app, configuration, entry, goos, goarch) {
			plan[index].status = applyUnchanged
		} else if app.Version == entry.Tag {
			plan[index].status = applyRestore
		} else {
			plan[index].status = applyInstall
		}
	}
	return plan
}

func repositoryParts(repository string) string {
	org, _, _ := strings.Cut(repository, "/")
	return org
}

func repositoryName(repository string) string {
	_, repo, _ := strings.Cut(repository, "/")
	return repo
}

func releaseTag(release *github.Release) string {
	if release == nil {
		return "<nil>"
	}
	return release.TagName
}

func resolveManifestAsset(release *github.Release, entry state.ManifestApp, goos, goarch string) (github.Asset, error) {
	asset, err := install.SelectAsset(release, goos, goarch, entry.Asset)
	if err != nil {
		return github.Asset{}, err
	}
	if entry.Asset != "" && asset.Name != entry.Asset {
		return github.Asset{}, fmt.Errorf("asset choice %q resolved to %q; manifest choices must use an exact asset name", entry.Asset, asset.Name)
	}
	return asset, nil
}

func applyChoicesMatch(app state.App, configuration *appcfg.AppConfig, entry state.ManifestApp, goos, goarch string) bool {
	if app.Provider != "" && app.Provider != entry.Provider {
		return false
	}
	if entry.PlatformOS != "" && (app.PlatformOS != goos || app.PlatformArch != goarch) {
		return false
	}
	if entry.Asset != "" && configuration.Asset != entry.Asset {
		return false
	}
	if entry.ArchiveBinary != "" && configuration.ArchiveBinary != entry.ArchiveBinary {
		return false
	}
	if entry.TargetName != "" && configuration.TargetName != entry.TargetName {
		return false
	}
	return true
}

func printApplyPlan(out io.Writer, plan []applyPlanEntry) error {
	counts := map[applyStatus]int{}
	for _, entry := range plan {
		counts[entry.status]++
		if entry.status == applyFailed {
			if _, err := fmt.Fprintf(out, "%s %s: %s\n", entry.status, entry.manifest.Repository, entry.reason); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(out, "%s %s %s\n", entry.status, entry.manifest.Repository, entry.manifest.Tag); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "Apply summary: %d install, %d restore, %d unchanged, %d failed\n", counts[applyInstall], counts[applyRestore], counts[applyUnchanged], counts[applyFailed])
	return err
}

func printApplyChecksumPolicyPending(out io.Writer, plan []applyPlanEntry) error {
	for _, entry := range plan {
		if (entry.status != applyInstall && entry.status != applyRestore) || entry.release == nil || entry.asset.Name == "" {
			continue
		}
		if _, err := fmt.Fprintf(out, "checksum verification pending: policy=%s repository=%s release=%s asset=%q\n", entry.checksumPolicy, entry.manifest.Repository, entry.release.TagName, entry.asset.Name); err != nil {
			return err
		}
	}
	return nil
}

func executeApplyPlan(ctx context.Context, client *github.Client, plan []applyPlanEntry, out io.Writer) error {
	return executeApplyPlanWithStaged(ctx, client, plan, out, nil)
}

func executeApplyPlanWithStaged(ctx context.Context, client *github.Client, plan []applyPlanEntry, out io.Writer, staged map[string]string) error {
	for index := range plan {
		entry := &plan[index]
		if entry.status == applyFailed {
			if _, err := fmt.Fprintf(out, "%s %s: %s\n", entry.status, entry.manifest.Repository, entry.reason); err != nil {
				return err
			}
			continue
		}
		if entry.status == applyUnchanged {
			if _, err := fmt.Fprintf(out, "%s %s %s\n", entry.status, entry.manifest.Repository, entry.manifest.Tag); err != nil {
				return err
			}
			continue
		}
		stagedPath := ""
		assetVerified := false
		if staged != nil {
			stagedPath = staged[entry.manifest.Repository]
			assetVerified = stagedPath != ""
		}
		err := runInstallWithOptions(ctx, client, entry.manifest.Repository, installOptions{
			assetOverride:     entry.asset.Name,
			assetPath:         stagedPath,
			assetVerified:     assetVerified,
			binName:           entry.manifest.ArchiveBinary,
			targetName:        entry.manifest.TargetName,
			tag:               entry.manifest.Tag,
			release:           entry.release,
			goos:              entry.goos,
			goarch:            entry.goarch,
			checksumPolicy:    string(entry.checksumPolicy),
			checksumPolicySet: entry.checksumPolicy != "",
			input:             strings.NewReader(""),
			force:             entry.status == applyRestore,
			exactTag:          true,
		}, io.Discard)
		if err != nil {
			entry.status = applyFailed
			entry.reason = err.Error()
			if _, writeErr := fmt.Fprintf(out, "%s %s: %s\n", entry.status, entry.manifest.Repository, entry.reason); writeErr != nil {
				return writeErr
			}
			continue
		}
		if _, err := fmt.Fprintf(out, "%s %s %s\n", entry.status, entry.manifest.Repository, entry.manifest.Tag); err != nil {
			return err
		}
	}
	if err := printApplySummary(out, plan); err != nil {
		return err
	}
	return applyPlanError(plan)
}

func printApplySummary(out io.Writer, plan []applyPlanEntry) error {
	counts := map[applyStatus]int{}
	for _, entry := range plan {
		counts[entry.status]++
	}
	_, err := fmt.Fprintf(out, "Apply summary: %d install, %d restore, %d unchanged, %d failed\n", counts[applyInstall], counts[applyRestore], counts[applyUnchanged], counts[applyFailed])
	return err
}

func applyPlanError(plan []applyPlanEntry) error {
	failed := 0
	for _, entry := range plan {
		if entry.status == applyFailed {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("apply failed for %d application(s)", failed)
	}
	return nil
}

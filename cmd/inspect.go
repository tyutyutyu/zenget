package cmd

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/appcfg"
	"zenget/internal/github"
	"zenget/internal/install"
	"zenget/internal/provider"
)

var inspectJSON bool

var inspectCmd = &cobra.Command{
	Use:   "inspect <org/repo>",
	Short: "Preview release and asset selection without downloading",
	Args:  cobra.ExactArgs(1),
	RunE:  runInspect,
}

func init() {
	inspectCmd.Flags().String("asset", "", "Explicitly select a release asset by exact name or substring")
	inspectCmd.Flags().String("asset-match", "", "Select assets whose names contain this reusable case-insensitive substring")
	inspectCmd.Flags().String("asset-match-regex", "", "Select assets whose names match this reusable RE2 regular expression")
	inspectCmd.Flags().String("tag", "", "Inspect a specific release tag; falls back to the most recent tag containing the string")
	inspectCmd.Flags().Int("min-age-days", 0, "Only inspect releases at least N days old; cannot be combined with --tag")
	inspectCmd.Flags().String("system", "", "Target platform as os/arch (e.g. linux/amd64, darwin/arm64)")
	inspectCmd.Flags().BoolVar(&inspectJSON, "json", false, "Print the inspection report as JSON")
	inspectCmd.Flags().String("api-base-url", "", "Release API base URL (e.g. GitHub Enterprise /api/v3, Forgejo /api/v1, GitLab /api/v4)")
	inspectCmd.Flags().String("forge", "", "Release forge: github, gitlab, or forgejo; auto-detected from API host/path when omitted")
	rootCmd.AddCommand(inspectCmd)
}

type inspectOptions struct {
	assetOverride   string
	assetMatch      string
	assetMatchRegex string
	tag             string
	minAgeDays      int
	goos            string
	goarch          string
	asJSON          bool
}

type inspectReport struct {
	Repository      string                    `json:"repository"`
	Release         inspectReleaseReport      `json:"release"`
	Target          inspectTargetReport       `json:"target"`
	Status          string                    `json:"status"`
	SelectedAsset   *inspectAssetReport       `json:"selected_asset"`
	SelectionSource string                    `json:"selection_source"`
	Selector        inspectSelectorReport     `json:"selector"`
	Reason          string                    `json:"reason"`
	Assets          []install.AssetDiagnostic `json:"assets"`
}

type inspectSelectorReport struct {
	Source   string   `json:"source"`
	Type     string   `json:"type"`
	Pattern  string   `json:"pattern"`
	Excluded []string `json:"excluded"`
	Decision string   `json:"decision"`
}

type inspectReleaseReport struct {
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"published_at"`
	AgeDays     int       `json:"age_days"`
	AgeFiltered bool      `json:"age_filtered"`
}

type inspectTargetReport struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Libc string `json:"libc"`
}

type inspectAssetReport struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	Format string `json:"format"`
}

func runInspect(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || !repoPattern.MatchString(args[0]) {
		return fmt.Errorf("invalid repository %q: expected org/repo", firstArg(args))
	}

	assetOverride, err := getOptionalStringFlag(cmd, "asset")
	if err != nil {
		return fmt.Errorf("read --asset flag: %w", err)
	}
	assetMatch, err := getOptionalStringFlag(cmd, "asset-match")
	if err != nil {
		return fmt.Errorf("read --asset-match flag: %w", err)
	}
	assetMatchRegex, err := getOptionalStringFlag(cmd, "asset-match-regex")
	if err != nil {
		return fmt.Errorf("read --asset-match-regex flag: %w", err)
	}
	tag, err := getOptionalStringFlag(cmd, "tag")
	if err != nil {
		return fmt.Errorf("read --tag flag: %w", err)
	}
	minAgeDays := 0
	minAgeChanged := false
	if cmd != nil && cmd.Flags().Lookup("min-age-days") != nil {
		minAgeDays, err = cmd.Flags().GetInt("min-age-days")
		if err != nil {
			return fmt.Errorf("read --min-age-days flag: %w", err)
		}
		minAgeChanged = cmd.Flags().Changed("min-age-days")
	}
	if err := validateReleaseSelection(tag, minAgeDays, minAgeChanged); err != nil {
		return err
	}
	if _, err := assetSelectorFromInstallOptions(installOptions{
		assetOverride:      assetOverride,
		assetOverrideSet:   flagChanged(cmd, "asset"),
		assetMatch:         assetMatch,
		assetMatchSet:      flagChanged(cmd, "asset-match"),
		assetMatchRegex:    assetMatchRegex,
		assetMatchRegexSet: flagChanged(cmd, "asset-match-regex"),
	}); err != nil {
		return err
	}

	system, err := getOptionalStringFlag(cmd, "system")
	if err != nil {
		return fmt.Errorf("read --system flag: %w", err)
	}
	goos, goarch, err := parseSystem(system)
	if err != nil {
		return err
	}

	asJSON := inspectJSON
	if cmd != nil && cmd.Flags().Lookup("json") != nil {
		asJSON, err = cmd.Flags().GetBool("json")
		if err != nil {
			return fmt.Errorf("read --json flag: %w", err)
		}
	}
	client, err := newCommandProvider(cmd)
	if err != nil {
		return err
	}
	err = runInspectWithClient(cmd.Context(), client, args[0], inspectOptions{
		assetOverride:   assetOverride,
		assetMatch:      assetMatch,
		assetMatchRegex: assetMatchRegex,
		tag:             tag,
		minAgeDays:      minAgeDays,
		goos:            goos,
		goarch:          goarch,
		asJSON:          asJSON,
	}, cmd.OutOrStdout(), time.Now().UTC())
	if err != nil {
		// Expected selection failures already produced a complete report. Keep
		// Cobra from appending usage or a second error line to JSON output.
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
	}
	return err
}

// runInspectWithClient performs a metadata-only inspection using client. It
// reads the persisted asset choice but never writes state, app configuration,
// wrappers, or downloaded files. now is injectable for deterministic reports.
func runInspectWithClient(ctx context.Context, client provider.Provider, repository string, options inspectOptions, out io.Writer, now time.Time) error {
	if !repoPattern.MatchString(repository) {
		return fmt.Errorf("invalid repository %q: expected org/repo", repository)
	}
	explicitSelector, err := assetSelectorFromInstallOptions(installOptions{
		assetOverride:   options.assetOverride,
		assetMatch:      options.assetMatch,
		assetMatchRegex: options.assetMatchRegex,
	})
	if err != nil {
		return err
	}
	if err := validateReleaseSelection(options.tag, options.minAgeDays, false); err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("GitHub client is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if options.goos == "" {
		options.goos = runtime.GOOS
	}
	if options.goarch == "" {
		options.goarch = runtime.GOARCH
	}
	if !install.ValidPlatform(options.goos, options.goarch) {
		return fmt.Errorf("unsupported inspect target %s/%s", options.goos, options.goarch)
	}

	org, repo, _ := strings.Cut(repository, "/")
	resolutionOptions := installOptions{tag: options.tag, minAgeDays: options.minAgeDays}
	release, ageFiltered, err := resolveInstallRelease(ctx, client, org, repo, resolutionOptions)
	if err != nil {
		return releaseResolutionError(repository, resolutionOptions, err)
	}
	if release == nil {
		return fmt.Errorf("release provider returned no release for %s", repository)
	}

	applicationConfig, err := appcfg.Load(repository)
	if err != nil {
		return fmt.Errorf("load app config: %w", err)
	}
	if applicationConfig == nil {
		applicationConfig = &appcfg.AppConfig{}
	}

	selectionSource := "automatic"
	selectionOverride := ""
	selectionSelector := explicitSelector
	if options.assetOverride != "" {
		selectionSource = "explicit"
		selectionOverride = options.assetOverride
	} else if explicitSelector != nil {
		selectionSource = "explicit"
	} else if applicationConfig.AssetSelector != nil {
		selectionSource = "persisted"
		selectionSelector = applicationConfig.AssetSelector
	} else if applicationConfig.Asset != "" {
		selectionSource = "persisted"
		selectionOverride = applicationConfig.Asset
	}

	var selection install.AssetSelection
	var selectionErr error
	if selectionSelector != nil {
		selection, selectionErr = install.AnalyzeAssetSelectionWithSelector(release, options.goos, options.goarch, selectionSelector)
	} else {
		selection, selectionErr = install.AnalyzeAssetSelection(release, options.goos, options.goarch, selectionOverride)
	}
	report := inspectReport{
		Repository: repository,
		Release: inspectReleaseReport{
			Tag:         release.TagName,
			PublishedAt: release.PublishedAt,
			AgeDays:     inspectAgeDays(release.PublishedAt, now),
			AgeFiltered: ageFiltered,
		},
		Target:          inspectTargetReport{OS: options.goos, Arch: options.goarch, Libc: install.TargetLibc(options.goos)},
		Status:          inspectSelectionStatus(release, selectionSource, options.assetOverride, selectionErr),
		SelectionSource: selectionSource,
		Selector:        newInspectSelectorReport(selectionSource, selectionOverride, selectionSelector, selection, selectionErr),
		Assets:          selection.Assets,
	}
	if selection.SelectedAsset != nil {
		report.SelectedAsset = newInspectAssetReport(*selection.SelectedAsset, selection.Assets)
	}
	report.Reason = inspectSelectionReason(applicationConfig.Asset, options.assetOverride, selectionSource, selectionSelector, selection, selectionErr)

	if err := renderInspectReport(out, report, options.asJSON); err != nil {
		return err
	}
	if selectionErr != nil {
		if selectionSource == "persisted" && options.assetOverride == "" {
			if selectionSelector != nil {
				return fmt.Errorf("saved asset selector %s %q is no longer available: %w", selectionSelector.Type, selectionSelector.Pattern, selectionErr)
			}
			return fmt.Errorf("saved asset choice %q is no longer available: %w", applicationConfig.Asset, selectionErr)
		}
		return selectionErr
	}
	return nil
}

func inspectAgeDays(publishedAt, now time.Time) int {
	if publishedAt.IsZero() || now.Before(publishedAt) {
		return 0
	}
	return int(now.Sub(publishedAt).Hours() / 24)
}

func inspectSelectionStatus(release *github.Release, source, explicit string, selectionErr error) string {
	if selectionErr == nil {
		return "selected"
	}
	if release != nil && len(release.Assets) == 0 {
		return "empty_release"
	}
	if source == "persisted" && explicit == "" {
		return "stale_choice"
	}
	message := selectionErr.Error()
	switch {
	case strings.Contains(message, "ambiguous assets matching"):
		return "ambiguous"
	case strings.Contains(message, "no asset matching") || strings.Contains(message, "no assets matching"):
		return "no_match"
	case strings.Contains(message, "no compatible") || strings.Contains(message, "none match") || strings.Contains(message, "no downloadable assets"):
		return "no_compatible_asset"
	default:
		return "selection_error"
	}
}

func inspectSelectionReason(saved, explicit, source string, selector *appcfg.AssetSelector, selection install.AssetSelection, selectionErr error) string {
	if selectionErr != nil {
		if source == "persisted" && explicit == "" {
			if selector != nil {
				return fmt.Sprintf("saved asset selector %s %q is stale: %v", selector.Type, selector.Pattern, selectionErr)
			}
			return fmt.Sprintf("saved asset choice %q is stale: %v", saved, selectionErr)
		}
		return selectionErr.Error()
	}
	if selection.SelectedAsset == nil {
		return "no asset selected"
	}
	for _, asset := range selection.Assets {
		if asset.ID == selection.SelectedAsset.ID && asset.Name == selection.SelectedAsset.Name && asset.Size == selection.SelectedAsset.Size && asset.Digest == selection.SelectedAsset.Digest {
			switch source {
			case "explicit":
				if selector != nil {
					return fmt.Sprintf("selected by explicit %s selector %q; %s", selector.Type, selector.Pattern, asset.Reason)
				}
				return "selected by explicit --asset override; " + asset.Reason
			case "persisted":
				if selector != nil {
					return fmt.Sprintf("selected by persisted %s selector %q; %s", selector.Type, selector.Pattern, asset.Reason)
				}
				return "selected by persisted app configuration; " + asset.Reason
			default:
				return asset.Reason
			}
		}
	}
	return "selected asset metadata is not present in diagnostics"
}

func newInspectSelectorReport(source, concrete string, selector *appcfg.AssetSelector, selection install.AssetSelection, selectionErr error) inspectSelectorReport {
	report := inspectSelectorReport{Source: source}
	if selector != nil {
		report.Type = string(selector.Type)
		report.Pattern = selector.Pattern
	} else if concrete != "" {
		report.Type = "asset"
		report.Pattern = concrete
	}
	for _, asset := range selection.Assets {
		if asset.Disposition == install.DispositionSelectorExcluded {
			report.Excluded = append(report.Excluded, asset.Name)
		}
	}
	if selection.SelectedAsset != nil {
		report.Decision = selection.SelectedAsset.Name
	} else if selectionErr != nil {
		report.Decision = "error"
	}
	return report
}

func newInspectAssetReport(asset github.Asset, diagnostics []install.AssetDiagnostic) *inspectAssetReport {
	format := "raw"
	for _, diagnostic := range diagnostics {
		if diagnostic.ID == asset.ID && diagnostic.Name == asset.Name && diagnostic.Size == asset.Size && diagnostic.Digest == asset.Digest {
			format = diagnostic.Format
			break
		}
	}
	return &inspectAssetReport{ID: asset.ID, Name: asset.Name, Size: asset.Size, Digest: asset.Digest, Format: format}
}

func renderInspectReport(out io.Writer, report inspectReport, asJSON bool) error {
	if asJSON {
		return writeJSON(out, report)
	}
	return printInspectReport(out, report)
}

func printInspectReport(out io.Writer, report inspectReport) error {
	if out == nil {
		out = io.Discard
	}
	published := "(unknown)"
	if !report.Release.PublishedAt.IsZero() {
		published = report.Release.PublishedAt.Format(time.RFC3339)
	}
	selected := "(none)"
	if report.SelectedAsset != nil {
		selected = fmt.Sprintf("%s (%s)", report.SelectedAsset.Name, report.SelectedAsset.Format)
	}
	if _, err := fmt.Fprintf(out,
		"Repository: %s\nRelease: %s\nPublished: %s\nAge: %d days\nTarget: %s/%s (libc: %s)\nStatus: %s\nSelection source: %s\nSelected asset: %s\nReason: %s\n\nAssets:\n",
		report.Repository, report.Release.Tag, published, report.Release.AgeDays,
		report.Target.OS, report.Target.Arch, report.Target.Libc, report.Status,
		report.SelectionSource, selected, report.Reason,
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Selector: source=%s type=%s pattern=%q decision=%s\n", report.Selector.Source, report.Selector.Type, report.Selector.Pattern, report.Selector.Decision); err != nil {
		return err
	}
	if len(report.Selector.Excluded) > 0 {
		if _, err := fmt.Fprintf(out, "Selector excluded: %s\n", strings.Join(report.Selector.Excluded, ", ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}

	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "NAME\tFORMAT\tDISPOSITION\tREASON\tCHECKSUM EVIDENCE"); err != nil {
		return err
	}
	for _, asset := range report.Assets {
		evidence := strings.Join(asset.ChecksumEvidence, ", ")
		if evidence == "" {
			evidence = "-"
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", asset.Name, asset.Format, asset.Disposition, asset.Reason, evidence); err != nil {
			return err
		}
	}
	return table.Flush()
}

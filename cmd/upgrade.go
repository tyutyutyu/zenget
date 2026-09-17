package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"zenget/internal/appcfg"
	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/install"
	"zenget/internal/state"
)

const upgradeLookupWorkers = 4

var (
	upgradeDryRun bool
	upgradeYes    bool
)

var upgradeCmd = &cobra.Command{
	Use:     "upgrade [org/repo...]",
	Aliases: []string{"up"},
	Short:   "Upgrade installed applications",
	Args:    validateUpgradeArgs,
	RunE:    runUpgrade,
}

func init() {
	rootCmd.AddCommand(upgradeCmd)
	upgradeCmd.Flags().BoolVar(&upgradeDryRun, "dry-run", false, "resolve and print the upgrade plan without changing anything")
	upgradeCmd.Flags().BoolVarP(&upgradeYes, "yes", "y", false, "skip the confirmation prompt")
}

func validateUpgradeArgs(cmd *cobra.Command, args []string) error {
	return validateUpgradeRepositories(args)
}

func validateUpgradeRepositories(repositories []string) error {
	for _, repository := range repositories {
		if !repoPattern.MatchString(repository) {
			return fmt.Errorf("invalid repository %q: expected org/repo", repository)
		}
	}
	return nil
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	return runUpgradeWithOptions(cmd.Context(), github.New(), args, upgradeOptions{
		dryRun:      upgradeDryRun,
		yes:         upgradeYes,
		input:       cmd.InOrStdin(),
		interactive: stdinIsTerminal(),
	}, cmd.OutOrStdout())
}

type upgradeOptions struct {
	dryRun      bool
	yes         bool
	input       io.Reader
	interactive bool
}

type upgradeStatus string

const (
	upgradePlanned   upgradeStatus = "update"
	upgradeUpdated   upgradeStatus = "updated"
	upgradeUnchanged upgradeStatus = "unchanged"
	upgradeSkipped   upgradeStatus = "skipped"
	upgradeFailed    upgradeStatus = "failed"
)

type upgradePlanEntry struct {
	repository     string
	app            state.App
	release        *github.Release
	current        string
	target         string
	assetName      string
	assetSelector  *appcfg.AssetSelector
	archiveBinary  string
	targetName     string
	goos           string
	goarch         string
	checksumPolicy config.ChecksumPolicy
	status         upgradeStatus
	reason         string
}

type upgradeLookup struct {
	repository string
	release    *github.Release
	err        error
}

// runUpgradeWithOptions resolves an ordered plan and executes its install
// entries sequentially. Release lookups are bounded and parallel, while all
// filesystem mutations happen in repository order.
func runUpgradeWithOptions(ctx context.Context, client *github.Client, repositories []string, options upgradeOptions, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return fmt.Errorf("GitHub client is nil")
	}
	if out == nil {
		out = io.Discard
	}
	if options.input == nil {
		options.input = strings.NewReader("")
	}
	if err := validateUpgradeRepositories(repositories); err != nil {
		return err
	}
	checksumPolicy, err := resolveInstallChecksumPolicy(installOptions{})
	if err != nil {
		return err
	}

	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	selected := selectUpgradeRepositories(st, repositories)
	plan := buildUpgradePlan(ctx, client, st, selected)
	for i := range plan {
		plan[i].checksumPolicy = checksumPolicy
	}
	if err := printUpgradePlan(out, plan); err != nil {
		return err
	}

	if options.dryRun {
		if err := printUpgradeChecksumPolicyPending(out, plan); err != nil {
			return err
		}
		if err := printUpgradeSummary(out, plan, true); err != nil {
			return err
		}
		return upgradePlanError(plan)
	}

	planned := countUpgradeStatus(plan, upgradePlanned)
	if planned > 0 && !options.yes {
		if !options.interactive {
			return fmt.Errorf("upgrade plan would modify %d application(s); rerun with --yes in non-interactive mode", planned)
		}
		confirmed, err := confirmUpgrade(options.input, out)
		if err != nil {
			return err
		}
		if !confirmed {
			return fmt.Errorf("upgrade cancelled")
		}
	}

	for i := range plan {
		entry := &plan[i]
		if entry.status != upgradePlanned {
			continue
		}
		if err := ctx.Err(); err != nil {
			markRemainingUpgradeFailures(plan[i:], err)
			break
		}

		assetOverride := entry.assetName
		var assetMatch, assetMatchRegex string
		if entry.assetSelector != nil {
			assetOverride = ""
			switch entry.assetSelector.Type {
			case appcfg.AssetSelectorSubstring:
				assetMatch = entry.assetSelector.Pattern
			case appcfg.AssetSelectorRegex:
				assetMatchRegex = entry.assetSelector.Pattern
			}
		}
		err := runInstallWithOptions(ctx, client, entry.repository, installOptions{
			assetOverride:     assetOverride,
			assetMatch:        assetMatch,
			assetMatchRegex:   assetMatchRegex,
			binName:           entry.archiveBinary,
			targetName:        entry.targetName,
			release:           entry.release,
			goos:              entry.goos,
			goarch:            entry.goarch,
			checksumPolicy:    string(entry.checksumPolicy),
			checksumPolicySet: true,
			input:             options.input,
			interactive:       false,
		}, io.Discard)
		if err != nil {
			entry.status = upgradeFailed
			entry.reason = err.Error()
			continue
		}

		updatedState, loadErr := state.Load()
		if loadErr != nil {
			entry.status = upgradeFailed
			entry.reason = fmt.Sprintf("reload state after install: %v", loadErr)
			continue
		}
		updatedApp, ok := updatedState.Get(entry.repository)
		if !ok || updatedApp.Version != entry.target {
			entry.status = upgradeFailed
			entry.reason = "installation completed without recording the target version"
			continue
		}
		entry.status = upgradeUpdated
		entry.reason = "installed successfully"
	}

	if err := printUpgradeResults(out, plan); err != nil {
		return err
	}
	if err := printUpgradeSummary(out, plan, false); err != nil {
		return err
	}
	return upgradeExecutionError(plan)
}

func selectUpgradeRepositories(st *state.State, requested []string) []string {
	if len(requested) > 0 {
		selected := append([]string(nil), requested...)
		sort.Strings(selected)
		return uniqueStrings(selected)
	}

	selected := make([]string, 0, len(st.Apps))
	for repository := range st.Apps {
		selected = append(selected, repository)
	}
	sort.Strings(selected)
	return selected
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	unique := values[:1]
	for _, value := range values[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

func buildUpgradePlan(ctx context.Context, client *github.Client, st *state.State, repositories []string) []upgradePlanEntry {
	plan := make([]upgradePlanEntry, 0, len(repositories))
	lookupRepositories := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		app, installed := st.Get(repository)
		entry := upgradePlanEntry{
			repository: repository,
			app:        app,
			current:    app.Version,
			status:     upgradeFailed,
		}
		if !installed {
			entry.reason = "not installed"
			plan = append(plan, entry)
			continue
		}
		if app.Pinned {
			entry.status = upgradeSkipped
			entry.reason = "pinned"
			plan = append(plan, entry)
			continue
		}
		if !repoPattern.MatchString(repository) {
			entry.reason = "invalid repository key in state"
			plan = append(plan, entry)
			continue
		}
		lookupRepositories = append(lookupRepositories, repository)
		plan = append(plan, entry)
	}

	lookups := lookupUpgradeReleasesWithState(ctx, client, lookupRepositories, st)
	for i := range plan {
		entry := &plan[i]
		if entry.status != upgradeFailed || entry.reason != "" {
			continue
		}
		lookup, ok := lookups[entry.repository]
		if !ok {
			entry.reason = "release lookup was not completed"
			continue
		}
		if lookup.err != nil {
			entry.reason = fmt.Sprintf("latest release lookup failed: %v", lookup.err)
			continue
		}
		if lookup.release == nil || lookup.release.TagName == "" {
			entry.reason = "latest release has no tag"
			continue
		}
		entry.release = lookup.release
		entry.target = lookup.release.TagName
		behind, valid := versionBehind(entry.current, entry.target)
		if !valid {
			entry.reason = "installed and latest versions are not valid semantic versions"
			continue
		}
		if !behind {
			entry.status = upgradeUnchanged
			if entry.current == entry.target {
				entry.reason = "already at target version"
			} else {
				entry.reason = "installed version is newer"
			}
			continue
		}

		choices, err := resolveUpgradeChoices(entry.repository, entry.app, lookup.release)
		if err != nil {
			entry.reason = err.Error()
			continue
		}
		entry.assetName = choices.assetName
		entry.assetSelector = choices.assetSelector
		entry.archiveBinary = choices.archiveBinary
		entry.targetName = choices.targetName
		entry.goos = choices.goos
		entry.goarch = choices.goarch
		entry.status = upgradePlanned
		entry.reason = "ready to update"
	}

	return plan
}

func lookupUpgradeReleasesWithState(ctx context.Context, client *github.Client, repositories []string, st *state.State) map[string]upgradeLookup {
	filters := make(map[string]string, len(repositories))
	for _, repository := range repositories {
		if app, ok := st.Get(repository); ok {
			filters[repository] = app.ReleaseFilter
		}
	}
	return lookupUpgradeReleasesWithFilters(ctx, client, repositories, filters)
}

func lookupUpgradeReleasesWithFilters(ctx context.Context, client *github.Client, repositories []string, filters map[string]string) map[string]upgradeLookup {
	results := make(map[string]upgradeLookup, len(repositories))
	if len(repositories) == 0 {
		return results
	}

	workerCount := upgradeLookupWorkers
	if len(repositories) < workerCount {
		workerCount = len(repositories)
	}
	jobs := make(chan string)
	updates := make(chan upgradeLookup, len(repositories))
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for repository := range jobs {
				org, repo, _ := strings.Cut(repository, "/")
				var release *github.Release
				var err error
				if filters[repository] == "" {
					release, err = client.LatestRelease(ctx, org, repo)
				} else {
					release, err = resolveLatestReleaseWithFilter(ctx, client, org, repo, filters[repository])
				}
				updates <- upgradeLookup{repository: repository, release: release, err: err}
			}
		}()
	}
	for _, repository := range repositories {
		jobs <- repository
	}
	close(jobs)
	wg.Wait()
	close(updates)
	for result := range updates {
		results[result.repository] = result
	}
	return results
}

type upgradeChoices struct {
	assetName     string
	assetSelector *appcfg.AssetSelector
	archiveBinary string
	targetName    string
	goos          string
	goarch        string
}

func resolveUpgradeChoices(repository string, app state.App, release *github.Release) (upgradeChoices, error) {
	choices := upgradeChoices{
		goos:   app.PlatformOS,
		goarch: app.PlatformArch,
	}
	if choices.goos == "" {
		choices.goos = runtime.GOOS
	}
	if choices.goarch == "" {
		choices.goarch = runtime.GOARCH
	}
	if !install.ValidPlatform(choices.goos, choices.goarch) {
		return upgradeChoices{}, fmt.Errorf("saved platform %s/%s is unsupported", choices.goos, choices.goarch)
	}

	config, err := appcfg.Load(repository)
	if err != nil {
		return upgradeChoices{}, fmt.Errorf("load app config: %w", err)
	}
	var asset github.Asset
	if config.AssetSelector != nil {
		asset, err = install.SelectAssetWithSelector(release, choices.goos, choices.goarch, config.AssetSelector)
	} else {
		asset, err = install.SelectAsset(release, choices.goos, choices.goarch, config.Asset)
	}
	if err != nil {
		if config.AssetSelector != nil {
			return upgradeChoices{}, fmt.Errorf("saved asset selector %s %q is invalid: %w", config.AssetSelector.Type, config.AssetSelector.Pattern, err)
		}
		if config.Asset != "" {
			return upgradeChoices{}, fmt.Errorf("saved asset choice %q is invalid: %w", config.Asset, err)
		}
		return upgradeChoices{}, fmt.Errorf("resolve release asset: %w", err)
	}
	choices.assetName = asset.Name
	if config.AssetSelector != nil {
		selectorCopy := *config.AssetSelector
		choices.assetSelector = &selectorCopy
	}
	choices.archiveBinary = config.ArchiveBinary
	if config.TargetName != "" {
		if !validTargetName(config.TargetName) {
			return upgradeChoices{}, fmt.Errorf("saved target name %q is invalid", config.TargetName)
		}
		choices.targetName = config.TargetName
	} else {
		choices.targetName = repository[strings.LastIndex(repository, "/")+1:]
	}
	return choices, nil
}

func printUpgradePlan(out io.Writer, plan []upgradePlanEntry) error {
	if len(plan) == 0 {
		_, err := fmt.Fprintln(out, "No applications installed.")
		return err
	}
	if _, err := fmt.Fprintln(out, "Upgrade plan:"); err != nil {
		return err
	}
	for _, entry := range plan {
		current := entry.current
		if current == "" {
			current = "-"
		}
		target := entry.target
		if target == "" {
			target = "-"
		}
		if _, err := fmt.Fprintf(out, "%s: %s -> %s [%s] (%s)\n", entry.repository, current, target, entry.status, entry.reason); err != nil {
			return err
		}
	}
	return nil
}

func printUpgradeChecksumPolicyPending(out io.Writer, plan []upgradePlanEntry) error {
	for _, entry := range plan {
		if entry.status != upgradePlanned || entry.release == nil || entry.assetName == "" {
			continue
		}
		if _, err := fmt.Fprintf(out, "checksum verification pending: policy=%s repository=%s release=%s asset=%q\n", entry.checksumPolicy, entry.repository, entry.release.TagName, entry.assetName); err != nil {
			return err
		}
	}
	return nil
}

func printUpgradeSummary(out io.Writer, plan []upgradePlanEntry, dryRun bool) error {
	updated := countUpgradeStatus(plan, upgradeUpdated)
	planned := countUpgradeStatus(plan, upgradePlanned)
	unchanged := countUpgradeStatus(plan, upgradeUnchanged)
	skipped := countUpgradeStatus(plan, upgradeSkipped)
	failed := countUpgradeStatus(plan, upgradeFailed)
	if dryRun {
		_, err := fmt.Fprintf(out, "Summary: planned=%d unchanged=%d skipped=%d failed=%d\n", planned, unchanged, skipped, failed)
		return err
	}
	_, err := fmt.Fprintf(out, "Summary: updated=%d unchanged=%d skipped=%d failed=%d\n", updated, unchanged, skipped, failed)
	return err
}

func printUpgradeResults(out io.Writer, plan []upgradePlanEntry) error {
	if _, err := fmt.Fprintln(out, "Upgrade results:"); err != nil {
		return err
	}
	for _, entry := range plan {
		if _, err := fmt.Fprintf(out, "%s: [%s] (%s)\n", entry.repository, entry.status, entry.reason); err != nil {
			return err
		}
	}
	return nil
}

func countUpgradeStatus(plan []upgradePlanEntry, status upgradeStatus) int {
	count := 0
	for _, entry := range plan {
		if entry.status == status {
			count++
		}
	}
	return count
}

func upgradePlanError(plan []upgradePlanEntry) error {
	failed := countUpgradeStatus(plan, upgradeFailed)
	if failed == 0 {
		return nil
	}
	return fmt.Errorf("upgrade plan contains %d failed entr%s", failed, pluralSuffix(failed, "y", "ies"))
}

func upgradeExecutionError(plan []upgradePlanEntry) error {
	failed := countUpgradeStatus(plan, upgradeFailed)
	if failed == 0 {
		return nil
	}
	return fmt.Errorf("upgrade failed for %d application%s", failed, pluralSuffix(failed, "", "s"))
}

func pluralSuffix(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func markRemainingUpgradeFailures(entries []upgradePlanEntry, err error) {
	for i := range entries {
		if entries[i].status == upgradePlanned {
			entries[i].status = upgradeFailed
			entries[i].reason = fmt.Sprintf("context cancelled: %v", err)
		}
	}
}

func confirmUpgrade(input io.Reader, out io.Writer) (bool, error) {
	if _, err := fmt.Fprint(out, "Proceed with the upgrade plan? [y/N] "); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("read upgrade confirmation: %w", err)
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes", nil
}

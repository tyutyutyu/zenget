package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/install"
	"zenget/internal/provider"
	"zenget/internal/state"
	"zenget/internal/verify"
	"zenget/internal/version"
)

var (
	selfExecutable   = os.Executable
	selfEvalSymlinks = filepath.EvalSymlinks
	selfGOOS         = runtime.GOOS
)

var selfUpgradeCmd = &cobra.Command{
	Use:   "self-upgrade",
	Short: "Upgrade zenget itself",
	Args:  cobra.NoArgs,
	RunE:  runSelfUpgrade,
}

func init() {
	selfUpgradeCmd.Flags().String("api-base-url", "", "Release API base URL (e.g. GitHub Enterprise /api/v3, Forgejo /api/v1, GitLab /api/v4)")
	selfUpgradeCmd.Flags().String("forge", "", "Release forge: github, gitlab, or forgejo; auto-detected from API host/path when omitted")
	rootCmd.AddCommand(selfUpgradeCmd)
}

func runSelfUpgrade(cmd *cobra.Command, args []string) error {
	client, err := newCommandProvider(cmd)
	if err != nil {
		return err
	}
	return runSelfUpgradeWithClient(cmd.Context(), client, cmd.OutOrStdout())
}

type selfUpgradeOptions struct {
	repository   string
	goos         string
	goarch       string
	executable   func() (string, error)
	evalSymlinks func(string) (string, error)
}

func runSelfUpgradeWithClient(ctx context.Context, client provider.Provider, out io.Writer) error {
	return runSelfUpgradeWithOptions(ctx, client, selfUpgradeOptions{
		repository:   version.Repository,
		goos:         selfGOOS,
		goarch:       runtime.GOARCH,
		executable:   selfExecutable,
		evalSymlinks: selfEvalSymlinks,
	}, out)
}

func runSelfUpgradeWithOptions(ctx context.Context, client provider.Provider, options selfUpgradeOptions, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return fmt.Errorf("release provider is nil")
	}
	if out == nil {
		out = io.Discard
	}
	if err := validateSelfUpgradeRepository(options.repository); err != nil {
		return err
	}
	if options.goos == "" {
		options.goos = runtime.GOOS
	}
	if options.goarch == "" {
		options.goarch = runtime.GOARCH
	}
	if options.goos == "windows" {
		return fmt.Errorf("self-upgrade is not supported on Windows; replace zenget.exe manually")
	}

	target, err := resolveSelfUpgradeTarget(options.executable, options.evalSymlinks)
	if err != nil {
		return err
	}
	installedState, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state before self-upgrade: %w", err)
	}
	if repository, managed, err := selfUpgradeManagedRepository(installedState, target); err != nil {
		return err
	} else if managed {
		return fmt.Errorf("refusing to replace zenget-managed path %q for %s; use zenget upgrade for managed installations", target, repository)
	}

	org, repo, _ := strings.Cut(options.repository, "/")
	release, err := client.LatestRelease(ctx, org, repo)
	if err != nil {
		return fmt.Errorf("fetch latest zenget release: %w", err)
	}
	if release == nil {
		return fmt.Errorf("fetch latest zenget release: provider returned no release")
	}
	if release.Draft {
		return fmt.Errorf("latest zenget release %q is a draft", release.TagName)
	}

	comparison, err := compareSelfVersions(version.Version, release.TagName)
	if err != nil {
		return err
	}
	if comparison >= 0 {
		if comparison == 0 {
			_, _ = fmt.Fprintf(out, "zenget %s is already up to date (latest %s)\n", version.Version, release.TagName)
		} else {
			_, _ = fmt.Fprintf(out, "zenget %s is newer than latest %s; no downgrade performed\n", version.Version, release.TagName)
		}
		return nil
	}

	asset, err := install.SelectAsset(release, options.goos, options.goarch, "")
	if err != nil {
		return fmt.Errorf("select zenget release asset: %w", err)
	}
	download, err := os.CreateTemp("", "zenget-self-upgrade-*")
	if err != nil {
		return fmt.Errorf("create temporary self-upgrade download: %w", err)
	}
	downloadPath := download.Name()
	if err := download.Close(); err != nil {
		_ = os.Remove(downloadPath)
		return fmt.Errorf("close temporary self-upgrade download %q: %w", downloadPath, err)
	}
	defer func() { _ = os.Remove(downloadPath) }()

	if err := client.DownloadAsset(ctx, asset, downloadPath); err != nil {
		return fmt.Errorf("download zenget release asset %q: %w", asset.Name, err)
	}
	if err := verify.Verify(ctx, client, out, asset, release, downloadPath); err != nil {
		return err
	}

	binary, err := install.ExtractBinary(downloadPath, install.DetectFormat(asset.Name), filepath.Base(repo))
	if err != nil {
		return fmt.Errorf("extract zenget binary from %q: %w", asset.Name, err)
	}
	if err := install.ReplaceExecutable(binary, target); err != nil {
		return fmt.Errorf("replace running zenget binary %q: %w", target, err)
	}
	_, _ = fmt.Fprintf(out, "Upgraded zenget from %s to %s at %s\n", version.Version, release.TagName, target)
	return nil
}

var selfVersionPattern = regexp.MustCompile(`^[vV]?([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

type selfSemVersion struct {
	major      uint64
	minor      uint64
	patch      uint64
	preRelease []string
}

func compareSelfVersions(current, latest string) (int, error) {
	currentVersion, err := parseSelfVersion(current)
	if err != nil {
		return 0, fmt.Errorf("invalid current zenget version %q: %w", current, err)
	}
	latestVersion, err := parseSelfVersion(latest)
	if err != nil {
		return 0, fmt.Errorf("invalid latest zenget release tag %q: %w", latest, err)
	}
	return compareParsedSelfVersions(currentVersion, latestVersion), nil
}

func parseSelfVersion(value string) (selfSemVersion, error) {
	matches := selfVersionPattern.FindStringSubmatch(value)
	if matches == nil {
		return selfSemVersion{}, fmt.Errorf("expected semantic version major.minor.patch with an optional prerelease")
	}
	parts := matches[1:4]
	numbers := make([]uint64, len(parts))
	for i, part := range parts {
		if len(part) > 1 && part[0] == '0' {
			return selfSemVersion{}, fmt.Errorf("numeric version components cannot contain leading zeroes")
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return selfSemVersion{}, fmt.Errorf("invalid numeric version component %q: %w", part, err)
		}
		numbers[i] = number
	}

	parsed := selfSemVersion{major: numbers[0], minor: numbers[1], patch: numbers[2]}
	if matches[4] != "" {
		for _, identifier := range strings.Split(matches[4], ".") {
			if isNumericSelfIdentifier(identifier) && len(identifier) > 1 && identifier[0] == '0' {
				return selfSemVersion{}, fmt.Errorf("numeric prerelease identifiers cannot contain leading zeroes")
			}
			parsed.preRelease = append(parsed.preRelease, identifier)
		}
	}
	return parsed, nil
}

func compareParsedSelfVersions(current, latest selfSemVersion) int {
	if comparison := compareUint64(current.major, latest.major); comparison != 0 {
		return comparison
	}
	if comparison := compareUint64(current.minor, latest.minor); comparison != 0 {
		return comparison
	}
	if comparison := compareUint64(current.patch, latest.patch); comparison != 0 {
		return comparison
	}
	return compareSelfPrereleases(current.preRelease, latest.preRelease)
}

func compareUint64(left, right uint64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func compareSelfPrereleases(current, latest []string) int {
	if len(current) == 0 && len(latest) == 0 {
		return 0
	}
	if len(current) == 0 {
		return 1
	}
	if len(latest) == 0 {
		return -1
	}

	for i := 0; i < len(current) && i < len(latest); i++ {
		if comparison := compareSelfIdentifiers(current[i], latest[i]); comparison != 0 {
			return comparison
		}
	}
	return compareInt(len(current), len(latest))
}

func compareSelfIdentifiers(current, latest string) int {
	currentNumeric := isNumericSelfIdentifier(current)
	latestNumeric := isNumericSelfIdentifier(latest)
	if currentNumeric && latestNumeric {
		return compareNumericSelfIdentifiers(current, latest)
	}
	if currentNumeric {
		return -1
	}
	if latestNumeric {
		return 1
	}
	return strings.Compare(current, latest)
}

func compareNumericSelfIdentifiers(current, latest string) int {
	if len(current) != len(latest) {
		return compareInt(len(current), len(latest))
	}
	return strings.Compare(current, latest)
}

func compareInt(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func isNumericSelfIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validateSelfUpgradeRepository(repository string) error {
	switch strings.ToLower(repository) {
	case "", "owner/repo", "org/repo", "your-org/your-repo":
		return fmt.Errorf("self-upgrade repository is not configured: %q", repository)
	}
	if !repoPattern.MatchString(repository) {
		return fmt.Errorf("invalid self-upgrade repository %q: expected org/repo", repository)
	}
	return nil
}

func resolveSelfUpgradeTarget(executable func() (string, error), evalSymlinks func(string) (string, error)) (string, error) {
	if executable == nil {
		return "", fmt.Errorf("resolve running zenget binary: executable resolver is nil")
	}
	if evalSymlinks == nil {
		return "", fmt.Errorf("resolve running zenget binary: symlink resolver is nil")
	}

	executablePath, err := executable()
	if err != nil {
		return "", fmt.Errorf("resolve running zenget binary: %w", err)
	}
	if executablePath == "" {
		return "", fmt.Errorf("resolve running zenget binary: executable path is empty")
	}
	resolvedPath, err := evalSymlinks(executablePath)
	if err != nil {
		return "", fmt.Errorf("resolve running zenget binary symlink %q: %w", executablePath, err)
	}
	if resolvedPath == "" {
		return "", fmt.Errorf("resolve running zenget binary: resolved path is empty")
	}
	absolutePath, err := filepath.Abs(resolvedPath)
	if err != nil {
		return "", fmt.Errorf("resolve running zenget binary path %q: %w", resolvedPath, err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return "", fmt.Errorf("inspect running zenget binary %q: %w", absolutePath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("running zenget binary %q is not a regular file", absolutePath)
	}
	return absolutePath, nil
}

func selfUpgradeManagedRepository(st *state.State, target string) (string, bool, error) {
	if st == nil {
		return "", false, nil
	}
	for repository, app := range st.Apps {
		paths := []string{app.InstallPath, app.RealPath}
		for _, artifact := range app.ArtifactPaths() {
			if artifact != nil {
				paths = append(paths, artifact.RealPath)
			}
		}
		for _, candidate := range paths {
			matches, err := selfUpgradePathsEqual(target, candidate)
			if err != nil {
				return "", false, err
			}
			if matches {
				return repository, true, nil
			}
		}
	}
	return "", false, nil
}

func selfUpgradePathsEqual(left, right string) (bool, error) {
	if left == "" || right == "" {
		return false, nil
	}
	leftAbsolute, err := filepath.Abs(left)
	if err != nil {
		return false, fmt.Errorf("resolve managed path %q: %w", left, err)
	}
	rightAbsolute, err := filepath.Abs(right)
	if err != nil {
		return false, fmt.Errorf("resolve managed path %q: %w", right, err)
	}
	if filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute) {
		return true, nil
	}
	leftResolved, leftErr := filepath.EvalSymlinks(leftAbsolute)
	rightResolved, rightErr := filepath.EvalSymlinks(rightAbsolute)
	if leftErr != nil || rightErr != nil {
		return false, nil
	}
	return filepath.Clean(leftResolved) == filepath.Clean(rightResolved), nil
}

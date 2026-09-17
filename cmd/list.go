package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/provider"
	"zenget/internal/state"
	"zenget/internal/usage"
)

var (
	listJSON    bool
	listUpdates bool
	listCheck   bool
	listUnused  bool
)

type exitStatusError struct {
	code    int
	message string
}

func (e *exitStatusError) Error() string {
	return e.message
}

func (e *exitStatusError) ExitCode() int {
	return e.code
}

var (
	errUpdatesAvailable         = &exitStatusError{code: 3, message: "updates available"}
	errUpdateCheckIndeterminate = &exitStatusError{code: 1, message: "update check indeterminate"}
)

// ANSI color codes used to highlight version differences in --updates output.
const (
	colorReset  = "\033[0m"
	colorWhite  = "\033[97m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorOrange = "\033[38;5;208m"
	colorRed    = "\033[31m"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "l"},
	Short:   "List installed applications",
	Args:    validateListArgs,
	RunE:    runList,
}

func init() {
	listCmd.Flags().BoolVar(&listJSON, "json", false, "print the full state as JSON")
	listCmd.Flags().BoolVarP(&listUpdates, "updates", "u", false, "show the latest upstream release for each app")
	listCmd.Flags().BoolVar(&listCheck, "check", false, "exit with status 3 when updates are available (requires --updates)")
	listCmd.Flags().BoolVar(&listUnused, "unused", false, "show apps not used in the last 90 days (ignores --updates)")
	listCmd.Flags().String("api-base-url", "", "Release API base URL (e.g. GitHub Enterprise /api/v3, Forgejo /api/v1, GitLab /api/v4)")
	listCmd.Flags().String("forge", "", "Release forge: github, gitlab, or forgejo; auto-detected from API host/path when omitted")
	rootCmd.AddCommand(listCmd)
}

func validateListArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return err
	}
	if listCheck && !listUpdates {
		return fmt.Errorf("--check requires --updates")
	}
	return nil
}

// latestInfo holds the upstream release data for a single app when --updates
// is enabled. Nil pointers mean the fetch failed or the state key was invalid.
type latestInfo struct {
	Version *string
	AgeDays *int
}

// listApp embeds state.App and adds the optional latest-release fields used by
// --json --updates.
type listApp struct {
	state.App
	LatestVersion *string `json:"latest_version,omitempty"`
	LatestAgeDays *int    `json:"latest_age_days,omitempty"`
}

func runList(cmd *cobra.Command, args []string) error {
	client, err := newCommandProvider(cmd)
	if err != nil {
		return err
	}
	return runListWithClient(cmd.Context(), client, cmd, time.Now().UTC())
}

// runListWithClient performs the list command using client for any upstream
// checks. Keeping the client injectable makes the command flow testable without
// contacting the real GitHub API.
func runListWithClient(ctx context.Context, client provider.Provider, cmd *cobra.Command, now time.Time) error {
	if ctx == nil {
		ctx = context.Background()
	}
	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	if listUnused {
		return printUnused(st, cmd.OutOrStdout(), listJSON, now)
	}

	var latest map[string]latestInfo
	if listUpdates {
		latest = fetchLatestReleases(ctx, client, st.Apps, now)
	}

	if err := printList(st, latest, cmd.OutOrStdout(), listJSON, now); err != nil {
		return err
	}
	if listCheck {
		status := updateCheckStatus(st.Apps, latest)
		if status != nil {
			// A check status is conveyed by the process exit code. Suppress
			// Cobra's error and usage text so the list output stays unchanged.
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
		}
		return status
	}
	return nil
}

func updateCheckStatus(apps map[string]state.App, latest map[string]latestInfo) error {
	updatesAvailable := false
	for key, app := range apps {
		info, ok := latest[key]
		if !ok || info.Version == nil {
			return errUpdateCheckIndeterminate
		}

		behind, valid := versionBehind(app.Version, *info.Version)
		if !valid {
			return errUpdateCheckIndeterminate
		}
		updatesAvailable = updatesAvailable || behind
	}
	if updatesAvailable {
		return errUpdatesAvailable
	}
	return nil
}

// printList writes the installed applications in st to out, either as a
// human-readable table sorted by name or as indented JSON.
func printList(st *state.State, latest map[string]latestInfo, out io.Writer, asJSON bool, now time.Time) error {
	if asJSON {
		if listUpdates {
			apps := make(map[string]listApp, len(st.Apps))
			for key, app := range st.Apps {
				la := listApp{App: app}
				if info, ok := latest[key]; ok {
					la.LatestVersion = info.Version
					la.LatestAgeDays = info.AgeDays
				}
				apps[key] = la
			}
			data, err := json.MarshalIndent(apps, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal state: %w", err)
			}
			_, err = fmt.Fprintf(out, "%s\n", data)
			return err
		}

		data, err := json.MarshalIndent(st.Apps, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal state: %w", err)
		}
		_, err = fmt.Fprintf(out, "%s\n", data)
		return err
	}

	if len(st.Apps) == 0 {
		_, err := fmt.Fprintln(out, "No applications installed.")
		return err
	}

	names := make([]string, 0, len(st.Apps))
	for name := range st.Apps {
		names = append(names, name)
	}
	sort.Strings(names)

	colors := colorEnabled(colorFlag, out)

	var headers []string
	if listUpdates {
		headers = []string{"NAME", "VERSION", "LATEST", "INSTALLED", "PATH"}
		if colors {
			for i, h := range headers {
				headers[i] = colorWhite + h + colorReset
			}
		}
	} else {
		headers = []string{"NAME", "VERSION", "INSTALLED", "PATH"}
	}

	rows := make([][]string, 0, len(names))
	for _, name := range names {
		app := st.Apps[name]
		version := app.Version
		if app.Pinned {
			version += " (pinned)"
		}
		if listUpdates && colors {
			if info, ok := latest[name]; ok && info.Version != nil {
				version = colorize(version, colorForDiff(semverDiff(app.Version, *info.Version)))
			}
		}
		row := []string{name, version}
		if listUpdates {
			row = append(row, formatLatest(latest[name], now), app.InstalledAt.Format("2006-01-02 15:04"), app.InstallPath)
		} else {
			row = append(row, app.InstalledAt.Format("2006-01-02 15:04"), app.InstallPath)
		}
		rows = append(rows, row)
	}

	widths := columnWidths(headers, rows)
	if err := writeRow(out, headers, widths); err != nil {
		return err
	}
	for _, row := range rows {
		if err := writeRow(out, row, widths); err != nil {
			return err
		}
	}
	return nil
}

// fetchLatestReleases asks GitHub for the latest release of every installed
// app concurrently. Errors are recorded per row so the command can still
// succeed; missing or malformed state keys simply yield an empty latestInfo.
func fetchLatestReleases(ctx context.Context, client provider.Provider, apps map[string]state.App, now time.Time) map[string]latestInfo {
	if client == nil || len(apps) == 0 {
		return map[string]latestInfo{}
	}

	type result struct {
		key  string
		info latestInfo
	}

	var wg sync.WaitGroup
	results := make(chan result, len(apps))

	for key := range apps {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			info := latestInfo{}
			if org, repo, ok := splitRepo(key); ok {
				var release *provider.Release
				var err error
				if apps[key].ReleaseFilter == "" {
					release, err = client.LatestRelease(ctx, org, repo)
				} else {
					release, err = resolveLatestReleaseWithFilter(ctx, client, org, repo, apps[key].ReleaseFilter)
				}
				if err == nil && release != nil && release.TagName != "" {
					age := ageDays(release.PublishedAt, now)
					v := release.TagName
					info = latestInfo{Version: &v, AgeDays: &age}
				}
			}
			results <- result{key: key, info: info}
		}(key)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	latest := make(map[string]latestInfo, len(apps))
	for r := range results {
		latest[r.key] = r.info
	}
	return latest
}

// formatLatest renders a latestInfo value as "<tag> (<N>d old)" or "?" when the
// fetch failed.
func formatLatest(info latestInfo, now time.Time) string {
	if info.Version == nil || info.AgeDays == nil {
		return "?"
	}
	return fmt.Sprintf("%s (%dd old)", *info.Version, *info.AgeDays)
}

// ageDays returns the number of whole days between publishedAt and now.
func ageDays(publishedAt, now time.Time) int {
	return int(now.Sub(publishedAt).Hours() / 24)
}

// splitRepo splits a state key into org/repo. Keys without exactly one slash
// or with empty parts are not valid GitHub repositories.
func splitRepo(key string) (string, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// diffLevel describes how far the installed version is behind the latest.
type diffLevel int

const (
	diffSame diffLevel = iota
	diffPatch
	diffMinor
	diffMajor
	diffInvalid
)

// semverDiff compares two semantic version strings. It accepts a leading "v"
// or "V" and returns diffInvalid when either value cannot be parsed.
func semverDiff(installed, latest string) diffLevel {
	iMaj, iMin, iPat, ok1 := parseSemver(installed)
	lMaj, lMin, lPat, ok2 := parseSemver(latest)
	if !ok1 || !ok2 {
		return diffInvalid
	}
	if iMaj != lMaj {
		return diffMajor
	}
	if iMin != lMin {
		return diffMinor
	}
	if iPat != lPat {
		return diffPatch
	}
	return diffSame
}

func versionBehind(installed, latest string) (behind, valid bool) {
	iMaj, iMin, iPat, installedValid := parseSemver(installed)
	lMaj, lMin, lPat, latestValid := parseSemver(latest)
	if !installedValid || !latestValid {
		return false, false
	}
	if iMaj != lMaj {
		return iMaj < lMaj, true
	}
	if iMin != lMin {
		return iMin < lMin, true
	}
	return iPat < lPat, true
}

// parseSemver extracts the three numeric segments of a semantic version. It
// ignores a leading "v" or "V" and requires exactly major.minor.patch.
func parseSemver(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	patch, err = strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

// colorForDiff maps a version difference to the ANSI color used for the
// VERSION column. diffInvalid yields an empty string so the text stays neutral.
func colorForDiff(level diffLevel) string {
	switch level {
	case diffSame:
		return colorGreen
	case diffPatch:
		return colorYellow
	case diffMinor:
		return colorOrange
	case diffMajor:
		return colorRed
	default:
		return ""
	}
}

// colorize wraps text in color and reset codes. An empty color returns text
// unchanged.
func colorize(text, color string) string {
	if color == "" {
		return text
	}
	return color + text + colorReset
}

// columnWidths computes the maximum visible width of each column, ignoring ANSI
// escape sequences so color codes do not misalign the table.
func columnWidths(headers []string, rows [][]string) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = visibleLen(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				continue
			}
			if w := visibleLen(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	return widths
}

// writeRow prints a single table row with two-space column separation.
func writeRow(out io.Writer, cells []string, widths []int) error {
	for i, cell := range cells {
		if i > 0 {
			if _, err := fmt.Fprint(out, "  "); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprint(out, padCell(cell, widths[i])); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out)
	return err
}

// padCell returns cell padded with spaces to the given visible width. ANSI
// escape sequences are ignored when computing the visible width.
func padCell(cell string, width int) string {
	pad := width - visibleLen(cell)
	if pad <= 0 {
		return cell
	}
	return cell + strings.Repeat(" ", pad)
}

// visibleLen returns the number of visible runes in s, skipping ANSI escape
// sequences (ESC followed by a byte sequence ending in a letter).
func visibleLen(s string) int {
	n := 0
	inEscape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEscape {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				inEscape = false
			}
			continue
		}
		if c == '\033' {
			inEscape = true
			continue
		}
		n++
	}
	return n
}

// bucketLabel names the age groups used by --unused.
type bucketLabel string

const (
	bucketNever   bucketLabel = "never"
	bucketYear    bucketLabel = "1 year or more"
	bucketHalf    bucketLabel = "6-12 months"
	bucketQuarter bucketLabel = "3-6 months"
)

// unusedApp describes one installed app together with the number of days
// since its last recorded invocation. daysSince is nil when no usage log exists
// or the log contains no usable events.
type unusedApp struct {
	Name        string
	InstallPath string
	DaysSince   *int
	Bucket      bucketLabel
	LastUseText string
}

// unusedAppJSON is the JSON representation of an unusedApp.
type unusedAppJSON struct {
	Name             string `json:"name"`
	InstallPath      string `json:"install_path"`
	DaysSinceLastUse *int   `json:"days_since_last_use"`
	Bucket           string `json:"bucket"`
}

// collectUnusedApps aggregates usage logs for every installed app and returns
// those that have never been used or were last used at least 90 days ago.
func collectUnusedApps(st *state.State, now time.Time) ([]unusedApp, error) {
	names := make([]string, 0, len(st.Apps))
	for name := range st.Apps {
		names = append(names, name)
	}
	sort.Strings(names)

	var unused []unusedApp
	for _, name := range names {
		events, err := readUsageEvents(name)
		if err != nil {
			return nil, err
		}
		stats := usage.Aggregate(name, events)
		app := st.Apps[name]

		item := unusedApp{
			Name:        name,
			InstallPath: app.InstallPath,
		}

		if stats.LastRun == nil {
			item.Bucket = bucketNever
			item.LastUseText = "never"
			unused = append(unused, item)
			continue
		}

		days := ageDays(*stats.LastRun, now)
		if days >= 90 {
			item.DaysSince = &days
			item.Bucket = bucketForDays(days)
			item.LastUseText = stats.LastRun.Format("2006-01-02 15:04")
			unused = append(unused, item)
		}
	}

	sort.Slice(unused, func(i, j int) bool {
		if unused[i].DaysSince == nil && unused[j].DaysSince == nil {
			return unused[i].Name < unused[j].Name
		}
		if unused[i].DaysSince == nil {
			return true
		}
		if unused[j].DaysSince == nil {
			return false
		}
		return *unused[i].DaysSince > *unused[j].DaysSince
	})

	return unused, nil
}

// bucketForDays maps a day count (>= 90) to a human-readable bucket.
func bucketForDays(days int) bucketLabel {
	switch {
	case days >= 365:
		return bucketYear
	case days >= 180:
		return bucketHalf
	default:
		return bucketQuarter
	}
}

// printUnused writes apps that have not been used recently, either as JSON or
// as a grouped table.
func printUnused(st *state.State, out io.Writer, asJSON bool, now time.Time) error {
	unused, err := collectUnusedApps(st, now)
	if err != nil {
		return err
	}

	if asJSON {
		return printUnusedJSON(unused, out)
	}
	return printUnusedTable(unused, out)
}

// printUnusedJSON renders the unused list as structured JSON.
func printUnusedJSON(unused []unusedApp, out io.Writer) error {
	items := make([]unusedAppJSON, len(unused))
	for i, app := range unused {
		items[i] = unusedAppJSON{
			Name:             app.Name,
			InstallPath:      app.InstallPath,
			DaysSinceLastUse: app.DaysSince,
			Bucket:           string(app.Bucket),
		}
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal unused apps: %w", err)
	}
	_, err = fmt.Fprintf(out, "%s\n", data)
	return err
}

// printUnusedTable renders the unused list as a grouped table. Empty results
// print a friendly message without an error.
func printUnusedTable(unused []unusedApp, out io.Writer) error {
	if len(unused) == 0 {
		_, err := fmt.Fprintln(out, "No apps unused for 90 days or more.")
		return err
	}

	headers := []string{"NAME", "LAST USE", "DAYS", "PATH"}
	var rows [][]string
	for _, app := range unused {
		daysText := "never"
		if app.DaysSince != nil {
			daysText = strconv.Itoa(*app.DaysSince)
		}
		rows = append(rows, []string{app.Name, app.LastUseText, daysText, app.InstallPath})
	}

	widths := columnWidths(headers, rows)
	if err := writeRow(out, headers, widths); err != nil {
		return err
	}

	var currentBucket bucketLabel
	for i, app := range unused {
		if app.Bucket != currentBucket {
			currentBucket = app.Bucket
			if _, err := fmt.Fprintf(out, "\n%s\n", string(currentBucket)); err != nil {
				return err
			}
		}
		if err := writeRow(out, rows[i], widths); err != nil {
			return err
		}
	}
	return nil
}

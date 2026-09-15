package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/config"
	"zenget/internal/state"
	"zenget/internal/upload"
	"zenget/internal/usage"
	"zenget/internal/wrapper"
)

var statsJSON bool

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show usage statistics for installed applications",
	Args:  cobra.NoArgs,
	RunE:  runStats,
}

func init() {
	statsCmd.Flags().BoolVar(&statsJSON, "json", false, "print statistics as JSON")
	rootCmd.AddCommand(statsCmd)
}

func runStats(cmd *cobra.Command, args []string) error {
	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if err := printStats(st, cmd.OutOrStdout(), statsJSON); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err == nil && cfg.UploadEnabled() {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := upload.MaybeUpload(ctx, cfg, http.DefaultClient, nil); err != nil {
			_ = upload.LogError(err)
		}
	}
	return nil
}

// printStats aggregates usage logs for the installed applications in st and
// writes them to out, either as a human-readable table or as JSON.
func printStats(st *state.State, out io.Writer, asJSON bool) error {
	if out == nil {
		out = io.Discard
	}

	stats, err := collectStats(st)
	if err != nil {
		return err
	}

	if asJSON {
		return printStatsJSON(stats, out)
	}
	return printStatsTable(stats, out)
}

func collectStats(st *state.State) ([]usage.Stats, error) {
	names := make([]string, 0, len(st.Apps))
	for name := range st.Apps {
		names = append(names, name)
	}
	sort.Strings(names)

	var stats []usage.Stats
	for _, name := range names {
		events, err := readUsageEvents(name)
		if err != nil {
			return nil, err
		}
		stats = append(stats, usage.Aggregate(name, events))
	}
	return usage.SortByApp(stats), nil
}

// readUsageEvents reads the current namespaced log and the legacy basename log
// so upgrades preserve usage history written by older wrappers.
func readUsageEvents(repository string) ([]usage.Event, error) {
	logPaths, err := wrapper.UsageLogPaths(repository)
	if err != nil {
		return nil, err
	}

	var events []usage.Event
	for _, logPath := range logPaths {
		logEvents, err := usage.ReadLog(logPath)
		if err != nil {
			return nil, err
		}
		events = append(events, logEvents...)
	}
	return events, nil
}

func printStatsTable(stats []usage.Stats, out io.Writer) error {
	if len(stats) == 0 {
		_, err := fmt.Fprintln(out, "No applications installed.")
		return err
	}

	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tRUNS\tLAST RUN\tTOTAL\tAVERAGE"); err != nil {
		return err
	}
	for _, s := range stats {
		lastRun := "never"
		if s.LastRun != nil {
			lastRun = s.LastRun.Format("2006-01-02 15:04")
		}
		if _, err := fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n",
			s.App, s.Runs, lastRun, roundDuration(s.Total), roundDuration(s.Average)); err != nil {
			return err
		}
	}
	return w.Flush()
}

func printStatsJSON(stats []usage.Stats, out io.Writer) error {
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}
	_, err = fmt.Fprintf(out, "%s\n", data)
	return err
}

// roundDuration formats d as seconds with two decimal places.
func roundDuration(d time.Duration) string {
	return fmt.Sprintf("%.2fs", d.Seconds())
}

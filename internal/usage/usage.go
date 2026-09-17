// Package usage reads and aggregates per-application usage logs written by
// generated wrappers.
package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Event represents a single recorded wrapper invocation.
type Event struct {
	Start      string `json:"start"`
	DurationS  int    `json:"duration_s"`
	ExitCode   int    `json:"exit_code"`
	Repository string `json:"repository,omitempty"`
	Version    string `json:"version,omitempty"`
}

// Record appends one dispatcher invocation to a JSONL usage log. Callers
// decide whether tracking is enabled before invoking this function.
func Record(path, repository, version string, start time.Time, duration time.Duration, exitCode int) error {
	if path == "" {
		return fmt.Errorf("usage log path is empty")
	}
	if start.IsZero() {
		start = time.Now().UTC()
	}
	event := Event{
		Start:      start.UTC().Format(time.RFC3339),
		DurationS:  int(duration / time.Second),
		ExitCode:   exitCode,
		Repository: repository,
		Version:    version,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal usage event: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		if makeErr := os.MkdirAll(filepath.Dir(path), 0755); makeErr != nil {
			return fmt.Errorf("create usage log directory: %w", makeErr)
		}
		file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	}
	if err != nil {
		return fmt.Errorf("open usage log %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write usage log %q: %w", path, err)
	}
	return nil
}

// Stats aggregates the recorded events for one application.
type Stats struct {
	Runs    int           `json:"runs"`
	LastRun *time.Time    `json:"last_run,omitempty"`
	Total   time.Duration `json:"total"`
	Average time.Duration `json:"average"`
	App     string        `json:"app"`
}

// RecordedStart returns the parsed start time of an event, or the zero time
// on failure.
func (e Event) RecordedStart() time.Time {
	t, _ := time.Parse(time.RFC3339, e.Start)
	return t
}

// ReadLog parses a JSONL usage log at path and returns the events in file
// order. Missing files are treated as empty logs.
func ReadLog(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open usage log %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	var events []Event
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("parse usage log line %q: %w", line, err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read usage log %q: %w", path, err)
	}
	return events, nil
}

// Aggregate computes per-app statistics from the supplied events.
func Aggregate(app string, events []Event) Stats {
	stats := Stats{App: app}
	if len(events) == 0 {
		return stats
	}

	var total time.Duration
	var lastRun *time.Time
	for _, ev := range events {
		total += time.Duration(ev.DurationS) * time.Second
		if start := ev.RecordedStart(); !start.IsZero() {
			if lastRun == nil || start.After(*lastRun) {
				c := start
				lastRun = &c
			}
		}
	}

	stats.Runs = len(events)
	stats.LastRun = lastRun
	stats.Total = total
	stats.Average = total / time.Duration(len(events))
	return stats
}

// SortByApp returns a copy of stats sorted by application name.
func SortByApp(stats []Stats) []Stats {
	out := make([]Stats, len(stats))
	copy(out, stats)
	sort.Slice(out, func(i, j int) bool {
		return out[i].App < out[j].App
	})
	return out
}

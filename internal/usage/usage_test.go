package usage

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRecordWritesDispatcherIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "tool.jsonl")
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	if err := Record(path, "acme/tool", "v1.2.3", start, 3*time.Second, 17); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	events, err := ReadLog(path)
	if err != nil {
		t.Fatalf("ReadLog() error = %v", err)
	}
	if len(events) != 1 || events[0].Repository != "acme/tool" || events[0].Version != "v1.2.3" || events[0].DurationS != 3 || events[0].ExitCode != 17 {
		t.Fatalf("recorded events = %+v, want dispatcher identity and status", events)
	}
	if got := events[0].RecordedStart(); got.IsZero() {
		t.Fatal("recorded event has invalid start time")
	}
}

func TestRecordRejectsEmptyPath(t *testing.T) {
	if err := Record("", "acme/tool", "v1", time.Now(), time.Second, 0); err == nil {
		t.Fatal("Record() error = nil, want empty path error")
	}
}

func TestReadMissingLogReturnsEmptyEvents(t *testing.T) {
	events, err := ReadLog(filepath.Join(t.TempDir(), "missing.jsonl"))
	if err != nil {
		t.Fatalf("ReadLog() error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ReadLog() returned %d events, want 0", len(events))
	}
}

func TestReadLogParsesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.jsonl")
	data := `{"start":"2026-08-25T10:00:00Z","duration_s":3,"exit_code":0}
{"start":"2026-08-25T10:05:00Z","duration_s":5,"exit_code":1}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	events, err := ReadLog(path)
	if err != nil {
		t.Fatalf("ReadLog() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ReadLog() returned %d events, want 2", len(events))
	}
	want := []Event{
		{Start: "2026-08-25T10:00:00Z", DurationS: 3, ExitCode: 0},
		{Start: "2026-08-25T10:05:00Z", DurationS: 5, ExitCode: 1},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %#v, want %#v", events, want)
	}
}

func TestReadLogSkipsBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.jsonl")
	data := `{"start":"2026-08-25T10:00:00Z","duration_s":1,"exit_code":0}

{"start":"2026-08-25T10:01:00Z","duration_s":2,"exit_code":0}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	events, err := ReadLog(path)
	if err != nil {
		t.Fatalf("ReadLog() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ReadLog() returned %d events, want 2", len(events))
	}
}

func TestReadLogInvalidJSONReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.jsonl")
	if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := ReadLog(path)
	if err == nil {
		t.Fatal("ReadLog() error = nil, want parse error")
	}
}

func TestAggregateEmptyEvents(t *testing.T) {
	stats := Aggregate("app", nil)
	if stats.Runs != 0 {
		t.Errorf("Runs = %d, want 0", stats.Runs)
	}
	if stats.LastRun != nil {
		t.Errorf("LastRun = %v, want nil", stats.LastRun)
	}
	if stats.Total != 0 {
		t.Errorf("Total = %v, want 0", stats.Total)
	}
	if stats.Average != 0 {
		t.Errorf("Average = %v, want 0", stats.Average)
	}
}

func TestAggregateComputesStats(t *testing.T) {
	events := []Event{
		{Start: "2026-08-25T10:00:00Z", DurationS: 3, ExitCode: 0},
		{Start: "2026-08-25T10:05:00Z", DurationS: 5, ExitCode: 1},
		{Start: "2026-08-25T10:02:00Z", DurationS: 2, ExitCode: 0},
	}

	stats := Aggregate("app", events)
	if stats.App != "app" {
		t.Errorf("App = %q, want %q", stats.App, "app")
	}
	if stats.Runs != 3 {
		t.Errorf("Runs = %d, want 3", stats.Runs)
	}
	if stats.Total != 10*time.Second {
		t.Errorf("Total = %v, want 10s", stats.Total)
	}
	if stats.Average != 10*time.Second/3 {
		t.Errorf("Average = %v, want %v", stats.Average, 10*time.Second/3)
	}
	if stats.LastRun == nil {
		t.Fatal("LastRun = nil, want 2026-08-25 10:05:00 UTC")
	}
	want := time.Date(2026, 8, 25, 10, 5, 0, 0, time.UTC)
	if !stats.LastRun.Equal(want) {
		t.Errorf("LastRun = %v, want %v", stats.LastRun, want)
	}
}

func TestSortByApp(t *testing.T) {
	stats := []Stats{
		{App: "zzz/tool"},
		{App: "acme/widget"},
		{App: "beta/app"},
	}
	got := SortByApp(stats)
	want := []Stats{
		{App: "acme/widget"},
		{App: "beta/app"},
		{App: "zzz/tool"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SortByApp() = %#v, want %#v", got, want)
	}
}

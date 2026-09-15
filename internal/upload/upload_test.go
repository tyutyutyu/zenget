package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zenget/internal/config"
	"zenget/internal/state"
	"zenget/internal/version"
)

func TestEndpointURLOverride(t *testing.T) {
	t.Setenv("ZENGET_USAGE_URL", "https://custom.example/usage")
	if got, want := EndpointURL(), "https://custom.example/usage"; got != want {
		t.Errorf("EndpointURL() = %q, want %q", got, want)
	}
}

func TestEndpointURLDefault(t *testing.T) {
	t.Setenv("ZENGET_USAGE_URL", "")
	if got, want := EndpointURL(), DefaultEndpointURL; got != want {
		t.Errorf("EndpointURL() = %q, want %q", got, want)
	}
}

func TestGenerateTokenIsHex(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}
	if len(tok) != 32 {
		t.Errorf("token length = %d, want 32", len(tok))
	}
	if _, err := hexToBytes(tok); err != nil {
		t.Errorf("token %q is not valid hex: %v", tok, err)
	}
}

func TestLoadOrCreateUserIDCreatesAndReuses(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id1, err := loadOrCreateUserID()
	if err != nil {
		t.Fatalf("loadOrCreateUserID() error = %v", err)
	}
	if id1 == "" {
		t.Fatal("user ID is empty")
	}

	id2, err := loadOrCreateUserID()
	if err != nil {
		t.Fatalf("loadOrCreateUserID() second call error = %v", err)
	}
	if id1 != id2 {
		t.Fatalf("user ID changed from %q to %q", id1, id2)
	}

	path, err := userIDPath()
	if err != nil {
		t.Fatalf("userIDPath() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != id1 {
		t.Errorf("stored user ID = %q, want %q", got, id1)
	}
}

func TestLoadWatermarkMissingReturnsEmpty(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	w, err := loadWatermark()
	if err != nil {
		t.Fatalf("loadWatermark() error = %v", err)
	}
	if w == nil {
		t.Fatal("loadWatermark() returned nil")
	}
	if !w.LastSentAt.IsZero() {
		t.Errorf("LastSentAt = %v, want zero", w.LastSentAt)
	}
	if len(w.Apps) != 0 {
		t.Errorf("Apps = %v, want empty", w.Apps)
	}
}

func TestWatermarkSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	original := &Watermark{
		LastSentAt: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC),
		Apps: map[string]appWatermark{
			"acme/widget": {Count: 3, SentAt: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)},
		},
	}
	if err := original.save(); err != nil {
		t.Fatalf("save() error = %v", err)
	}

	loaded, err := loadWatermark()
	if err != nil {
		t.Fatalf("loadWatermark() error = %v", err)
	}
	if !loaded.LastSentAt.Equal(original.LastSentAt) {
		t.Errorf("LastSentAt = %v, want %v", loaded.LastSentAt, original.LastSentAt)
	}
	if loaded.Apps["acme/widget"].Count != 3 {
		t.Errorf("count = %d, want 3", loaded.Apps["acme/widget"].Count)
	}
}

func TestEventsSinceSkipsSentEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.jsonl")
	data := `{"start":"2026-08-25T10:00:00Z","duration_s":1,"exit_code":0}
{"start":"2026-08-25T10:01:00Z","duration_s":2,"exit_code":0}
{"start":"2026-08-25T10:02:00Z","duration_s":3,"exit_code":1}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	events, total, err := eventsSince(path, 1)
	if err != nil {
		t.Fatalf("eventsSince() error = %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].DurationS != 2 {
		t.Errorf("first event duration = %d, want 2", events[0].DurationS)
	}
}

func TestEventsSinceResetsWhenTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.jsonl")
	if err := os.WriteFile(path, []byte(`{"start":"2026-08-25T10:00:00Z","duration_s":1,"exit_code":0}
`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	events, total, err := eventsSince(path, 5)
	if err != nil {
		t.Fatalf("eventsSince() error = %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(events) != 1 {
		t.Errorf("events = %d, want 1", len(events))
	}
}

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time { return c.now }

type fakeClient struct {
	statusCode int
	respBody   []byte
	requests   []*http.Request
	doErr      error
}

func (f *fakeClient) Do(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req)
	if f.doErr != nil {
		return nil, f.doErr
	}
	code := f.statusCode
	if code == 0 {
		code = 200
	}
	body := io.NopCloser(bytes.NewReader(f.respBody))
	return &http.Response{StatusCode: code, Body: body}, nil
}

func setupUploadTest(t *testing.T) (string, *fixedClock) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	return t.TempDir(), &fixedClock{now: now}
}

func writeUsageLog(t *testing.T, app string, events string) {
	t.Helper()
	dataHome := os.Getenv("XDG_DATA_HOME")
	logDir := filepath.Join(dataHome, "zenget", "usage")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	logPath := filepath.Join(logDir, app+".jsonl")
	if err := os.WriteFile(logPath, []byte(events), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func saveState(t *testing.T, apps map[string]state.App) {
	t.Helper()
	st := &state.State{Apps: apps}
	if err := st.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

func TestMaybeUploadDisabledWhenUploadNotEnabled(t *testing.T) {
	setupUploadTest(t)
	fake := &fakeClient{}
	cfg := &config.Config{UsageTracking: true, UsageUpload: false}
	if err := MaybeUpload(context.Background(), cfg, fake, &fixedClock{}); err != nil {
		t.Fatalf("MaybeUpload() error = %v", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("requests = %d, want 0", len(fake.requests))
	}
}

func TestMaybeUploadDisabledWhenTrackingNotEnabled(t *testing.T) {
	setupUploadTest(t)
	fake := &fakeClient{}
	cfg := &config.Config{UsageTracking: false, UsageUpload: true}
	if err := MaybeUpload(context.Background(), cfg, fake, &fixedClock{}); err != nil {
		t.Fatalf("MaybeUpload() error = %v", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("requests = %d, want 0", len(fake.requests))
	}
}

func TestMaybeUploadSkipsWithin24Hours(t *testing.T) {
	_, clock := setupUploadTest(t)
	fake := &fakeClient{}

	w := &Watermark{
		LastSentAt: clock.Now().Add(-1 * time.Hour),
		Apps:       make(map[string]appWatermark),
	}
	if err := w.save(); err != nil {
		t.Fatalf("save() error = %v", err)
	}

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	if err := MaybeUpload(context.Background(), cfg, fake, clock); err != nil {
		t.Fatalf("MaybeUpload() error = %v", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("requests = %d, want 0", len(fake.requests))
	}
}

func TestMaybeUploadSendsPayloadAndUpdatesWatermark(t *testing.T) {
	_, clock := setupUploadTest(t)
	fake := &fakeClient{statusCode: 200}

	saveState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})
	writeUsageLog(t, "widget", `{"start":"2026-08-25T10:00:00Z","duration_s":2,"exit_code":0}
`)

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	if err := MaybeUpload(context.Background(), cfg, fake, clock); err != nil {
		t.Fatalf("MaybeUpload() error = %v", err)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake.requests))
	}

	var payload Payload
	body, _ := io.ReadAll(fake.requests[0].Body)
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", payload.SchemaVersion)
	}
	if payload.ZengetVersion != version.Version {
		t.Errorf("zenget_version = %q, want %q", payload.ZengetVersion, version.Version)
	}
	if payload.SendID == "" {
		t.Error("send_id is empty")
	}
	if payload.UserID == "" {
		t.Error("user_id is empty")
	}
	if payload.SentAt == "" {
		t.Error("sent_at is empty")
	}
	if len(payload.Apps) != 1 {
		t.Fatalf("apps = %d, want 1", len(payload.Apps))
	}
	if payload.Apps[0].App != "acme/widget" {
		t.Errorf("app = %q, want acme/widget", payload.Apps[0].App)
	}
	if len(payload.Apps[0].Events) != 1 {
		t.Fatalf("events = %d, want 1", len(payload.Apps[0].Events))
	}

	w, err := loadWatermark()
	if err != nil {
		t.Fatalf("loadWatermark() error = %v", err)
	}
	if !w.LastSentAt.Equal(clock.Now()) {
		t.Errorf("LastSentAt = %v, want %v", w.LastSentAt, clock.Now())
	}
	if w.Apps["acme/widget"].Count != 1 {
		t.Errorf("widget count = %d, want 1", w.Apps["acme/widget"].Count)
	}
}

func TestMaybeUploadFailureDoesNotUpdateWatermark(t *testing.T) {
	_, clock := setupUploadTest(t)
	fake := &fakeClient{statusCode: 500}

	saveState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})
	writeUsageLog(t, "widget", `{"start":"2026-08-25T10:00:00Z","duration_s":2,"exit_code":0}
`)

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	err := MaybeUpload(context.Background(), cfg, fake, clock)
	if err == nil {
		t.Fatal("MaybeUpload() error = nil, want failure")
	}

	w, err := loadWatermark()
	if err != nil {
		t.Fatalf("loadWatermark() error = %v", err)
	}
	if !w.LastSentAt.IsZero() {
		t.Errorf("LastSentAt = %v, want zero", w.LastSentAt)
	}
	if _, ok := w.Apps["acme/widget"]; ok {
		t.Error("widget watermark updated on failure")
	}
}

func TestMaybeUploadRetriesAfterFailure(t *testing.T) {
	_, clock := setupUploadTest(t)
	first := &fakeClient{statusCode: 500}

	saveState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})
	writeUsageLog(t, "widget", `{"start":"2026-08-25T10:00:00Z","duration_s":2,"exit_code":0}
`)

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	_ = MaybeUpload(context.Background(), cfg, first, clock)

	second := &fakeClient{statusCode: 200}
	if err := MaybeUpload(context.Background(), cfg, second, clock); err != nil {
		t.Fatalf("retry MaybeUpload() error = %v", err)
	}
	if len(second.requests) != 1 {
		t.Errorf("retry requests = %d, want 1", len(second.requests))
	}
}

func TestMaybeUploadSendsOnlyNewEvents(t *testing.T) {
	_, clock := setupUploadTest(t)
	fake := &fakeClient{statusCode: 200}

	saveState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})
	writeUsageLog(t, "widget", `{"start":"2026-08-25T10:00:00Z","duration_s":1,"exit_code":0}
{"start":"2026-08-25T10:01:00Z","duration_s":2,"exit_code":0}
`)

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	if err := MaybeUpload(context.Background(), cfg, fake, clock); err != nil {
		t.Fatalf("MaybeUpload() error = %v", err)
	}

	// Add one more event and ensure only the new one is sent.
	writeUsageLog(t, "widget", `{"start":"2026-08-25T10:00:00Z","duration_s":1,"exit_code":0}
{"start":"2026-08-25T10:01:00Z","duration_s":2,"exit_code":0}
{"start":"2026-08-25T10:02:00Z","duration_s":3,"exit_code":1}
`)
	clock.now = clock.now.Add(25 * time.Hour)
	fake2 := &fakeClient{statusCode: 200}
	if err := MaybeUpload(context.Background(), cfg, fake2, clock); err != nil {
		t.Fatalf("MaybeUpload() second error = %v", err)
	}
	if len(fake2.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fake2.requests))
	}
	var payload Payload
	body, _ := io.ReadAll(fake2.requests[0].Body)
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if len(payload.Apps) != 1 || len(payload.Apps[0].Events) != 1 {
		t.Fatalf("events = %d, want 1", len(payload.Apps[0].Events))
	}
	if payload.Apps[0].Events[0].DurationS != 3 {
		t.Errorf("duration = %d, want 3", payload.Apps[0].Events[0].DurationS)
	}
}

func TestMaybeUploadNoEventsStillUpdatesWatermark(t *testing.T) {
	_, clock := setupUploadTest(t)
	fake := &fakeClient{statusCode: 200}

	saveState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})

	cfg := &config.Config{UsageTracking: true, UsageUpload: true}
	if err := MaybeUpload(context.Background(), cfg, fake, clock); err != nil {
		t.Fatalf("MaybeUpload() error = %v", err)
	}
	if len(fake.requests) != 0 {
		t.Errorf("requests = %d, want 0", len(fake.requests))
	}

	w, err := loadWatermark()
	if err != nil {
		t.Fatalf("loadWatermark() error = %v", err)
	}
	if !w.LastSentAt.Equal(clock.Now()) {
		t.Errorf("LastSentAt = %v, want %v", w.LastSentAt, clock.Now())
	}
}

func TestLogErrorAppendsToFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	err := LogError(errors.New("upload failed"))
	if err != nil {
		t.Fatalf("LogError() error = %v", err)
	}

	path, _ := errorLogPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "upload failed") {
		t.Errorf("error log missing message: %q", data)
	}
}

func hexToBytes(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("odd length")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var b byte
		if _, err := fmt.Sscanf(s[i:i+2], "%02x", &b); err != nil {
			return nil, err
		}
		out[i/2] = b
	}
	return out, nil
}

// Package upload sends collected usage events to a central collector.
package upload

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"zenget/internal/config"
	"zenget/internal/limits"
	"zenget/internal/state"
	"zenget/internal/usage"
	"zenget/internal/version"
	"zenget/internal/wrapper"
)

// DefaultEndpointURL is the placeholder collector endpoint. It is overridden by
// the ZENGET_USAGE_URL environment variable for tests or custom collectors.
const DefaultEndpointURL = "https://telemetry.zenget.example/v1/usage"

// SchemaVersion is the current upload payload schema version.
const SchemaVersion = 1

// uploadDir returns the directory used for upload state and logs.
func uploadDir() (string, error) {
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, "zenget", "upload"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}

	return filepath.Join(home, ".local", "share", "zenget", "upload"), nil
}

// userIDPath returns the path of the persistent user identifier.
func userIDPath() (string, error) {
	dir, err := uploadDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "user_id"), nil
}

// watermarkPath returns the path of the upload watermark file.
func watermarkPath() (string, error) {
	dir, err := uploadDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "watermark.json"), nil
}

// errorLogPath returns the path of the local upload error log.
func errorLogPath() (string, error) {
	dir, err := uploadDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "errors.log"), nil
}

// GenerateToken returns a secure random 128-bit token encoded as hex.
func GenerateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// loadOrCreateUserID returns the persistent user identifier, generating and
// storing one if it does not yet exist.
func loadOrCreateUserID() (string, error) {
	path, err := userIDPath()
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		return string(bytes.TrimSpace(data)), nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read user ID %q: %w", path, err)
	}

	id, err := GenerateToken()
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create upload directory %q: %w", dir, err)
	}

	temp, err := os.CreateTemp(dir, "user_id-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary user ID file: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := temp.WriteString(id); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("write user ID: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close user ID file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return "", fmt.Errorf("save user ID %q: %w", path, err)
	}

	removeTemp = false
	return id, nil
}

// appWatermark records how many events from a usage log have been successfully
// sent. Count is the number of events acknowledged by the collector.
type appWatermark struct {
	Count  int       `json:"count"`
	SentAt time.Time `json:"sent_at"`
}

// Watermark tracks the last successful upload position per application.
type Watermark struct {
	LastSentAt time.Time               `json:"last_sent_at"`
	Apps       map[string]appWatermark `json:"apps"`
}

// loadWatermark reads the watermark or returns an empty one.
func loadWatermark() (*Watermark, error) {
	path, err := watermarkPath()
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config limits: %w", err)
	}
	data, err := limits.ReadFile(path, cfg.EffectiveLimits().StructuredBytes, "structured JSON")
	if err != nil {
		if os.IsNotExist(err) {
			return &Watermark{Apps: make(map[string]appWatermark)}, nil
		}
		return nil, fmt.Errorf("read watermark %q: %w", path, err)
	}

	var w Watermark
	if err := limits.DecodeJSON(bytes.NewReader(data), &w, cfg.EffectiveLimits().StructuredBytes, "structured JSON", path); err != nil {
		return nil, fmt.Errorf("parse watermark %q: %w", path, err)
	}
	if w.Apps == nil {
		w.Apps = make(map[string]appWatermark)
	}
	return &w, nil
}

// save writes the watermark atomically.
func (w *Watermark) save() error {
	path, err := watermarkPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create upload directory %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal watermark: %w", err)
	}

	temp, err := os.CreateTemp(dir, "watermark-*.json")
	if err != nil {
		return fmt.Errorf("create temporary watermark file: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if n, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write watermark: %w", err)
	} else if n != len(data) {
		_ = temp.Close()
		return fmt.Errorf("write watermark: %w", io.ErrShortWrite)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close watermark file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("save watermark %q: %w", path, err)
	}

	removeTemp = false
	return nil
}

// Clock provides the current time. It is satisfied by time.Now in production
// and by test doubles.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// HTTPClient performs HTTP requests. It is satisfied by *http.Client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// EndpointURL returns the configured collector URL. The ZENGET_USAGE_URL
// environment variable overrides the built-in placeholder.
func EndpointURL() string {
	if u := os.Getenv("ZENGET_USAGE_URL"); u != "" {
		return u
	}
	return DefaultEndpointURL
}

// AppPayload groups raw events for a single application.
type AppPayload struct {
	App    string        `json:"app"`
	Events []usage.Event `json:"events"`
}

// Payload is the JSON body sent to the collector.
type Payload struct {
	SchemaVersion int          `json:"schema_version"`
	SendID        string       `json:"send_id"`
	UserID        string       `json:"user_id"`
	SentAt        string       `json:"sent_at"`
	ZengetVersion string       `json:"zenget_version"`
	Apps          []AppPayload `json:"apps"`
}

// eventsSince returns the events in path that occur after the first count
// events, plus the total number of events in the file. Missing files are
// treated as empty. If count is greater than the current number of events, it
// is treated as zero so nothing is lost when a log is truncated.
func eventsSince(path string, count int) ([]usage.Event, int, error) {
	all, err := usage.ReadLog(path)
	if err != nil {
		return nil, 0, err
	}
	if count < 0 || count > len(all) {
		count = 0
	}
	return all[count:], len(all), nil
}

// LogError appends err to the local upload error log with a timestamp.
func LogError(err error) error {
	if err == nil {
		return nil
	}
	path, err2 := errorLogPath()
	if err2 != nil {
		return err2
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create upload directory: %w", err)
	}
	line := fmt.Sprintf("%s %v\n", time.Now().UTC().Format(time.RFC3339), err)
	if f, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); openErr == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
	return nil
}

// MaybeUpload checks whether an upload is due and sends pending events. It
// returns nil when uploading is disabled, not yet due, or succeeds, and
// returns an error when the upload attempt fails. The caller is responsible for
// keeping failures silent from the user.
func MaybeUpload(ctx context.Context, cfg *config.Config, client HTTPClient, clock Clock) error {
	if !cfg.UploadEnabled() {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	if clock == nil {
		clock = systemClock{}
	}

	userID, err := loadOrCreateUserID()
	if err != nil {
		return err
	}

	watermark, err := loadWatermark()
	if err != nil {
		return err
	}

	now := clock.Now()
	if !watermark.LastSentAt.IsZero() && now.Sub(watermark.LastSentAt) < 24*time.Hour {
		return nil
	}

	st, err := state.Load()
	if err != nil {
		return err
	}

	apps, err := buildAppPayloads(st, watermark)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		// Record a successful "empty" send so we do not keep checking for 24h.
		watermark.LastSentAt = now
		if err := watermark.save(); err != nil {
			return err
		}
		return nil
	}

	sendID, err := GenerateToken()
	if err != nil {
		return err
	}

	payload := Payload{
		SchemaVersion: SchemaVersion,
		SendID:        sendID,
		UserID:        userID,
		SentAt:        now.Format(time.RFC3339),
		ZengetVersion: version.Version,
		Apps:          apps,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal upload payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, EndpointURL(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload returned HTTP %d", resp.StatusCode)
	}

	watermark.LastSentAt = now
	if err := watermark.save(); err != nil {
		return err
	}

	return nil
}

// MaybeUploadNow is a convenience wrapper that uses the default HTTP client
// and the system clock.
func MaybeUploadNow(ctx context.Context, cfg *config.Config) error {
	return MaybeUpload(ctx, cfg, http.DefaultClient, systemClock{})
}

// buildAppPayloads collects pending events per installed app and returns the
// updated watermark. It mutates watermark.Apps with the new counts.
func buildAppPayloads(st *state.State, watermark *Watermark) ([]AppPayload, error) {
	names := make([]string, 0, len(st.Apps))
	for name := range st.Apps {
		names = append(names, name)
	}
	sort.Strings(names)

	var payloads []AppPayload
	for _, name := range names {
		logPaths, err := wrapper.UsageLogPaths(name)
		if err != nil {
			return nil, err
		}
		logPath := logPaths[0]
		if _, statErr := os.Stat(logPath); os.IsNotExist(statErr) && len(logPaths) > 1 {
			logPath = logPaths[1]
		}
		events, total, err := eventsSince(logPath, watermark.Apps[name].Count)
		if err != nil {
			return nil, err
		}
		if len(events) > 0 {
			payloads = append(payloads, AppPayload{App: name, Events: events})
		}
		watermark.Apps[name] = appWatermark{Count: total, SentAt: watermark.LastSentAt}
	}

	return payloads, nil
}

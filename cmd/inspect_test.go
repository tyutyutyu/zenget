package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/appcfg"
	"zenget/internal/github"
	"zenget/internal/provider"
)

func inspectRelease(tag string, assets ...map[string]any) *provider.Release {
	release := &provider.Release{
		TagName:     tag,
		PublishedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	}
	for _, asset := range assets {
		release.Assets = append(release.Assets, provider.Asset{
			ID:                 int64(asset["id"].(int)),
			Name:               asset["name"].(string),
			Size:               int64(asset["size"].(int)),
			Digest:             stringValue(asset["digest"]),
			BrowserDownloadURL: stringValue(asset["browser_download_url"]),
			URL:                stringValue(asset["url"]),
		})
	}
	return release
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return value.(string)
}

func inspectAsset(id int, name string, size int, digest string) map[string]any {
	return map[string]any{"id": id, "name": name, "size": size, "digest": digest}
}

func TestRunInspectWithClientMetadataOnlyHumanReport(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var releaseRequests, downloadRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			releaseRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name":     "v1.2.3",
				"published_at": "2026-09-01T10:00:00Z",
				"assets": []map[string]any{
					{"id": 1, "name": "widget-linux-amd64.tar.gz", "size": 123, "digest": "sha256:abc", "browser_download_url": server.URL + "/asset"},
					{"id": 2, "name": "widget-linux-amd64.zip", "size": 100, "browser_download_url": server.URL + "/asset"},
				},
			})
		case "/asset":
			downloadRequests.Add(1)
			t.Errorf("inspect downloaded an asset")
			http.Error(w, "unexpected download", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	err := runInspectWithClient(context.Background(), client, "acme/widget", inspectOptions{
		goos: "linux", goarch: "amd64",
	}, &output, time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("runInspectWithClient() error = %v", err)
	}
	for _, want := range []string{
		"Repository: acme/widget",
		"Release: v1.2.3",
		"Target: linux/amd64",
		"Selection source: automatic",
		"Selected asset: widget-linux-amd64.tar.gz (tar.gz)",
		"widget-linux-amd64.zip",
		"lower_format_priority",
		"release digest",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("human report missing %q:\n%s", want, output.String())
		}
	}
	if releaseRequests.Load() != 1 || downloadRequests.Load() != 0 {
		t.Fatalf("requests = release %d, downloads %d; want 1, 0", releaseRequests.Load(), downloadRequests.Load())
	}
	assertInspectPathsUnchanged(t, configHome, home)
}

func TestRunInspectWithClientJSONIsStableAndANSIFree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	release := inspectRelease("v2.0.0",
		inspectAsset(2, "widget-linux-amd64.zip", 20, ""),
		inspectAsset(1, "widget-linux-amd64.tar.gz", 30, "sha256:abc"),
		inspectAsset(3, "widget.tar.gz.sha256", 40, ""),
	)
	client := &resolutionTestProvider{latest: release}
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	var first, second bytes.Buffer
	options := inspectOptions{goos: "linux", goarch: "amd64", asJSON: true}
	if err := runInspectWithClient(context.Background(), client, "acme/widget", options, &first, now); err != nil {
		t.Fatalf("first inspect: %v", err)
	}
	if err := runInspectWithClient(context.Background(), client, "acme/widget", options, &second, now); err != nil {
		t.Fatalf("second inspect: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("JSON output is not stable:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
	if strings.ContainsAny(first.String(), "\033") {
		t.Fatalf("JSON contains ANSI escape sequence: %q", first.String())
	}
	var report inspectReport
	if err := json.Unmarshal(first.Bytes(), &report); err != nil {
		t.Fatalf("parse JSON report: %v\n%s", err, first.String())
	}
	if report.Repository != "acme/widget" || report.Status != "selected" || report.SelectedAsset == nil {
		t.Fatalf("JSON report = %#v", report)
	}
	if report.SelectedAsset.Name != "widget-linux-amd64.tar.gz" || len(report.Assets) != 3 {
		t.Fatalf("JSON selection/assets = %#v/%d", report.SelectedAsset, len(report.Assets))
	}
	if client.latestCalls != 2 || client.byTagCalls != 0 || client.listCalls != 0 {
		t.Fatalf("provider calls = latest %d, tag %d, list %d", client.latestCalls, client.byTagCalls, client.listCalls)
	}
}

func TestRunInspectWithClientPersistedChoiceIsReadOnlyAndExplicitWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	config := &appcfg.AppConfig{Asset: "widget-darwin-amd64.tar.gz"}
	if err := config.Save("acme/widget"); err != nil {
		t.Fatalf("save app config fixture: %v", err)
	}
	configPath, err := appcfg.Path("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	client := &resolutionTestProvider{latest: inspectRelease("v1.0.0",
		inspectAsset(1, "widget-linux-amd64.tar.gz", 10, ""),
		inspectAsset(2, "widget-darwin-amd64.tar.gz", 11, ""),
	)}
	var persistedOutput bytes.Buffer
	if err := runInspectWithClient(context.Background(), client, "acme/widget", inspectOptions{
		goos: "linux", goarch: "amd64", asJSON: true,
	}, &persistedOutput, time.Now().UTC()); err != nil {
		t.Fatalf("persisted inspect: %v", err)
	}
	var persistedReport inspectReport
	if err := json.Unmarshal(persistedOutput.Bytes(), &persistedReport); err != nil {
		t.Fatal(err)
	}
	if persistedReport.SelectionSource != "persisted" || persistedReport.SelectedAsset == nil || persistedReport.SelectedAsset.Name != config.Asset {
		t.Fatalf("persisted report = %#v", persistedReport)
	}

	var explicitOutput bytes.Buffer
	if err := runInspectWithClient(context.Background(), client, "acme/widget", inspectOptions{
		assetOverride: "linux", goos: "linux", goarch: "amd64", asJSON: true,
	}, &explicitOutput, time.Now().UTC()); err != nil {
		t.Fatalf("explicit inspect: %v", err)
	}
	var explicitReport inspectReport
	if err := json.Unmarshal(explicitOutput.Bytes(), &explicitReport); err != nil {
		t.Fatal(err)
	}
	if explicitReport.SelectionSource != "explicit" || explicitReport.SelectedAsset == nil || explicitReport.SelectedAsset.Name != "widget-linux-amd64.tar.gz" {
		t.Fatalf("explicit report = %#v", explicitReport)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("inspect changed app config: before %q, after %q", before, after)
	}
}

func TestRunInspectWithClientReportsSelectorTrace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	client := &resolutionTestProvider{latest: inspectRelease("v2.0.0",
		inspectAsset(2, "widget-linux-amd64-v2.zip", 20, ""),
		inspectAsset(1, "widget-linux-amd64-v2.tar.gz", 30, ""),
		inspectAsset(3, "widget-linux-amd64-v1.tar.gz", 31, ""),
		inspectAsset(4, "widget-darwin-amd64-v2.tar.gz", 32, ""),
	)}
	var jsonOutput bytes.Buffer
	if err := runInspectWithClient(context.Background(), client, "acme/widget", inspectOptions{
		assetMatchRegex: `^widget-linux-amd64-v2\.(tar\.gz|zip)$`,
		goos:            "linux",
		goarch:          "amd64",
		asJSON:          true,
	}, &jsonOutput, time.Now().UTC()); err != nil {
		t.Fatalf("selector inspect: %v", err)
	}
	var report inspectReport
	if err := json.Unmarshal(jsonOutput.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Selector.Source != "explicit" || report.Selector.Type != "regex" || report.Selector.Pattern != `^widget-linux-amd64-v2\.(tar\.gz|zip)$` {
		t.Fatalf("selector trace = %#v", report.Selector)
	}
	if report.Selector.Decision != "widget-linux-amd64-v2.tar.gz" || len(report.Selector.Excluded) != 2 {
		t.Fatalf("selector decision/exclusions = %q/%v", report.Selector.Decision, report.Selector.Excluded)
	}
	if report.Selector.Excluded[0] != "widget-darwin-amd64-v2.tar.gz" || report.Selector.Excluded[1] != "widget-linux-amd64-v1.tar.gz" {
		t.Fatalf("selector exclusions = %v", report.Selector.Excluded)
	}

	var humanOutput bytes.Buffer
	if err := runInspectWithClient(context.Background(), client, "acme/widget", inspectOptions{
		assetMatch: "linux-amd64-v2", goos: "linux", goarch: "amd64",
	}, &humanOutput, time.Now().UTC()); err != nil {
		t.Fatalf("substring selector inspect: %v", err)
	}
	for _, want := range []string{"Selector: source=explicit type=substring", "Selector excluded:", "selector_excluded"} {
		if !strings.Contains(humanOutput.String(), want) {
			t.Errorf("human selector report missing %q:\n%s", want, humanOutput.String())
		}
	}
}

func TestRunInspectWithClientReportsStaleChoiceWithoutRewrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	config := &appcfg.AppConfig{Asset: "widget-linux-amd64.tar.gz"}
	if err := config.Save("acme/widget"); err != nil {
		t.Fatal(err)
	}
	configPath, err := appcfg.Path("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	client := &resolutionTestProvider{latest: inspectRelease("v1.1.0", inspectAsset(2, "widget-linux-arm64.tar.gz", 12, ""))}
	var output bytes.Buffer
	err = runInspectWithClient(context.Background(), client, "acme/widget", inspectOptions{
		goos: "linux", goarch: "amd64", asJSON: true,
	}, &output, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), `saved asset choice "widget-linux-amd64.tar.gz" is no longer available`) {
		t.Fatalf("error = %v, want stale choice error", err)
	}
	var report inspectReport
	if unmarshalErr := json.Unmarshal(output.Bytes(), &report); unmarshalErr != nil {
		t.Fatalf("parse stale report: %v\n%s", unmarshalErr, output.String())
	}
	if report.Status != "stale_choice" || report.SelectedAsset != nil || len(report.Assets) != 1 {
		t.Fatalf("stale report = %#v", report)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("stale inspect changed app config: before %q, after %q", before, after)
	}
}

func TestRunInspectWithClientReportsExpectedSelectionFailures(t *testing.T) {
	tests := []struct {
		name       string
		release    *provider.Release
		override   string
		wantStatus string
		wantText   string
	}{
		{
			name:       "empty release",
			release:    inspectRelease("v1.0.0"),
			wantStatus: "empty_release",
			wantText:   "has no downloadable assets",
		},
		{
			name: "no compatible platform",
			release: inspectRelease("v1.0.0",
				inspectAsset(1, "widget-darwin-amd64.tar.gz", 10, ""),
				inspectAsset(2, "widget-windows-amd64.zip", 11, ""),
			),
			wantStatus: "no_compatible_asset",
			wantText:   "none match linux/amd64",
		},
		{
			name: "ambiguous override",
			release: inspectRelease("v1.0.0",
				inspectAsset(1, "widget-linux-amd64.tar.gz", 10, ""),
				inspectAsset(2, "widget-linux-amd64.zip", 11, ""),
			),
			override:   "linux",
			wantStatus: "ambiguous",
			wantText:   "ambiguous assets matching",
		},
		{
			name: "missing override",
			release: inspectRelease("v1.0.0",
				inspectAsset(1, "widget-linux-amd64.tar.gz", 10, ""),
			),
			override:   "windows",
			wantStatus: "no_match",
			wantText:   "no asset matching",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			client := &resolutionTestProvider{latest: test.release}
			var output bytes.Buffer
			err := runInspectWithClient(context.Background(), client, "acme/widget", inspectOptions{
				assetOverride: test.override, goos: "linux", goarch: "amd64", asJSON: true,
			}, &output, time.Now().UTC())
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error = %v, want %q", err, test.wantText)
			}
			var report inspectReport
			if unmarshalErr := json.Unmarshal(output.Bytes(), &report); unmarshalErr != nil {
				t.Fatalf("parse report: %v\n%s", unmarshalErr, output.String())
			}
			if report.Status != test.wantStatus || report.SelectedAsset != nil || len(report.Assets) != len(test.release.Assets) {
				t.Fatalf("report status/selection/assets = %q/%#v/%d", report.Status, report.SelectedAsset, len(report.Assets))
			}
		})
	}
}

func TestRunInspectUsesPrivateMetadataAuthentication(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const secret = "inspect-private-token"
	t.Setenv("ZENGET_GITHUB_TOKEN", secret)
	var downloadRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/private/releases/latest" {
			t.Errorf("request path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.0.0",
			"assets": []map[string]any{{
				"id": 1, "name": "private-linux-amd64.tar.gz", "size": 10,
				"browser_download_url": server.URL + "/private-asset",
			}},
		})
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	if err := runInspectWithClient(context.Background(), client, "acme/private", inspectOptions{
		goos: "linux", goarch: "amd64", asJSON: true,
	}, &output, time.Now().UTC()); err != nil {
		t.Fatalf("private inspect: %v", err)
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "Authorization") {
		t.Fatalf("private report exposes authentication material: %q", output.String())
	}
	if downloadRequests.Load() != 0 {
		t.Fatalf("download requests = %d, want 0", downloadRequests.Load())
	}
}

func TestRunInspectFlagValidation(t *testing.T) {
	newCommand := func() *cobra.Command {
		command := &cobra.Command{Use: "inspect"}
		command.Flags().String("asset", "", "")
		command.Flags().String("asset-match", "", "")
		command.Flags().String("asset-match-regex", "", "")
		command.Flags().String("tag", "", "")
		command.Flags().Int("min-age-days", 0, "")
		command.Flags().String("system", "", "")
		command.Flags().Bool("json", false, "")
		command.Flags().String("api-base-url", "", "")
		command.Flags().String("forge", "", "")
		return command
	}

	command := newCommand()
	if err := command.Flags().Set("min-age-days", "0"); err != nil {
		t.Fatal(err)
	}
	if err := runInspect(command, []string{"acme/widget"}); err == nil || !strings.Contains(err.Error(), "--min-age-days must be a positive integer") {
		t.Fatalf("zero age error = %v", err)
	}

	command = newCommand()
	if err := command.Flags().Set("asset", "widget"); err != nil {
		t.Fatal(err)
	}
	if err := command.Flags().Set("asset-match", "linux"); err != nil {
		t.Fatal(err)
	}
	if err := runInspect(command, []string{"acme/widget"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("selector conflict error = %v", err)
	}

	command = newCommand()
	if err := command.Flags().Set("asset-match-regex", "["); err != nil {
		t.Fatal(err)
	}
	if err := runInspect(command, []string{"acme/widget"}); err == nil || !strings.Contains(err.Error(), "invalid asset selector regex") {
		t.Fatalf("invalid selector regex error = %v", err)
	}

	command = newCommand()
	if err := command.Flags().Set("tag", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := command.Flags().Set("min-age-days", "7"); err != nil {
		t.Fatal(err)
	}
	if err := runInspect(command, []string{"acme/widget"}); err == nil || !strings.Contains(err.Error(), "--tag cannot be combined with --min-age-days") {
		t.Fatalf("conflicting flag error = %v", err)
	}

	command = newCommand()
	if err := command.Flags().Set("system", "linux/not-an-arch"); err != nil {
		t.Fatal(err)
	}
	if err := runInspect(command, []string{"acme/widget"}); err == nil || !strings.Contains(err.Error(), "unsupported --system") {
		t.Fatalf("system error = %v", err)
	}

	if err := runInspect(newCommand(), []string{"badrepo"}); err == nil || !strings.Contains(err.Error(), "invalid repository") {
		t.Fatalf("repository error = %v", err)
	}
}

func assertInspectPathsUnchanged(t *testing.T, configHome, home string) {
	t.Helper()
	if entries, err := os.ReadDir(configHome); err != nil {
		t.Fatalf("read config home: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("inspect created config entries: %v", entries)
	}
	for _, path := range []string{filepath.Join(home, ".local"), filepath.Join(home, ".local", "bin")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("inspect changed %s: stat error = %v", path, err)
		}
	}
}

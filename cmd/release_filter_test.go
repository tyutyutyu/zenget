package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zenget/internal/github"
	"zenget/internal/provider"
	"zenget/internal/state"
)

type releaseFilterTestProvider struct {
	latest       *provider.Release
	latestErr    error
	byTag        *provider.Release
	byTagErr     error
	listed       []provider.Release
	body         []byte
	latestCalls  int
	byTagCalls   int
	listCalls    int
	downloads    int
	requestedTag string
}

type pagedReleaseFilterTestProvider struct {
	*releaseFilterTestProvider
	allListed []provider.Release
	allCalls  int
}

func (p *pagedReleaseFilterTestProvider) ListReleasesAll(context.Context, string, string) ([]provider.Release, error) {
	p.allCalls++
	return p.allListed, nil
}

func (p *releaseFilterTestProvider) LatestRelease(context.Context, string, string) (*provider.Release, error) {
	p.latestCalls++
	return p.latest, p.latestErr
}

func (p *releaseFilterTestProvider) ReleaseByTag(_ context.Context, _, _, tag string) (*provider.Release, error) {
	p.byTagCalls++
	p.requestedTag = tag
	return p.byTag, p.byTagErr
}

func (p *releaseFilterTestProvider) ListReleases(context.Context, string, string) ([]provider.Release, error) {
	p.listCalls++
	return p.listed, nil
}

func (p *releaseFilterTestProvider) DownloadAsset(_ context.Context, _ provider.Asset, destination string, _ ...int64) error {
	p.downloads++
	return os.WriteFile(destination, p.body, 0600)
}

func TestResolveReleaseWithFilterSelectsNewestMatchingStableRelease(t *testing.T) {
	now := time.Now().UTC()
	providerClient := &releaseFilterTestProvider{listed: []provider.Release{
		{TagName: "cli-v9.0.0", PublishedAt: now, Prerelease: true},
		{TagName: "server-v8.0.0", PublishedAt: now.Add(-time.Hour)},
		{TagName: "cli-v2.0.0", PublishedAt: now.Add(-2 * time.Hour)},
		{TagName: "cli-v1.0.0", PublishedAt: now.Add(-3 * time.Hour)},
	}}

	release, err := resolveReleaseWithFilter(context.Background(), providerClient, "acme", "tool", "", "cli-*")
	if err != nil {
		t.Fatalf("resolveReleaseWithFilter() error = %v", err)
	}
	if release.TagName != "cli-v2.0.0" {
		t.Fatalf("selected release = %q, want cli-v2.0.0", release.TagName)
	}
	if providerClient.latestCalls != 0 || providerClient.listCalls != 1 {
		t.Fatalf("provider calls = latest %d/list %d, want 0/1", providerClient.latestCalls, providerClient.listCalls)
	}
}

func TestResolveReleaseWithFilterPreservesExactTagPriorityAndFiltersFallback(t *testing.T) {
	exact := &provider.Release{TagName: "server-v7.0.0"}
	providerClient := &releaseFilterTestProvider{
		byTag:    exact,
		byTagErr: errors.New("unexpected 500 response"),
	}
	release, err := resolveReleaseWithFilter(context.Background(), providerClient, "acme", "tool", "7.0", "cli-*")
	if err == nil || !strings.Contains(err.Error(), "unexpected 500") {
		t.Fatalf("non-404 exact lookup error = %v, want provider error", err)
	}
	if providerClient.listCalls != 0 {
		t.Fatalf("list calls = %d, want 0 after non-404 exact error", providerClient.listCalls)
	}

	providerClient = &releaseFilterTestProvider{
		byTagErr: errors.New("release lookup returned 404"),
		listed: []provider.Release{
			{TagName: "server-v9.0.0", PublishedAt: time.Now().UTC()},
			{TagName: "cli-v1.0.0", PublishedAt: time.Now().UTC().Add(-time.Hour)},
			{TagName: "cli-v2.0.0", PublishedAt: time.Now().UTC()},
		},
	}
	release, err = resolveReleaseWithFilter(context.Background(), providerClient, "acme", "tool", "v", "cli-*")
	if err != nil {
		t.Fatalf("filtered fallback error = %v", err)
	}
	if release.TagName != "cli-v2.0.0" {
		t.Fatalf("filtered fallback release = %q, want cli-v2.0.0", release.TagName)
	}
	if providerClient.byTagCalls != 1 || providerClient.listCalls != 1 {
		t.Fatalf("fallback provider calls = tag %d/list %d, want 1/1", providerClient.byTagCalls, providerClient.listCalls)
	}

	providerClient = &releaseFilterTestProvider{byTag: exact}
	release, err = resolveReleaseWithFilter(context.Background(), providerClient, "acme", "tool", "server", "cli-*")
	if err != nil {
		t.Fatalf("exact tag priority error = %v", err)
	}
	if release != exact || providerClient.listCalls != 0 {
		t.Fatalf("exact release = %#v/list calls %d, want exact release and no list", release, providerClient.listCalls)
	}
}

func TestResolveReleaseWithFilterReportsInvalidAndEmptyFilters(t *testing.T) {
	providerClient := &releaseFilterTestProvider{listed: []provider.Release{{TagName: "server-v1.0.0"}}}
	if _, err := resolveReleaseWithFilter(context.Background(), providerClient, "acme", "tool", "", "["); err == nil || !strings.Contains(err.Error(), "[") {
		t.Fatalf("invalid filter error = %v, want filter text", err)
	}
	if providerClient.listCalls != 0 {
		t.Fatalf("list calls for invalid filter = %d, want 0", providerClient.listCalls)
	}
	if _, err := resolveReleaseWithFilter(context.Background(), providerClient, "acme", "tool", "", "cli-*"); err == nil || !strings.Contains(err.Error(), "cli-*") {
		t.Fatalf("empty result error = %v, want filter text", err)
	}
}

func TestResolveReleaseWithoutFilterKeepsExistingLatestLookup(t *testing.T) {
	latest := &provider.Release{TagName: "v2.0.0"}
	providerClient := &releaseFilterTestProvider{latest: latest, listed: []provider.Release{{TagName: "v1.0.0"}}}

	release, err := resolveReleaseWithFilter(context.Background(), providerClient, "acme", "tool", "", "")
	if err != nil {
		t.Fatalf("unfiltered resolution error = %v", err)
	}
	if release != latest || providerClient.latestCalls != 1 || providerClient.listCalls != 0 {
		t.Fatalf("unfiltered result/calls = %#v/%d/%d, want latest/1/0", release, providerClient.latestCalls, providerClient.listCalls)
	}
}

func TestResolveReleaseWithFilterUsesPagedReleaseProviderWhenAvailable(t *testing.T) {
	providerClient := &pagedReleaseFilterTestProvider{
		releaseFilterTestProvider: &releaseFilterTestProvider{
			listed: []provider.Release{{TagName: "cli-v1.0.0"}},
		},
		allListed: []provider.Release{{TagName: "cli-v2.0.0", PublishedAt: time.Now().UTC()}},
	}

	release, err := resolveReleaseWithFilter(context.Background(), providerClient, "acme", "tool", "", "cli-*")
	if err != nil {
		t.Fatalf("resolveReleaseWithFilter() error = %v", err)
	}
	if release.TagName != "cli-v2.0.0" {
		t.Fatalf("selected release = %q, want cli-v2.0.0", release.TagName)
	}
	if providerClient.allCalls != 1 || providerClient.listCalls != 0 {
		t.Fatalf("provider calls = all %d/list %d, want 1/0", providerClient.allCalls, providerClient.listCalls)
	}
}

func TestResolveReleaseWithMinAgeFilterAppliesFilter(t *testing.T) {
	now := time.Now().UTC()
	providerClient := &releaseFilterTestProvider{listed: []provider.Release{
		{TagName: "server-v9.0.0", PublishedAt: now.AddDate(0, 0, -90)},
		{TagName: "cli-v2.0.0", PublishedAt: now.AddDate(0, 0, -2)},
		{TagName: "cli-v1.0.0", PublishedAt: now.AddDate(0, 0, -30)},
	}}

	release, ageFiltered, err := resolveReleaseWithMinAgeFilter(context.Background(), providerClient, "acme", "tool", 7, "cli-*")
	if err != nil {
		t.Fatalf("resolveReleaseWithMinAgeFilter() error = %v", err)
	}
	if release.TagName != "cli-v1.0.0" || !ageFiltered {
		t.Fatalf("selected release/ageFiltered = %q/%v, want cli-v1.0.0/true", release.TagName, ageFiltered)
	}
	if providerClient.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", providerClient.listCalls)
	}
}

func TestResolveReleaseWithMinAgeFilterReportsFilterWhenNoReleaseQualifies(t *testing.T) {
	providerClient := &releaseFilterTestProvider{listed: []provider.Release{
		{TagName: "server-v1.0.0", PublishedAt: time.Now().UTC().AddDate(0, 0, -30)},
	}}

	_, _, err := resolveReleaseWithMinAgeFilter(context.Background(), providerClient, "acme", "tool", 7, "cli-*")
	if err == nil || !strings.Contains(err.Error(), "cli-*") {
		t.Fatalf("no qualifying release error = %v, want filter text", err)
	}
}

func TestRunInstallPersistsAndOverridesReleaseFilter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))

	providerClient := &releaseFilterTestProvider{
		listed: []provider.Release{
			{TagName: "server-v9.0.0", PublishedAt: time.Now().UTC(), Assets: []provider.Asset{{Name: "widget-linux-amd64"}}},
			{TagName: "cli-v1.0.0", PublishedAt: time.Now().UTC().Add(-time.Hour), Assets: []provider.Asset{{Name: "widget-linux-amd64"}}},
		},
		body: []byte("cli v1"),
	}
	options := installOptions{
		releaseFilter:    "cli-*",
		releaseFilterSet: true,
		goos:             "linux",
		goarch:           "amd64",
		input:            strings.NewReader(""),
	}
	if err := runInstallWithOptions(context.Background(), providerClient, "acme/widget", options, io.Discard); err != nil {
		t.Fatalf("filtered install: %v", err)
	}
	app := mustReleaseFilterApp(t, "acme/widget")
	if app.Version != "cli-v1.0.0" || app.ReleaseFilter != "cli-*" {
		t.Fatalf("filtered install state = %#v, want cli tag and filter", app)
	}

	providerClient.listed = []provider.Release{
		{TagName: "server-v9.0.0", PublishedAt: time.Now().UTC(), Assets: []provider.Asset{{Name: "widget-linux-amd64"}}},
		{TagName: "cli-v2.0.0", PublishedAt: time.Now().UTC().Add(-time.Hour), Assets: []provider.Asset{{Name: "widget-linux-amd64"}}},
	}
	providerClient.body = []byte("cli v2")
	if err := runInstallWithOptions(context.Background(), providerClient, "acme/widget", installOptions{
		goos:   "linux",
		goarch: "amd64",
		input:  strings.NewReader(""),
	}, io.Discard); err != nil {
		t.Fatalf("state-filtered upgrade: %v", err)
	}
	app = mustReleaseFilterApp(t, "acme/widget")
	if app.Version != "cli-v2.0.0" || app.ReleaseFilter != "cli-*" {
		t.Fatalf("state-filtered upgrade state = %#v, want cli v2 and retained filter", app)
	}

	providerClient.listed = []provider.Release{{
		TagName: "server-v3.0.0", PublishedAt: time.Now().UTC(), Assets: []provider.Asset{{Name: "widget-linux-amd64"}},
	}}
	providerClient.latest = &provider.Release{
		TagName: "server-v3.0.0", PublishedAt: time.Now().UTC(), Assets: []provider.Asset{{Name: "widget-linux-amd64"}},
	}
	providerClient.body = []byte("server v3")
	if err := runInstallWithOptions(context.Background(), providerClient, "acme/widget", installOptions{
		releaseFilter:    "server-*",
		releaseFilterSet: true,
		goos:             "linux",
		goarch:           "amd64",
		input:            strings.NewReader(""),
	}, io.Discard); err != nil {
		t.Fatalf("explicit-filter upgrade: %v", err)
	}
	app = mustReleaseFilterApp(t, "acme/widget")
	if app.Version != "server-v3.0.0" || app.ReleaseFilter != "server-*" {
		t.Fatalf("explicit-filter state = %#v, want server v3 and filter", app)
	}

	if err := runInstallWithOptions(context.Background(), providerClient, "acme/widget", installOptions{
		releaseFilterSet: true,
		goos:             "linux",
		goarch:           "amd64",
		input:            strings.NewReader(""),
	}, io.Discard); err != nil {
		t.Fatalf("empty-filter clear: %v", err)
	}
	app = mustReleaseFilterApp(t, "acme/widget")
	if app.ReleaseFilter != "" {
		t.Fatalf("cleared filter = %q, want empty", app.ReleaseFilter)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state before pinned filter update: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state does not contain app before pinned filter update")
	}
	app.Pinned = true
	st.Set("acme/widget", app)
	if err := st.Save(); err != nil {
		t.Fatalf("save pinned state: %v", err)
	}
	providerClient.listed = []provider.Release{{
		TagName: "cli-v4.0.0", PublishedAt: time.Now().UTC(), Assets: []provider.Asset{{Name: "widget-linux-amd64"}},
	}}
	var output bytes.Buffer
	if err := runInstallWithOptions(context.Background(), providerClient, "acme/widget", installOptions{
		releaseFilter:    "cli-*",
		releaseFilterSet: true,
		goos:             "linux",
		goarch:           "amd64",
		input:            strings.NewReader(""),
	}, &output); err != nil || !strings.Contains(output.String(), "pinned") {
		t.Fatalf("pinned filter update output/error = %q/%v, want pinned message", output.String(), err)
	}
	app = mustReleaseFilterApp(t, "acme/widget")
	if !app.Pinned || app.Version != "server-v3.0.0" || app.ReleaseFilter != "cli-*" {
		t.Fatalf("pinned filter state = %#v, want pinned server v3 with cli filter", app)
	}
}

func TestFetchLatestReleasesUsesStoredReleaseFilter(t *testing.T) {
	now := time.Now().UTC()
	providerClient := &releaseFilterTestProvider{
		latest: &provider.Release{TagName: "server-v9.0.0", PublishedAt: now},
		listed: []provider.Release{
			{TagName: "server-v8.0.0", PublishedAt: now},
			{TagName: "cli-v2.0.0", PublishedAt: now.Add(-time.Hour)},
		},
	}
	latest := fetchLatestReleases(context.Background(), providerClient, map[string]state.App{
		"acme/widget": {ReleaseFilter: "cli-*"},
	}, now)
	info := latest["acme/widget"]
	if info.Version == nil || *info.Version != "cli-v2.0.0" {
		t.Fatalf("latest filtered version = %#v, want cli-v2.0.0", info.Version)
	}
	if providerClient.latestCalls != 0 || providerClient.listCalls != 1 {
		t.Fatalf("list update provider calls = latest %d/list %d, want 0/1", providerClient.latestCalls, providerClient.listCalls)
	}
}

func TestLookupUpgradeReleasesUsesStoredReleaseFilter(t *testing.T) {
	var latestRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases":
			_ = json.NewEncoder(w).Encode([]provider.Release{
				{TagName: "server-v9.0.0", PublishedAt: time.Now().UTC()},
				{TagName: "cli-v2.0.0", PublishedAt: time.Now().UTC().Add(-time.Hour)},
			})
		case "/repos/acme/widget/releases/latest":
			latestRequests++
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := github.New()
	client.BaseURL = server.URL
	st := &state.State{Apps: map[string]state.App{
		"acme/widget": {Version: "cli-v1.0.0", ReleaseFilter: "cli-*"},
	}}

	lookups := lookupUpgradeReleasesWithState(context.Background(), client, []string{"acme/widget"}, st)
	lookup := lookups["acme/widget"]
	if lookup.err != nil {
		t.Fatalf("filtered upgrade lookup error = %v", lookup.err)
	}
	if lookup.release == nil || lookup.release.TagName != "cli-v2.0.0" {
		t.Fatalf("filtered upgrade release = %#v, want cli-v2.0.0", lookup.release)
	}
	if latestRequests != 0 {
		t.Fatalf("latest endpoint requests = %d, want 0", latestRequests)
	}
}

func mustReleaseFilterApp(t *testing.T, repository string) state.App {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get(repository)
	if !ok {
		t.Fatalf("state does not contain %s", repository)
	}
	return app
}

func TestReleaseFilterStateJSONIsOmittedWhenEmpty(t *testing.T) {
	data := []byte(`{"apps":{"acme/widget":{"version":"v1.0.0","release_filter":"cli-*"}}}`)
	var st state.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal release filter state: %v", err)
	}
	if got := st.Apps["acme/widget"].ReleaseFilter; got != "cli-*" {
		t.Fatalf("release filter = %q, want cli-*", got)
	}
	encoded, err := json.Marshal(state.State{Apps: map[string]state.App{"acme/widget": {Version: "v1.0.0"}}})
	if err != nil {
		t.Fatalf("marshal empty release filter state: %v", err)
	}
	if bytes.Contains(encoded, []byte("release_filter")) {
		t.Fatalf("empty release filter was serialized: %s", encoded)
	}
}

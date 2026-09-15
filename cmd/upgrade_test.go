package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zenget/internal/appcfg"
	"zenget/internal/github"
	"zenget/internal/hashx"
	"zenget/internal/install"
	"zenget/internal/state"
)

type upgradeFixture struct {
	tag            string
	assetName      string
	assetBody      []byte
	latestStatus   int
	assetStatus    int
	latestRequests atomic.Int32
	assetRequests  atomic.Int32
}

func newUpgradeTestServer(t *testing.T, fixtures map[string]*upgradeFixture) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 5 && parts[0] == "repos" && parts[3] == "releases" && parts[4] == "latest" {
			fixture := fixtures[parts[1]+"/"+parts[2]]
			if fixture == nil {
				http.NotFound(w, r)
				return
			}
			fixture.latestRequests.Add(1)
			if fixture.latestStatus != 0 && fixture.latestStatus != http.StatusOK {
				w.WriteHeader(fixture.latestStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": fixture.tag,
				"assets": []map[string]string{{
					"name":                 fixture.assetName,
					"browser_download_url": server.URL + "/assets/" + fixture.assetName,
				}},
			})
			return
		}
		if len(parts) == 2 && parts[0] == "assets" {
			for _, fixture := range fixtures {
				if fixture.assetName != parts[1] {
					continue
				}
				fixture.assetRequests.Add(1)
				if fixture.assetStatus != 0 && fixture.assetStatus != http.StatusOK {
					w.WriteHeader(fixture.assetStatus)
					return
				}
				_, _ = w.Write(fixture.assetBody)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func seedUpgradeApp(t *testing.T, repository, version string, binary []byte, pinned bool, targetName string) state.App {
	t.Helper()
	wrapperPath, realPath, err := install.Install(binary, repository, false, targetName)
	if err != nil {
		t.Fatalf("install seed %s: %v", repository, err)
	}
	return state.App{
		Version:      version,
		InstalledAt:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		DownloadURL:  "https://example.invalid/asset",
		InstallPath:  wrapperPath,
		RealPath:     realPath,
		XXH3_64:      hashx.Bytes(binary),
		Pinned:       pinned,
		PlatformOS:   runtime.GOOS,
		PlatformArch: runtime.GOARCH,
	}
}

func saveUpgradeState(t *testing.T, apps map[string]state.App) {
	t.Helper()
	if err := (&state.State{Apps: apps}).Save(); err != nil {
		t.Fatalf("save state: %v", err)
	}
}

func newUpgradeClient(server *httptest.Server) *github.Client {
	client := github.New()
	client.BaseURL = server.URL
	return client
}

func TestUpgradeAllAndSelectedRepositories(t *testing.T) {
	setUpgradeEnvironment(t)
	apps := map[string]state.App{
		"acme/alpha": seedUpgradeApp(t, "acme/alpha", "v1.0.0", []byte("alpha old"), false, "alpha"),
		"acme/beta":  seedUpgradeApp(t, "acme/beta", "v1.0.0", []byte("beta old"), false, "beta"),
	}
	saveUpgradeState(t, apps)
	fixtures := map[string]*upgradeFixture{
		"acme/alpha": {tag: "v2.0.0", assetName: "alpha", assetBody: []byte("alpha new")},
		"acme/beta":  {tag: "v2.0.0", assetName: "beta", assetBody: []byte("beta new")},
	}
	server := newUpgradeTestServer(t, fixtures)

	var output bytes.Buffer
	if err := runUpgradeWithOptions(context.Background(), newUpgradeClient(server), nil, upgradeOptions{yes: true}, &output); err != nil {
		t.Fatalf("upgrade all: %v", err)
	}
	if !strings.Contains(output.String(), "acme/alpha: v1.0.0 -> v2.0.0") ||
		!strings.Contains(output.String(), "acme/beta: v1.0.0 -> v2.0.0") ||
		!strings.Contains(output.String(), "Summary: updated=2 unchanged=0 skipped=0 failed=0") {
		t.Fatalf("upgrade output = %q", output.String())
	}
	assertUpgradeBinary(t, "acme/alpha", []byte("alpha new"))
	assertUpgradeBinary(t, "acme/beta", []byte("beta new"))

	// A selected repository is the only one looked up and executed.
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state after all upgrade: %v", err)
	}
	st.Apps["acme/alpha"] = seedUpgradeApp(t, "acme/alpha", "v2.0.0", []byte("alpha new"), false, "alpha")
	saveUpgradeState(t, st.Apps)
	fixtures["acme/alpha"].tag = "v3.0.0"
	fixtures["acme/beta"].tag = "v2.0.0"
	fixtures["acme/beta"].assetBody = []byte("unexpected beta download")
	output.Reset()
	if err := runUpgradeWithOptions(context.Background(), newUpgradeClient(server), []string{"acme/alpha"}, upgradeOptions{yes: true}, &output); err != nil {
		t.Fatalf("selected upgrade: %v", err)
	}
	if fixtures["acme/beta"].latestRequests.Load() != 1 {
		t.Fatalf("selected upgrade looked up beta %d times", fixtures["acme/beta"].latestRequests.Load())
	}
	assertUpgradeBinary(t, "acme/alpha", []byte("alpha new"))
}

func TestUpgradePlanHandlesUnchangedPinnedAndFailedEntries(t *testing.T) {
	setUpgradeEnvironment(t)
	oldPinned := []byte("pinned")
	oldCurrent := []byte("current")
	oldUpdate := []byte("update old")
	oldFailed := []byte("failed old")
	apps := map[string]state.App{
		"acme/current": seedUpgradeApp(t, "acme/current", "v2.0.0", oldCurrent, false, "current"),
		"acme/failed":  seedUpgradeApp(t, "acme/failed", "v1.0.0", oldFailed, false, "failed"),
		"acme/pinned":  seedUpgradeApp(t, "acme/pinned", "v1.0.0", oldPinned, true, "pinned"),
		"acme/update":  seedUpgradeApp(t, "acme/update", "v1.0.0", oldUpdate, false, "update"),
	}
	saveUpgradeState(t, apps)
	fixtures := map[string]*upgradeFixture{
		"acme/current": {tag: "v2.0.0", assetName: "current", assetBody: []byte("unused")},
		"acme/failed":  {tag: "v2.0.0", assetName: "failed", assetStatus: http.StatusInternalServerError},
		"acme/pinned":  {tag: "v2.0.0", assetName: "pinned", assetBody: []byte("must not download")},
		"acme/update":  {tag: "v2.0.0", assetName: "update", assetBody: []byte("update new")},
	}
	server := newUpgradeTestServer(t, fixtures)

	var output bytes.Buffer
	err := runUpgradeWithOptions(context.Background(), newUpgradeClient(server), nil, upgradeOptions{yes: true}, &output)
	if err == nil {
		t.Fatal("upgrade error = nil, want partial failure")
	}
	wantLines := []string{
		"acme/current: v2.0.0 -> v2.0.0 [unchanged]",
		"acme/failed: v1.0.0 -> v2.0.0 [update]",
		"acme/pinned: v1.0.0 -> - [skipped] (pinned)",
		"acme/update: v1.0.0 -> v2.0.0 [update]",
		"Summary: updated=1 unchanged=1 skipped=1 failed=1",
	}
	for _, want := range wantLines {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
	if fixtures["acme/pinned"].latestRequests.Load() != 0 || fixtures["acme/pinned"].assetRequests.Load() != 0 {
		t.Fatal("pinned application triggered a network request")
	}
	assertUpgradeBinary(t, "acme/pinned", oldPinned)
	assertUpgradeBinary(t, "acme/update", []byte("update new"))
	assertUpgradeBinary(t, "acme/failed", oldFailed)
}

func TestUpgradeDryRunDoesNotMutateOrDownload(t *testing.T) {
	setUpgradeEnvironment(t)
	oldBinary := []byte("old binary")
	app := seedUpgradeApp(t, "acme/widget", "v1.0.0", oldBinary, false, "widget")
	saveUpgradeState(t, map[string]state.App{"acme/widget": app})
	statePath, err := state.Path()
	if err != nil {
		t.Fatalf("state path: %v", err)
	}
	oldState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	oldConfig, err := appcfg.Path("acme/widget")
	if err != nil {
		t.Fatalf("app config path: %v", err)
	}
	if _, err := os.Stat(oldConfig); !os.IsNotExist(err) {
		t.Fatalf("unexpected app config fixture: %v", err)
	}
	fixtures := map[string]*upgradeFixture{
		"acme/widget": {tag: "v2.0.0", assetName: "widget", assetBody: []byte("new binary")},
	}
	server := newUpgradeTestServer(t, fixtures)

	var output bytes.Buffer
	if err := runUpgradeWithOptions(context.Background(), newUpgradeClient(server), nil, upgradeOptions{dryRun: true}, &output); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !strings.Contains(output.String(), "acme/widget: v1.0.0 -> v2.0.0 [update]") ||
		!strings.Contains(output.String(), "Summary: planned=1 unchanged=0 skipped=0 failed=0") {
		t.Fatalf("dry-run output = %q", output.String())
	}
	if got, err := os.ReadFile(statePath); err != nil || !bytes.Equal(got, oldState) {
		t.Fatalf("state changed during dry-run: %q, err=%v", got, err)
	}
	assertUpgradeBinary(t, "acme/widget", oldBinary)
	if fixtures["acme/widget"].assetRequests.Load() != 0 {
		t.Fatalf("dry-run downloaded asset %d times", fixtures["acme/widget"].assetRequests.Load())
	}
}

func TestUpgradeNonInteractiveRequiresYesBeforeMutation(t *testing.T) {
	setUpgradeEnvironment(t)
	oldBinary := []byte("old binary")
	app := seedUpgradeApp(t, "acme/widget", "v1.0.0", oldBinary, false, "widget")
	saveUpgradeState(t, map[string]state.App{"acme/widget": app})
	fixtures := map[string]*upgradeFixture{
		"acme/widget": {tag: "v2.0.0", assetName: "widget", assetBody: []byte("new binary")},
	}
	server := newUpgradeTestServer(t, fixtures)

	err := runUpgradeWithOptions(context.Background(), newUpgradeClient(server), nil, upgradeOptions{input: strings.NewReader("y\n")}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v, want --yes guidance", err)
	}
	if fixtures["acme/widget"].assetRequests.Load() != 0 {
		t.Fatal("non-interactive refusal downloaded an asset")
	}
	assertUpgradeBinary(t, "acme/widget", oldBinary)
}

func TestUpgradePreservesSavedAssetTargetAndPlatformChoices(t *testing.T) {
	setUpgradeEnvironment(t)
	oldBinary := []byte("old binary")
	app := seedUpgradeApp(t, "acme/widget", "v1.0.0", oldBinary, false, "custom-widget")
	saveUpgradeState(t, map[string]state.App{"acme/widget": app})
	if err := (&appcfg.AppConfig{Asset: "widget-linux", TargetName: "custom-widget"}).Save("acme/widget"); err != nil {
		t.Fatalf("save app config: %v", err)
	}
	fixture := &upgradeFixture{tag: "v2.0.0", assetName: "widget-linux", assetBody: []byte("new binary")}
	server := newUpgradeTestServer(t, map[string]*upgradeFixture{"acme/widget": fixture})

	var output bytes.Buffer
	if err := runUpgradeWithOptions(context.Background(), newUpgradeClient(server), nil, upgradeOptions{yes: true}, &output); err != nil {
		t.Fatalf("upgrade with saved choices: %v", err)
	}
	if fixture.assetRequests.Load() != 1 {
		t.Fatalf("asset requests = %d, want 1", fixture.assetRequests.Load())
	}
	assertUpgradeBinary(t, "acme/widget", []byte("new binary"))
	updated, err := state.Load()
	if err != nil {
		t.Fatalf("load updated state: %v", err)
	}
	got := updated.Apps["acme/widget"]
	if got.InstallPath != app.InstallPath || got.PlatformOS != runtime.GOOS || got.PlatformArch != runtime.GOARCH {
		t.Fatalf("updated state choices = %#v, want install path %q and platform %s/%s", got, app.InstallPath, runtime.GOOS, runtime.GOARCH)
	}
}

func TestUpgradeRejectsInvalidArguments(t *testing.T) {
	if err := validateUpgradeRepositories([]string{"not-a-repository"}); err == nil || !strings.Contains(err.Error(), "expected org/repo") {
		t.Fatalf("validateUpgradeRepositories() error = %v", err)
	}
}

func setUpgradeEnvironment(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
}

func assertUpgradeBinary(t *testing.T, repository string, want []byte) {
	t.Helper()
	app, ok := loadUpgradeApp(t, repository)
	if !ok {
		t.Fatalf("state does not contain %s", repository)
	}
	got, err := os.ReadFile(app.RealPath)
	if err != nil {
		t.Fatalf("read %s binary: %v", repository, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s binary = %q, want %q", repository, got, want)
	}
}

func loadUpgradeApp(t *testing.T, repository string) (state.App, bool) {
	t.Helper()
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get(repository)
	return app, ok
}

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zenget/internal/github"
	"zenget/internal/state"
)

func TestPinAndUnpinCommandsAreIdempotent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st := &state.State{}
	st.Set("acme/widget", state.App{Version: "v1.2.3"})
	if err := st.Save(); err != nil {
		t.Fatalf("save state fixture: %v", err)
	}

	var output bytes.Buffer
	if err := setPinned("acme/widget", true, &output); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if !strings.Contains(output.String(), "Pinned acme/widget at v1.2.3") {
		t.Fatalf("pin output = %q", output.String())
	}
	output.Reset()
	if err := setPinned("acme/widget", true, &output); err != nil {
		t.Fatalf("repeat pin: %v", err)
	}
	if !strings.Contains(output.String(), "already pinned") {
		t.Fatalf("repeat pin output = %q", output.String())
	}

	output.Reset()
	if err := setPinned("acme/widget", false, &output); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if !strings.Contains(output.String(), "Unpinned acme/widget") {
		t.Fatalf("unpin output = %q", output.String())
	}
	output.Reset()
	if err := setPinned("acme/widget", false, &output); err != nil {
		t.Fatalf("repeat unpin: %v", err)
	}
	if !strings.Contains(output.String(), "already unpinned") {
		t.Fatalf("repeat unpin output = %q", output.String())
	}
}

func TestPinAndUnpinRejectUnknownRepositories(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, pinned := range []bool{true, false} {
		err := setPinned("acme/missing", pinned, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "acme/missing is not installed") {
			t.Fatalf("setPinned(%v) error = %v", pinned, err)
		}
	}
}

func TestPinnedInstallSkipsImplicitUpgradeAndAllowsExplicitTag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var latestTag atomic.Value
	latestTag.Store("v2.0.0")
	var assetBody atomic.Value
	assetBody.Store([]byte("version two"))
	var assetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			writePinTestRelease(w, r, latestTag.Load().(string))
		case "/repos/acme/widget/releases/tags/v3.0.0":
			writePinTestRelease(w, r, "v3.0.0")
		case "/assets/widget":
			assetRequests.Add(1)
			_, _ = w.Write(assetBody.Load().([]byte))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	assetBody.Store([]byte("version one"))
	latestTag.Store("v1.0.0")
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("initial install: %v", err)
	}
	if err := setPinned("acme/widget", true, io.Discard); err != nil {
		t.Fatalf("pin: %v", err)
	}

	assetRequests.Store(0)
	latestTag.Store("v2.0.0")
	assetBody.Store([]byte("version two"))
	var output bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("pinned implicit install: %v", err)
	}
	if !strings.Contains(output.String(), "pinned") || !strings.Contains(output.String(), "unpin") || !strings.Contains(output.String(), "--tag") {
		t.Fatalf("pinned output = %q", output.String())
	}
	if got := assetRequests.Load(); got != 0 {
		t.Fatalf("asset requests for pinned install = %d, want 0", got)
	}

	output.Reset()
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "v3.0.0", "", "", &output); err != nil {
		t.Fatalf("explicit tagged install: %v", err)
	}
	if !strings.Contains(output.String(), "Upgraded widget v1.0.0 -> v3.0.0") {
		t.Fatalf("explicit tag output = %q", output.String())
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok || !app.Pinned || app.Version != "v3.0.0" {
		t.Fatalf("state after explicit tag = %#v, present = %v", app, ok)
	}

	if err := setPinned("acme/widget", false, io.Discard); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	latestTag.Store("v4.0.0")
	assetBody.Store([]byte("version four"))
	output.Reset()
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", &output); err != nil {
		t.Fatalf("install after unpin: %v", err)
	}
	if !strings.Contains(output.String(), "Upgraded widget v3.0.0 -> v4.0.0") {
		t.Fatalf("post-unpin output = %q", output.String())
	}
	st, err = state.Load()
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	app, ok = st.Get("acme/widget")
	if !ok || app.Pinned || app.Version != "v4.0.0" {
		t.Fatalf("state after unpin upgrade = %#v, present = %v", app, ok)
	}
}

func TestPinnedStateIsShownByListTableAndJSON(t *testing.T) {
	apps := map[string]state.App{
		"acme/widget": {Version: "v1.2.3", Pinned: true, InstallPath: "/home/u/.local/bin/widget"},
	}
	saveFixtureState(t, apps)

	var table bytes.Buffer
	if err := printList(mustLoadState(t), nil, &table, false, timeNowForPinTest()); err != nil {
		t.Fatalf("list table: %v", err)
	}
	if !strings.Contains(table.String(), "v1.2.3 (pinned)") {
		t.Fatalf("table output = %q", table.String())
	}

	var encoded bytes.Buffer
	if err := printList(mustLoadState(t), nil, &encoded, true, timeNowForPinTest()); err != nil {
		t.Fatalf("list JSON: %v", err)
	}
	var got map[string]state.App
	if err := json.Unmarshal(encoded.Bytes(), &got); err != nil {
		t.Fatalf("parse list JSON: %v", err)
	}
	if !got["acme/widget"].Pinned {
		t.Fatalf("JSON app = %#v, want pinned", got["acme/widget"])
	}
}

func TestPinnedListUpdatesStillShowsLatestRelease(t *testing.T) {
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.2.3", Pinned: true, InstallPath: "/home/u/.local/bin/widget"},
	})
	now := timeNowForPinTest()
	server := newListTestServer(t, func(path string) map[string]any {
		if path == "/repos/acme/widget/releases/latest" {
			return map[string]any{"tag_name": "v1.3.0", "published_at": "2026-09-01T00:00:00Z"}
		}
		return nil
	})

	output, err := runListWithTestFlags(t, newListTestClient(t, server), true, false, colorModeNever, now)
	if err != nil {
		t.Fatalf("list --updates: %v", err)
	}
	if !strings.Contains(output.String(), "v1.2.3 (pinned)") || !strings.Contains(output.String(), "v1.3.0") {
		t.Fatalf("pinned updates output = %q", output.String())
	}
}

func writePinTestRelease(w http.ResponseWriter, r *http.Request, tag string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tag_name": tag,
		"assets": []map[string]string{{
			"name":                 "widget",
			"browser_download_url": "http://" + r.Host + "/assets/widget",
		}},
	})
}

func timeNowForPinTest() time.Time {
	return time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
}

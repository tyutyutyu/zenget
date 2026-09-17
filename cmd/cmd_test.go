package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/github"
	"zenget/internal/state"
)

// newBufferedCommand returns a command whose standard and error outputs are
// captured in the returned buffers.
func newBufferedCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var output, errOut bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&errOut)
	return cmd, &output, &errOut
}

func TestRunInstallRejectsInvalidRepository(t *testing.T) {
	err := runInstall(&cobra.Command{}, []string{"badrepo"})
	if err == nil || !strings.Contains(err.Error(), `invalid repository "badrepo"`) {
		t.Fatalf("error = %v, want invalid repository", err)
	}
}

func TestRunInstallWithoutArguments(t *testing.T) {
	err := runInstall(&cobra.Command{}, nil)
	if err == nil || !strings.Contains(err.Error(), `invalid repository ""`) {
		t.Fatalf("error = %v, want invalid repository", err)
	}
}

func TestRunUninstallRejectsInvalidRepository(t *testing.T) {
	err := runUninstall(&cobra.Command{}, []string{"badrepo"})
	if err == nil || !strings.Contains(err.Error(), `invalid repository "badrepo"`) {
		t.Fatalf("error = %v, want invalid repository", err)
	}
}

func TestRunUninstallNotInstalledViaCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cmd, _, _ := newBufferedCommand()
	err := runUninstall(cmd, []string{"acme/widget"})
	if err == nil || !strings.Contains(err.Error(), "acme/widget is not installed") {
		t.Fatalf("error = %v, want not installed", err)
	}
}

func TestRunUninstallHashErrorOnDirectoryTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A directory at the install path makes hashing fail.
	target := home + "/.local/bin/widget"
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstallPath: target},
	})

	err := runUninstallWithWriters("acme/widget", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "hash installed binary") {
		t.Fatalf("error = %v, want hash error", err)
	}
}

func TestRunListLoadError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	err := runList(&cobra.Command{}, nil)
	if err == nil || !strings.Contains(err.Error(), "load state") {
		t.Fatalf("error = %v, want load state error", err)
	}
}

func TestRunListTableViaCommand(t *testing.T) {
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstallPath: "/home/u/.local/bin/widget"},
	})

	cmd, output, _ := newBufferedCommand()
	if err := runList(cmd, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"NAME", "acme/widget", "v1.0.0"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("table output missing %q:\n%s", want, output.String())
		}
	}
}

func TestRunListJSONViaCommand(t *testing.T) {
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstallPath: "/home/u/.local/bin/widget"},
	})

	listJSON = true
	t.Cleanup(func() { listJSON = false })

	cmd, output, _ := newBufferedCommand()
	if err := runList(cmd, nil); err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("parse JSON output: %v", err)
	}
	if _, ok := got["acme/widget"]; !ok {
		t.Fatalf("JSON output missing acme/widget: %s", output.String())
	}
}

func TestExecuteHelpSucceeds(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{"zenget", "--help"}

	rootCmd.SetOut(io.Discard)
	t.Cleanup(func() { rootCmd.SetOut(nil) })

	Execute()
}

func TestRunInstallWithClientNilClient(t *testing.T) {
	err := runInstallWithClient(context.Background(), nil, "acme/widget", "", "", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "GitHub client is nil") {
		t.Fatalf("error = %v, want nil client error", err)
	}
}

// releaseServer starts a test server that serves the latest release endpoint
// from release and proxies asset downloads to assetHandler.
func releaseServer(t *testing.T, release map[string]any, assetHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			if release == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		assetHandler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func releasePayload(server *httptest.Server, assetName string) map[string]any {
	return map[string]any{
		"tag_name": "v1.0.0",
		"assets": []map[string]string{{
			"name":                 assetName,
			"browser_download_url": server.URL + "/assets/" + assetName,
		}},
	}
}

func TestRunInstallWithClientReleaseFetchError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	server := releaseServer(t, nil, http.NotFound)

	client := github.New()
	client.BaseURL = server.URL
	err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "fetch latest release for acme/widget") {
		t.Fatalf("error = %v, want fetch latest release error", err)
	}
}

func TestRunInstallWithClientNoAssets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	server := releaseServer(t, map[string]any{"tag_name": "v1.0.0", "assets": []map[string]string{}}, http.NotFound)

	client := github.New()
	client.BaseURL = server.URL
	err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), `release "v1.0.0" has no downloadable assets`) {
		t.Fatalf("error = %v, want no assets error", err)
	}
}

func TestRunInstallWithClientDownloadError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	server := releaseServer(t, nil, http.NotFound)
	// The release payload needs the final server URL, so build it after start.
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = json.NewEncoder(w).Encode(releasePayload(server, "widget"))
			return
		}
		http.NotFound(w, r)
	})

	client := github.New()
	client.BaseURL = server.URL
	err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), `download release asset "widget"`) {
		t.Fatalf("error = %v, want download error", err)
	}
}

func TestRunInstallWithClientExtractError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	server := releaseServer(t, nil, http.NotFound)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = json.NewEncoder(w).Encode(releasePayload(server, "widget.tar.gz"))
			return
		}
		// Not a valid gzip stream.
		_, _ = w.Write([]byte("garbage"))
	})

	client := github.New()
	client.BaseURL = server.URL
	err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), `extract release asset "widget.tar.gz"`) {
		t.Fatalf("error = %v, want extract error", err)
	}
}

func TestRunInstallWithClientNilContextAndOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	binary := []byte("#!/bin/sh\necho widget\n")
	asset := tarGzAsset(t, "widget", binary)
	server := releaseServer(t, nil, http.NotFound)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widget/releases/latest" {
			_ = json.NewEncoder(w).Encode(releasePayload(server, "widget.tar.gz"))
			return
		}
		_, _ = w.Write(asset)
	})

	client := github.New()
	client.BaseURL = server.URL
	if err := runInstallWithClient(nil, client, "acme/widget", "", "", "", "", nil); err != nil { //nolint:staticcheck // this test covers nil-context normalization
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(home + "/.local/bin/widget"); err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
}

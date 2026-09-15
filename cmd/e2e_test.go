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
	"strings"
	"sync/atomic"
	"testing"

	"zenget/internal/github"
	"zenget/internal/hashx"
	"zenget/internal/state"
)

// TestEndToEndInstallListUninstall drives the full user flow against a mock
// GitHub server: install, list, upgrade to a new release, then uninstall.
func TestEndToEndInstallListUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZENGET_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	binaryV1 := []byte("#!/bin/sh\necho hi\n")
	binaryV2 := []byte("#!/bin/sh\necho hi from v2\n")

	var tag atomic.Value
	tag.Store("v1.0.0")
	var assetBody atomic.Value
	assetBody.Store(tarGzAsset(t, "tool", binaryV1))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/tool/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag.Load().(string),
				"assets": []map[string]string{{
					"name":                 "tool-linux-amd64.tar.gz",
					"browser_download_url": "http://" + r.Host + "/download",
				}},
			})
		case "/download":
			_, _ = w.Write(assetBody.Load().([]byte))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := github.New()
	client.BaseURL = server.URL
	target := filepath.Join(home, ".local", "bin", "tool")

	listOutput := func(t *testing.T) string {
		t.Helper()
		cmd, output, _ := newBufferedCommand()
		if err := runList(cmd, nil); err != nil {
			t.Fatalf("list: %v", err)
		}
		return output.String()
	}

	// Install v1.0.0.
	var installOut bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/tool", "", "", "", "", &installOut); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(installOut.String(), "Installed tool v1.0.0 to ") {
		t.Fatalf("install output = %q", installOut.String())
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("installed wrapper missing: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0755); got != want {
		t.Fatalf("installed mode = %o, want %o", got, want)
	}
	wrapper, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(wrapper), "zenget-managed wrapper") {
		t.Fatalf("installed wrapper = %q, err = %v", wrapper, err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	app, ok := st.Get("acme/tool")
	if !ok {
		t.Fatal("state does not contain acme/tool")
	}
	realBin := app.RealPath
	gotBinary, err := os.ReadFile(realBin)
	if err != nil || !bytes.Equal(gotBinary, binaryV1) {
		t.Fatalf("installed real binary = %q, err = %v, want %q", gotBinary, err, binaryV1)
	}
	if app.Version != "v1.0.0" || app.XXH3_64 != hashx.Bytes(binaryV1) || app.InstallPath != target || app.RealPath != realBin {
		t.Fatalf("state app = %#v", app)
	}

	// List shows the installed app.
	out := listOutput(t)
	for _, want := range []string{"acme/tool", "v1.0.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}

	// Upgrade: the mock now serves v2.0.0 with different binary content.
	tag.Store("v2.0.0")
	assetBody.Store(tarGzAsset(t, "tool", binaryV2))
	installOut.Reset()
	if err := runInstallWithClient(context.Background(), client, "acme/tool", "", "", "", "", &installOut); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if got, want := installOut.String(), "Upgraded tool v1.0.0 -> v2.0.0\n"; got != want {
		t.Fatalf("upgrade output = %q, want %q", got, want)
	}
	if got, err := os.ReadFile(target); err != nil || !strings.Contains(string(got), "zenget-managed wrapper") {
		t.Fatalf("upgraded wrapper = %q, err = %v", got, err)
	}

	st, err = state.Load()
	if err != nil {
		t.Fatalf("load state after upgrade: %v", err)
	}
	app, ok = st.Get("acme/tool")
	if !ok || app.Version != "v2.0.0" || app.XXH3_64 != hashx.Bytes(binaryV2) {
		t.Fatalf("state after upgrade = %#v, exists = %v", app, ok)
	}
	newRealBin := app.RealPath
	if newRealBin == realBin {
		t.Fatalf("upgrade reused active artifact path %q", newRealBin)
	}
	if got, err := os.ReadFile(newRealBin); err != nil || !bytes.Equal(got, binaryV2) {
		t.Fatalf("upgraded real binary = %q, err = %v, want %q", got, err, binaryV2)
	}
	if _, err := os.Stat(realBin); err != nil {
		t.Fatalf("previous real binary should be retained: %v", err)
	}
	realBin = newRealBin

	// Uninstall removes the binary and the registry entry.
	var uninstallOut bytes.Buffer
	if err := runUninstallWithWriters("acme/tool", &uninstallOut, io.Discard); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("wrapper still present after uninstall, stat err = %v", err)
	}
	if _, err := os.Stat(realBin); !os.IsNotExist(err) {
		t.Fatalf("real binary still present after uninstall, stat err = %v", err)
	}

	st, err = state.Load()
	if err != nil {
		t.Fatalf("load state after uninstall: %v", err)
	}
	if len(st.Apps) != 0 {
		t.Fatalf("state after uninstall = %#v, want empty", st.Apps)
	}
	if out := listOutput(t); !strings.Contains(out, "No applications installed.") {
		t.Fatalf("list after uninstall = %q", out)
	}
}

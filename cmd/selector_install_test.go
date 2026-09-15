package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"zenget/internal/appcfg"
	"zenget/internal/github"
)

func TestRunInstallSelectorPersistsAndReusesAcrossReleaseVersions(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	binaryV1 := []byte("#!/bin/sh\necho selector-v1\n")
	binaryV2 := []byte("#!/bin/sh\necho selector-v2\n")
	assetV1 := tarGzAsset(t, "widget", binaryV1)
	assetV2 := tarGzAsset(t, "widget", binaryV2)
	var version atomic.Int32
	version.Store(1)
	var releaseRequests, downloadRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			releaseRequests.Add(1)
			v := version.Load()
			name := "widget-linux-amd64-v1.tar.gz"
			tag := "v1.0.0"
			if v == 2 {
				name = "widget-linux-amd64-v2.tar.gz"
				tag = "v2.0.0"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag,
				"assets": []map[string]any{
					{"id": 1, "name": name, "browser_download_url": server.URL + "/download"},
					{"id": 2, "name": strings.Replace(name, "amd64", "arm64", 1), "browser_download_url": server.URL + "/download"},
				},
			})
		case "/download":
			downloadRequests.Add(1)
			if version.Load() == 1 {
				_, _ = w.Write(assetV1)
			} else {
				_, _ = w.Write(assetV2)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := github.New()
	client.BaseURL = server.URL
	var firstOutput bytes.Buffer
	if err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		assetMatch: "linux-amd64", goos: "linux", goarch: "amd64",
		input: strings.NewReader(""),
	}, &firstOutput); err != nil {
		t.Fatalf("selector install: %v", err)
	}
	config, err := appcfg.Load("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if config.Asset != "" || config.AssetSelector == nil || config.AssetSelector.Type != appcfg.AssetSelectorSubstring || config.AssetSelector.Pattern != "linux-amd64" {
		t.Fatalf("saved selector config = %#v", config)
	}

	version.Store(2)
	var secondOutput bytes.Buffer
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "linux", "amd64", &secondOutput); err != nil {
		t.Fatalf("selector upgrade: %v", err)
	}
	if !strings.Contains(secondOutput.String(), "Upgraded widget v1.0.0 -> v2.0.0") {
		t.Fatalf("upgrade output = %q", secondOutput.String())
	}
	if releaseRequests.Load() != 2 || downloadRequests.Load() != 2 {
		t.Fatalf("requests = releases %d, downloads %d; want 2, 2", releaseRequests.Load(), downloadRequests.Load())
	}
}

func TestInstallSelectorValidationPrecedesClientAndState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := runInstallWithOptions(context.Background(), nil, "acme/widget", installOptions{
		assetMatchRegex: "[",
	}, nil); err == nil || !strings.Contains(err.Error(), "invalid asset selector regex") {
		t.Fatalf("invalid regex error = %v", err)
	}
	if err := runInstallWithOptions(context.Background(), nil, "acme/widget", installOptions{
		assetOverride: "widget", assetMatch: "linux",
	}, nil); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("selector conflict error = %v", err)
	}
	if _, err := os.Stat(filepathForSelectorTest(t, "acme/widget")); !os.IsNotExist(err) {
		t.Fatalf("validation created app config: %v", err)
	}
}

func filepathForSelectorTest(t *testing.T, repository string) string {
	t.Helper()
	path, err := appcfg.Path(repository)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"zenget/internal/appcfg"
	"zenget/internal/github"
	"zenget/internal/state"
)

func TestRunInstallWithOptionsInteractiveAssetAndNameChoices(t *testing.T) {
	home := t.TempDir()
	configHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{
					{"name": "widget-one", "browser_download_url": server.URL + "/one"},
					{"name": "widget-two", "browser_download_url": server.URL + "/two"},
				},
			})
		case "/one":
			_, _ = io.WriteString(w, "one")
		case "/two":
			_, _ = io.WriteString(w, "two")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		goos:        "linux",
		goarch:      "amd64",
		input:       strings.NewReader("2\ncustom-widget\n"),
		interactive: true,
	}, &output)
	if err != nil {
		t.Fatalf("interactive install: %v", err)
	}
	if !strings.Contains(output.String(), "1) widget-one") || !strings.Contains(output.String(), "2) widget-two") {
		t.Fatalf("interactive output = %q", output.String())
	}

	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, ok := st.Get("acme/widget")
	if !ok || !strings.HasSuffix(app.InstallPath, "custom-widget") {
		t.Fatalf("state app = %#v, exists = %v", app, ok)
	}
	cfg, err := appcfg.Load("acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Asset != "widget-two" || cfg.TargetName != "custom-widget" {
		t.Fatalf("app config = %#v", cfg)
	}
}

func TestResolveInstallAssetNonInteractiveAmbiguity(t *testing.T) {
	release := &github.Release{
		TagName: "v1.0.0",
		Assets:  []github.Asset{{Name: "one"}, {Name: "two"}},
	}
	_, _, _, err := resolveInstallAsset(release, "linux", "amd64", "", "", false, nil)
	if err == nil || !strings.Contains(err.Error(), "--asset") {
		t.Fatalf("resolveInstallAsset() error = %v, want --asset guidance", err)
	}
}

func TestRunInstallWithOptionsArchiveBinaryChoice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var server *httptest.Server
	archive := multiExecutableTarGzAsset(t, "first", "second")
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]string{{
					"name":                 "widget-linux-amd64.tar.gz",
					"browser_download_url": server.URL + "/asset",
				}},
			})
		case "/asset":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := github.New()
	client.BaseURL = server.URL
	var output bytes.Buffer
	err := runInstallWithOptions(context.Background(), client, "acme/widget", installOptions{
		goos:        "linux",
		goarch:      "amd64",
		input:       strings.NewReader("2\n\n"),
		interactive: true,
	}, &output)
	if err != nil {
		t.Fatalf("archive install: %v", err)
	}
	path := installedRealPath(t, "acme/widget")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("installed archive member = %q, want second", data)
	}
}

func multiExecutableTarGzAsset(t *testing.T, first, second string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range []struct {
		name string
		data string
	}{
		{"bin/first", first},
		{"bin/second", second},
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: member.name, Mode: 0755, Size: int64(len(member.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(member.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

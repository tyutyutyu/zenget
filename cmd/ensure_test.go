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

func TestRestoreReusesArtifactCacheForMissingBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var latestTag atomic.Value
	latestTag.Store("v1.0.0")
	var exactRequests, assetRequests atomic.Int32
	server := newEnsureTestServer(t, func(r *http.Request) (string, int) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			return latestTag.Load().(string), http.StatusOK
		case "/repos/acme/widget/releases/tags/v1.0.0":
			exactRequests.Add(1)
			return "v1.0.0", http.StatusOK
		case "/assets/widget":
			assetRequests.Add(1)
			return "version one", http.StatusOK
		default:
			return "", http.StatusNotFound
		}
	})
	client := newEnsureTestClient(server)
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state after install: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state does not contain acme/widget")
	}
	realPath := app.RealPath
	wrapperPath := filepath.Join(home, ".local", "bin", "widget")
	if err := os.Remove(realPath); err != nil {
		t.Fatalf("remove real binary: %v", err)
	}
	latestTag.Store("v2.0.0")

	var output bytes.Buffer
	if err := runEnsureWithClient(context.Background(), client, []string{"acme/widget"}, &output); err != nil {
		t.Fatalf("ensure: %v\noutput: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "RESTORED acme/widget (v1.0.0)") || !strings.Contains(output.String(), "1 restored") {
		t.Fatalf("ensure output = %q", output.String())
	}
	if got := exactRequests.Load(); got != 1 {
		t.Fatalf("exact release requests = %d, want 1", got)
	}
	if got := assetRequests.Load(); got != 1 {
		t.Fatalf("asset requests = %d, want 1 because restore uses cache", got)
	}
	if got, err := os.ReadFile(realPath); err != nil || string(got) != "version one" {
		t.Fatalf("restored real binary = %q, err = %v", got, err)
	}
	wrapper, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("read restored wrapper: %v", err)
	}
	if !bytes.Contains(wrapper, []byte("zenget-managed wrapper")) {
		t.Fatalf("restored wrapper has no marker: %q", wrapper)
	}
	st, err = state.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if app, ok := st.Get("acme/widget"); !ok || app.Version != "v1.0.0" || app.XXH3_64 != hashx.Bytes([]byte("version one")) {
		t.Fatalf("state after ensure = %#v, present = %v", app, ok)
	}
}

func TestEnsureRestoresModifiedBinaryAndMissingWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	server := newEnsureTestServer(t, func(r *http.Request) (string, int) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest", "/repos/acme/widget/releases/tags/v1.0.0":
			return "v1.0.0", http.StatusOK
		case "/assets/widget":
			return "version one", http.StatusOK
		default:
			return "", http.StatusNotFound
		}
	})
	client := newEnsureTestClient(server)
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state after install: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state does not contain acme/widget")
	}
	realPath := app.RealPath
	wrapperPath := filepath.Join(home, ".local", "bin", "widget")
	if err := os.WriteFile(realPath, []byte("modified"), 0755); err != nil {
		t.Fatalf("modify real binary: %v", err)
	}
	if err := os.Remove(wrapperPath); err != nil {
		t.Fatalf("remove wrapper: %v", err)
	}

	var output bytes.Buffer
	if err := runEnsureWithClient(context.Background(), client, nil, &output); err != nil {
		t.Fatalf("ensure: %v\noutput: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "RESTORED acme/widget") {
		t.Fatalf("ensure output = %q", output.String())
	}
	if got, err := os.ReadFile(realPath); err != nil || string(got) != "version one" {
		t.Fatalf("restored real binary = %q, err = %v", got, err)
	}
	if _, err := os.Stat(wrapperPath); err != nil {
		t.Fatalf("restored wrapper: %v", err)
	}
}

func TestEnsureLeavesHealthyApplicationUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var exactRequests atomic.Int32
	server := newEnsureTestServer(t, func(r *http.Request) (string, int) {
		switch r.URL.Path {
		case "/repos/acme/widget/releases/latest":
			return "v1.0.0", http.StatusOK
		case "/repos/acme/widget/releases/tags/v1.0.0":
			exactRequests.Add(1)
			return "v1.0.0", http.StatusOK
		case "/assets/widget":
			return "version one", http.StatusOK
		default:
			return "", http.StatusNotFound
		}
	})
	client := newEnsureTestClient(server)
	if err := runInstallWithClient(context.Background(), client, "acme/widget", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("initial install: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state after install: %v", err)
	}
	app, ok := st.Get("acme/widget")
	if !ok {
		t.Fatal("state does not contain acme/widget")
	}
	realPath := app.RealPath
	before, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("read binary before ensure: %v", err)
	}

	var output bytes.Buffer
	if err := runEnsureWithClient(context.Background(), client, nil, &output); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !strings.Contains(output.String(), "HEALTHY acme/widget (v1.0.0)") || !strings.Contains(output.String(), "1 healthy") {
		t.Fatalf("ensure output = %q", output.String())
	}
	if got := exactRequests.Load(); got != 0 {
		t.Fatalf("exact release requests for healthy app = %d, want 0", got)
	}
	after, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("read binary after ensure: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("healthy binary changed from %q to %q", before, after)
	}
}

func TestEnsureContinuesAfterOneApplicationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	server := newEnsureTestServer(t, func(r *http.Request) (string, int) {
		switch r.URL.Path {
		case "/repos/acme/good/releases/latest", "/repos/acme/good/releases/tags/v1.0.0":
			return "v1.0.0", http.StatusOK
		case "/repos/acme/good/assets/good":
			return "good binary", http.StatusOK
		case "/repos/acme/bad/releases/latest":
			return "v1.0.0", http.StatusOK
		case "/repos/acme/bad/assets/bad":
			return "bad binary", http.StatusOK
		case "/repos/acme/bad/releases/tags/v1.0.0":
			return "release missing", http.StatusNotFound
		default:
			return "", http.StatusNotFound
		}
	})
	client := newEnsureTestClient(server)
	if err := runInstallWithClient(context.Background(), client, "acme/good", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("install good: %v", err)
	}
	if err := runInstallWithClient(context.Background(), client, "acme/bad", "", "", "", "", io.Discard); err != nil {
		t.Fatalf("install bad: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatalf("load state after install: %v", err)
	}
	goodApp, ok := st.Get("acme/good")
	if !ok {
		t.Fatal("state does not contain acme/good")
	}
	badApp, ok := st.Get("acme/bad")
	if !ok {
		t.Fatal("state does not contain acme/bad")
	}
	goodPath := goodApp.RealPath
	badPath := badApp.RealPath
	if err := os.WriteFile(goodPath, []byte("damaged"), 0755); err != nil {
		t.Fatalf("damage good binary: %v", err)
	}
	if err := os.Remove(badPath); err != nil {
		t.Fatalf("remove bad binary: %v", err)
	}

	var output bytes.Buffer
	err = runEnsureWithClient(context.Background(), client, nil, &output)
	if err == nil || !strings.Contains(err.Error(), "ensure failed for 1 application") {
		t.Fatalf("ensure error = %v\noutput: %s", err, output.String())
	}
	text := output.String()
	if !strings.Contains(text, "RESTORED acme/good") || !strings.Contains(text, "FAILED acme/bad") || !strings.Contains(text, "1 restored, 0 healthy, 1 failed") {
		t.Fatalf("ensure summary = %q", text)
	}
	if got, err := os.ReadFile(goodPath); err != nil || string(got) != "good binary" {
		t.Fatalf("good binary after ensure = %q, err = %v", got, err)
	}
}

func TestEnsureUnknownRepositoryReturnsFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var output bytes.Buffer
	err := runEnsureWithClient(context.Background(), nil, []string{"acme/missing"}, &output)
	if err == nil || !strings.Contains(err.Error(), "ensure failed") {
		t.Fatalf("ensure error = %v", err)
	}
	if !strings.Contains(output.String(), "acme/missing is not registered") || !strings.Contains(output.String(), "1 failed") {
		t.Fatalf("unknown repository output = %q", output.String())
	}
}

func newEnsureTestServer(t *testing.T, handler func(*http.Request) (string, int)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, status := handler(r)
		w.WriteHeader(status)
		if status == http.StatusOK && strings.Contains(r.URL.Path, "/releases/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tagFromReleaseBody(body),
				"assets": []map[string]string{{
					"name":                 assetNameForRequest(r.URL.Path),
					"browser_download_url": "http://" + r.Host + assetPathForRequest(r.URL.Path),
				}},
			})
			return
		}
		if status == http.StatusOK {
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newEnsureTestClient(server *httptest.Server) *github.Client {
	client := github.New()
	client.BaseURL = server.URL
	return client
}

func tagFromReleaseBody(body string) string {
	return body
}

func assetPathForRequest(path string) string {
	switch {
	case strings.Contains(path, "/repos/acme/good/"):
		return "/repos/acme/good/assets/good"
	case strings.Contains(path, "/repos/acme/bad/"):
		return "/repos/acme/bad/assets/bad"
	default:
		return "/assets/widget"
	}
}

func assetNameForRequest(path string) string {
	switch {
	case strings.Contains(path, "/repos/acme/good/"):
		return "good"
	case strings.Contains(path, "/repos/acme/bad/"):
		return "bad"
	default:
		return "widget"
	}
}

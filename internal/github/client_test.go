package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zenget/internal/config"
	"zenget/internal/limits"
)

const contentsTestCommit = "0123456789abcdef0123456789abcdef01234567"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func disableDownloadRetryWait(t *testing.T) {
	t.Helper()
	original := downloadRetryWait
	downloadRetryWait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { downloadRetryWait = original })
}

func TestLatestReleaseParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("request method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/acme/widget/releases/latest" {
			t.Errorf("request path = %s, want /repos/acme/widget/releases/latest", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q, want application/vnd.github+json", got)
		}
		if got := r.Header.Get("User-Agent"); got != "zenget" {
			t.Errorf("User-Agent header = %q, want zenget", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name":"v1.2.3","published_at":"2026-08-25T10:00:00Z","assets":[{"name":"widget-linux-amd64","url":"https://api.github.com/repos/acme/widget/releases/assets/1","browser_download_url":"https://example.com/widget","digest":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},{"name":"widget-darwin-arm64","browser_download_url":"https://example.com/widget-darwin"}]}`)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	release, err := client.LatestRelease(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("LatestRelease() error = %v", err)
	}
	if release.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want v1.2.3", release.TagName)
	}
	if len(release.Assets) != 2 {
		t.Fatalf("len(Assets) = %d, want 2", len(release.Assets))
	}
	if release.Assets[0].Name != "widget-linux-amd64" {
		t.Errorf("first asset name = %q, want widget-linux-amd64", release.Assets[0].Name)
	}
	if release.Assets[0].URL != "https://api.github.com/repos/acme/widget/releases/assets/1" {
		t.Errorf("first asset API URL = %q, want https://api.github.com/repos/acme/widget/releases/assets/1", release.Assets[0].URL)
	}
	if release.Assets[0].Digest != "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("first asset digest = %q, want GitHub SHA-256 digest", release.Assets[0].Digest)
	}
	if release.Assets[1].BrowserDownloadURL != "https://example.com/widget-darwin" {
		t.Errorf("second asset URL = %q, want https://example.com/widget-darwin", release.Assets[1].BrowserDownloadURL)
	}
	wantPublished := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	if !release.PublishedAt.Equal(wantPublished) {
		t.Errorf("PublishedAt = %v, want %v", release.PublishedAt, wantPublished)
	}
}

func TestContentsAtCommitUsesPinnedAPIPathAndIgnoresDownloadURL(t *testing.T) {
	t.Setenv("ZENGET_GITHUB_TOKEN", "contents-token")
	body := []byte(`{"schema_version":1,"entries":[]}`)
	encoded := base64.StdEncoding.EncodeToString(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/registry/contents/catalog/registry.json" {
			t.Errorf("contents path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("ref"); got != contentsTestCommit {
			t.Errorf("contents ref = %q, want %q", got, contentsTestCommit)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer contents-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != githubAccept {
			t.Errorf("Accept = %q, want %q", got, githubAccept)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "encoding": "base64", "path": "catalog/registry.json", "size": len(body),
			"content": encoded, "download_url": "https://evil.example.test/recipe.json",
		})
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	got, err := client.ContentsAtCommit(context.Background(), "acme", "registry", contentsTestCommit, "catalog/registry.json")
	if err != nil {
		t.Fatalf("ContentsAtCommit() error = %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("contents = %q, want %q", got, body)
	}
	if got, err := client.ContentsAt(context.Background(), "acme", "registry", contentsTestCommit, "catalog/registry.json"); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("ContentsAt() = %q, error %v", got, err)
	}
}

func TestContentsAtCommitRejectsMovingRefsUnsafeResponsesAndExternalRedirects(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		path string
		body string
		want string
	}{
		{name: "branch", ref: "main", path: "registry.json", want: "exactly 40"},
		{name: "short sha", ref: contentsTestCommit[:12], path: "registry.json", want: "exactly 40"},
		{name: "traversal", ref: contentsTestCommit, path: "../registry.json", want: "dot-segments"},
		{name: "directory", ref: contentsTestCommit, path: "registry.json", body: `[{"type":"file"}]`, want: "decode GitHub contents"},
		{name: "wrong encoding", ref: contentsTestCommit, path: "registry.json", body: `{"type":"file","encoding":"none","content":"x"}`, want: "unsupported encoding"},
		{name: "bad base64", ref: contentsTestCommit, path: "registry.json", body: `{"type":"file","encoding":"base64","content":"%%%"}`, want: "base64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.body == "" {
					t.Fatal("test body not configured")
				}
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := New()
			client.BaseURL = server.URL
			_, err := client.ContentsAtCommit(context.Background(), "acme", "registry", test.ref, test.path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ContentsAtCommit() error = %v, want %q", err, test.want)
			}
		})
	}

	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"file","encoding":"base64","content":"eA=="}`)
	}))
	defer redirectTarget.Close()
	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/contents", http.StatusFound)
	}))
	defer redirectSource.Close()
	client := New()
	client.BaseURL = redirectSource.URL
	_, err := client.ContentsAtCommit(context.Background(), "acme", "registry", contentsTestCommit, "registry.json")
	if err == nil || !strings.Contains(err.Error(), "untrusted host") {
		t.Fatalf("external redirect error = %v, want untrusted host", err)
	}
}

func TestNewTokenEnvironmentPrecedence(t *testing.T) {
	t.Setenv("ZENGET_GITHUB_TOKEN", "zenget-token")
	t.Setenv("GITHUB_TOKEN", "github-token")

	client := New()
	if client.Token != "zenget-token" {
		t.Errorf("Token = %q, want zenget-token", client.Token)
	}
}

func TestNewConfiguredRejectsInvalidForgeAndBaseURLCombinations(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		forge   string
		want    string
	}{
		{name: "unknown forge", baseURL: "https://example.com", forge: "bitbucket", want: "unsupported forge"},
		{name: "relative base", baseURL: "example.com/api/v1", want: "absolute HTTP(S) URL"},
		{name: "query base", baseURL: "https://example.com/api/v1?token=secret", want: "query"},
		{name: "gitlab wrong api path", baseURL: "https://example.com/api/v3", forge: "gitlab", want: "path must end with /api/v4"},
		{name: "forgejo missing base", forge: "forgejo", want: "required for forgejo/gitea"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewConfigured(test.baseURL, test.forge)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewConfigured() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNewConfiguredGitLabMapsReleasesAndUsesJobToken(t *testing.T) {
	t.Setenv("CI_JOB_TOKEN", "ci-job-token")
	t.Setenv("GITLAB_TOKEN", "gitlab-token")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/acme/widget/releases":
			if got := r.URL.EscapedPath(); got != "/api/v4/projects/acme%2Fwidget/releases" {
				t.Errorf("escaped GitLab path = %q, want encoded project path", got)
			}
			if got := r.Header.Get("Accept"); got != "application/json" {
				t.Errorf("GitLab Accept = %q, want application/json", got)
			}
			if got := r.Header.Get("JOB-TOKEN"); got != "ci-job-token" {
				t.Errorf("JOB-TOKEN = %q, want CI job token", got)
			}
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q, want empty for GitLab", got)
			}
			_, _ = io.WriteString(w, `[{"tag_name":"v3.0.0","upcoming_release":true,"released_at":"2026-08-30T10:00:00Z","assets":{"links":[]}},`+
				`{"tag_name":"v2.0.0","released_at":"2026-08-25T10:00:00Z","assets":{"links":[{"name":"widget-linux-amd64","url":"`+server.URL+`/downloads/widget","direct_asset_url":"`+server.URL+`/downloads/widget-direct"}]}}]`)
		case "/downloads/widget-direct":
			if got := r.Header.Get("JOB-TOKEN"); got != "ci-job-token" {
				t.Errorf("asset JOB-TOKEN = %q, want CI job token", got)
			}
			_, _ = io.WriteString(w, "gitlab binary")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewConfigured(server.URL, "gitlab")
	if err != nil {
		t.Fatalf("NewConfigured() error = %v", err)
	}
	if client.BaseURL != server.URL+"/api/v4" {
		t.Fatalf("BaseURL = %q, want %q", client.BaseURL, server.URL+"/api/v4")
	}
	release, err := client.LatestRelease(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("LatestRelease() error = %v", err)
	}
	if release.TagName != "v2.0.0" {
		t.Fatalf("TagName = %q, want v2.0.0 after skipping upcoming release", release.TagName)
	}
	if !release.PublishedAt.Equal(time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("PublishedAt = %v, want GitLab released_at", release.PublishedAt)
	}
	if len(release.Assets) != 1 || release.Assets[0].URL != server.URL+"/downloads/widget-direct" || release.Assets[0].BrowserDownloadURL != server.URL+"/downloads/widget" {
		t.Fatalf("mapped assets = %#v, want direct and browser URLs", release.Assets)
	}

	destPath := filepath.Join(t.TempDir(), "widget")
	if err := client.DownloadAsset(context.Background(), release.Assets[0], destPath); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}
	if got, err := os.ReadFile(destPath); err != nil || string(got) != "gitlab binary" {
		t.Fatalf("downloaded GitLab asset = %q, error = %v", got, err)
	}
}

func TestNewConfiguredGitLabReleaseByTagUsesProjectPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/acme/widget/releases/v1.2.3" {
			t.Errorf("GitLab tag request path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.3","released_at":"2026-08-25T10:00:00Z","assets":{"links":[]}}`)
	}))
	defer server.Close()

	client, err := NewConfigured(server.URL, "gitlab")
	if err != nil {
		t.Fatalf("NewConfigured() error = %v", err)
	}
	release, err := client.ReleaseByTag(context.Background(), "acme", "widget", "v1.2.3")
	if err != nil {
		t.Fatalf("ReleaseByTag() error = %v", err)
	}
	if release.TagName != "v1.2.3" {
		t.Fatalf("tag release = %q, want v1.2.3", release.TagName)
	}
}

func TestNewConfiguredUsesForgeTokenFallbacks(t *testing.T) {
	t.Setenv("CI_JOB_TOKEN", "")
	t.Setenv("GITLAB_TOKEN", "gitlab-fallback")
	gitlab, err := NewConfigured("https://gitlab.example.test/api/v4", "gitlab")
	if err != nil {
		t.Fatalf("GitLab NewConfigured() error = %v", err)
	}
	if gitlab.Token != "gitlab-fallback" || gitlab.tokenHeader != "PRIVATE-TOKEN" {
		t.Fatalf("GitLab fallback = token %q, header %q", gitlab.Token, gitlab.tokenHeader)
	}

	t.Setenv("FORGEJO_TOKEN", "")
	t.Setenv("CODEBERG_TOKEN", "codeberg-fallback")
	forgejo, err := NewConfigured("https://forgejo.example.test/api/v1", "forgejo")
	if err != nil {
		t.Fatalf("Forgejo NewConfigured() error = %v", err)
	}
	if forgejo.Token != "codeberg-fallback" || forgejo.tokenPrefix != "token " {
		t.Fatalf("Forgejo fallback = token %q, prefix %q", forgejo.Token, forgejo.tokenPrefix)
	}
}

func TestNewConfiguredAutoDetectsGitLabAndForgejo(t *testing.T) {
	gitlab, err := NewConfigured("https://gitlab.com", "")
	if err != nil {
		t.Fatalf("GitLab auto-detection error = %v", err)
	}
	if gitlab.Forge != ForgeGitLab || gitlab.BaseURL != "https://gitlab.com/api/v4" {
		t.Fatalf("GitLab auto-detection = forge %q, base %q", gitlab.Forge, gitlab.BaseURL)
	}

	forgejo, err := NewConfigured("https://codeberg.org", "")
	if err != nil {
		t.Fatalf("Forgejo auto-detection error = %v", err)
	}
	if forgejo.Forge != ForgeForgejo || forgejo.BaseURL != "https://codeberg.org/api/v1" {
		t.Fatalf("Forgejo auto-detection = forge %q, base %q", forgejo.Forge, forgejo.BaseURL)
	}
}

func TestNewConfiguredForgejoUsesCompatibleAPIAndToken(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "forgejo-token")
	t.Setenv("CODEBERG_TOKEN", "codeberg-token")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/widget/releases/latest" {
			t.Errorf("Forgejo request path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != githubAccept {
			t.Errorf("Forgejo Accept = %q, want GitHub-compatible Accept", got)
		}
		if got := r.Header.Get("Authorization"); got != "token forgejo-token" {
			t.Errorf("Forgejo Authorization = %q, want token auth", got)
		}
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.3","assets":[]}`)
	}))
	defer server.Close()

	client, err := NewConfigured(server.URL+"/api/v1", "")
	if err != nil {
		t.Fatalf("NewConfigured() error = %v", err)
	}
	release, err := client.LatestRelease(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("LatestRelease() error = %v", err)
	}
	if release.TagName != "v1.2.3" || client.Forge != ForgeForgejo {
		t.Fatalf("Forgejo release = %#v, forge = %q", release, client.Forge)
	}
}

func TestNewConfiguredGitHubEnterprisePreservesAPIPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/acme/widget/releases/latest" {
			t.Errorf("enterprise request path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != githubAccept {
			t.Errorf("enterprise Accept = %q, want GitHub Accept", got)
		}
		_, _ = io.WriteString(w, `{"tag_name":"v9.0.0","assets":[]}`)
	}))
	defer server.Close()

	client, err := NewConfigured(server.URL+"/api/v3", "github")
	if err != nil {
		t.Fatalf("NewConfigured() error = %v", err)
	}
	release, err := client.LatestRelease(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("LatestRelease() error = %v", err)
	}
	if release.TagName != "v9.0.0" {
		t.Fatalf("enterprise TagName = %q, want v9.0.0", release.TagName)
	}
}

func TestLatestReleaseSendsOptionalAuthorization(t *testing.T) {
	tests := []struct {
		name        string
		zengetToken string
		githubToken string
		wantAuth    string
	}{
		{name: "zenget token", zengetToken: "zenget-token", githubToken: "github-token", wantAuth: "Bearer zenget-token"},
		{name: "github token fallback", githubToken: "github-token", wantAuth: "Bearer github-token"},
		{name: "anonymous", wantAuth: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.zengetToken == "" {
				t.Setenv("ZENGET_GITHUB_TOKEN", "")
			} else {
				t.Setenv("ZENGET_GITHUB_TOKEN", test.zengetToken)
			}
			if test.githubToken == "" {
				t.Setenv("GITHUB_TOKEN", "")
			} else {
				t.Setenv("GITHUB_TOKEN", test.githubToken)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != test.wantAuth {
					t.Errorf("Authorization header = %q, want %q", got, test.wantAuth)
				}
				fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[]}`)
			}))
			defer server.Close()

			client := New()
			client.BaseURL = server.URL
			if _, err := client.LatestRelease(context.Background(), "org", "repo"); err != nil {
				t.Fatalf("LatestRelease() error = %v", err)
			}
		})
	}
}

func TestLatestReleaseNonOKIncludesStatusAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "release not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	_, err := client.LatestRelease(context.Background(), "org", "missing")
	if err == nil {
		t.Fatal("LatestRelease() error = nil, want 404 error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("LatestRelease() error = %q, want status 404", err)
	}
	if !strings.Contains(err.Error(), "release not found") {
		t.Errorf("LatestRelease() error = %q, want response body", err)
	}
}

func TestDownloadAssetWritesBytes(t *testing.T) {
	const want = "zenget asset contents\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, want)
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	if err := client.DownloadAsset(context.Background(), Asset{BrowserDownloadURL: server.URL + "/widget"}, destPath); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", destPath, err)
	}
	if string(got) != want {
		t.Errorf("downloaded contents = %q, want %q", got, want)
	}
	info, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", destPath, err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Errorf("downloaded file mode = %04o, want 0644", got)
	}
}

func TestDownloadAssetRejectsKnownOversizeBeforeWriting(t *testing.T) {
	const limit = int64(4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "12345")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	if err := os.WriteFile(destPath, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, destPath, limit)
	if err == nil || !errors.Is(err, limits.ErrLimitExceeded) {
		t.Fatalf("DownloadAsset() error = %v, want size limit error", err)
	}
	if !strings.Contains(err.Error(), "release download limit=4") || !strings.Contains(err.Error(), "widget") {
		t.Errorf("DownloadAsset() error = %q, want limit and asset name", err)
	}
	if strings.Contains(err.Error(), server.URL) {
		t.Errorf("DownloadAsset() error leaked URL: %q", err)
	}
	if got, err := os.ReadFile(destPath); err != nil || string(got) != "existing" {
		t.Fatalf("destination after preflight rejection = %q, err = %v", got, err)
	}
}

func TestDownloadAssetRejectsUnknownLengthWhileStreaming(t *testing.T) {
	const limit = int64(4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer is not flushable")
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = io.WriteString(w, "12345")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL + "?token=secret"}, destPath, limit)
	if err == nil || !errors.Is(err, limits.ErrLimitExceeded) {
		t.Fatalf("DownloadAsset() error = %v, want streaming size limit error", err)
	}
	if !strings.Contains(err.Error(), "release download limit=4") || !strings.Contains(err.Error(), "widget") {
		t.Errorf("DownloadAsset() error = %q, want limit and asset name", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("DownloadAsset() error leaked URL query: %q", err)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("destination after streaming rejection exists or could not be checked: %v", err)
	}
}

func TestDownloadAssetWithLimitUsesSpecificDiagnosticKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "12345")
	}))
	defer server.Close()

	client := New()
	err := client.DownloadAssetWithLimit(
		context.Background(),
		Asset{Name: "checksums.txt", BrowserDownloadURL: server.URL},
		filepath.Join(t.TempDir(), "checksums.txt"),
		4,
		"checksum asset",
	)
	if err == nil || !errors.Is(err, limits.ErrLimitExceeded) {
		t.Fatalf("DownloadAssetWithLimit() error = %v, want size limit error", err)
	}
	if !strings.Contains(err.Error(), "checksum asset limit=4") {
		t.Fatalf("DownloadAssetWithLimit() error = %v, want checksum diagnostic", err)
	}

	if err := client.DownloadAssetWithLimit(context.Background(), Asset{Name: "checksums.txt"}, "", 0, "checksum asset"); err == nil {
		t.Fatal("DownloadAssetWithLimit() error = nil for zero limit")
	}
}

func TestDownloadAssetHonorsConfiguredDefaultLimit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := (&config.Config{MaxDownloadBytes: 4}).Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "12345")
	}))
	defer server.Close()

	client := New()
	destPath := filepath.Join(t.TempDir(), "widget")
	err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, destPath)
	if err == nil || !errors.Is(err, limits.ErrLimitExceeded) {
		t.Fatalf("DownloadAsset() error = %v, want configured-size-path-compatible limit error", err)
	}
}

func TestLatestReleaseRejectsOversizeStructuredJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.0.0","padding":"`+strings.Repeat("x", int(limits.DefaultStructuredBytes))+`"}`)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	_, err := client.LatestRelease(context.Background(), "acme", "widget")
	if err == nil || !errors.Is(err, limits.ErrLimitExceeded) {
		t.Fatalf("LatestRelease() error = %v, want structured-input limit error", err)
	}
	if !strings.Contains(err.Error(), "structured JSON") || !strings.Contains(err.Error(), "latest release acme/widget") {
		t.Errorf("LatestRelease() error = %q, want bounded-input diagnostics", err)
	}
}

func TestLatestReleaseRejectsDuplicateJSONKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.0.0","tag_name":"v2.0.0"}`)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	_, err := client.LatestRelease(context.Background(), "acme", "widget")
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
		t.Fatalf("LatestRelease() error = %v, want duplicate-key error", err)
	}
}

func TestDownloadAssetUsesAPIURLAndAuthenticatedHeaders(t *testing.T) {
	const token = "private-test-token"
	const want = "private asset contents\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/assets/1" {
			t.Errorf("request path = %s, want /api/assets/1", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/octet-stream" {
			t.Errorf("Accept header = %q, want application/octet-stream", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q, want Bearer token", got)
		}
		if got := r.Header.Get("User-Agent"); got != "zenget" {
			t.Errorf("User-Agent = %q, want zenget", got)
		}
		_, _ = io.WriteString(w, want)
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	client.BaseURL = server.URL
	client.Token = token
	asset := Asset{
		Name:               "widget",
		URL:                server.URL + "/api/assets/1",
		BrowserDownloadURL: server.URL + "/browser-download",
	}
	if err := client.DownloadAsset(context.Background(), asset, destPath); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", destPath, err)
	}
	if string(got) != want {
		t.Errorf("downloaded contents = %q, want %q", got, want)
	}
}

func TestDownloadAssetFallsBackToBrowserURLWithoutCrossOriginAuthorization(t *testing.T) {
	const token = "private-test-token"
	const want = "browser fallback contents\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/browser-download" {
			t.Errorf("request path = %s, want /browser-download", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/octet-stream" {
			t.Errorf("Accept header = %q, want application/octet-stream", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header = %q, want no cross-origin token", got)
		}
		_, _ = io.WriteString(w, want)
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	client.BaseURL = "https://api.github.com"
	client.Token = token
	asset := Asset{Name: "widget", BrowserDownloadURL: server.URL + "/browser-download"}
	if err := client.DownloadAsset(context.Background(), asset, destPath); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", destPath, err)
	}
	if string(got) != want {
		t.Errorf("downloaded contents = %q, want %q", got, want)
	}
}

func TestDownloadAssetRedirectDoesNotForwardAuthorizationAcrossOrigins(t *testing.T) {
	const token = "private-test-token"
	const want = "redirected asset contents\n"
	downloadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			http.Error(w, "authorization leaked", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Accept"); got != "application/octet-stream" {
			t.Errorf("redirect Accept header = %q, want application/octet-stream", got)
		}
		_, _ = io.WriteString(w, want)
	}))
	defer downloadServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/assets/1" {
			t.Errorf("API request path = %s, want /api/assets/1", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("API Authorization header = %q, want Bearer token", got)
		}
		if got := r.Header.Get("Accept"); got != "application/octet-stream" {
			t.Errorf("API Accept header = %q, want application/octet-stream", got)
		}
		http.Redirect(w, r, downloadServer.URL+"/payload", http.StatusFound)
	}))
	defer apiServer.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	client.BaseURL = apiServer.URL
	client.Token = token
	asset := Asset{Name: "widget", URL: apiServer.URL + "/api/assets/1"}
	if err := client.DownloadAsset(context.Background(), asset, destPath); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", destPath, err)
	}
	if string(got) != want {
		t.Errorf("downloaded contents = %q, want %q", got, want)
	}
}

func TestDownloadAssetRequiresDownloadURL(t *testing.T) {
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{Name: "widget"}, filepath.Join(t.TempDir(), "widget"))
	if err == nil {
		t.Fatal("DownloadAsset() error = nil, want missing URL error")
	}
	if !strings.Contains(err.Error(), "has no download URL") {
		t.Errorf("DownloadAsset() error = %q, want missing URL message", err)
	}
}

func TestLatestReleaseMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name":`)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	_, err := client.LatestRelease(context.Background(), "org", "repo")
	if err == nil {
		t.Fatal("LatestRelease() error = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode latest release") {
		t.Errorf("LatestRelease() error = %q, want decode error", err)
	}
}

func TestLatestReleaseRequestError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	client := New()
	client.BaseURL = serverURL
	_, err := client.LatestRelease(context.Background(), "org", "repo")
	if err == nil {
		t.Fatal("LatestRelease() error = nil, want request error")
	}
	if !strings.Contains(err.Error(), "send GitHub request") {
		t.Errorf("LatestRelease() error = %q, want send request error", err)
	}
}

func TestLatestReleaseEmptyErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	_, err := client.LatestRelease(context.Background(), "org", "repo")
	if err == nil {
		t.Fatal("LatestRelease() error = nil, want 403 error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("LatestRelease() error = %q, want status 403", err)
	}
}

func TestLatestReleaseNilHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v2.0.0","assets":[]}`)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	release, err := client.LatestRelease(context.Background(), "org", "repo")
	if err != nil {
		t.Fatalf("LatestRelease() error = %v", err)
	}
	if release.TagName != "v2.0.0" {
		t.Errorf("TagName = %q, want v2.0.0", release.TagName)
	}
}

func TestGetInvalidEndpoint(t *testing.T) {
	client := New()
	_, err := client.get(context.Background(), "://invalid-url")
	if err == nil {
		t.Fatal("get() error = nil, want request creation error")
	}
	if !strings.Contains(err.Error(), "create GitHub request") {
		t.Errorf("get() error = %q, want create request error", err)
	}
}

func TestDownloadAssetInvalidURL(t *testing.T) {
	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{BrowserDownloadURL: "://invalid-url"}, destPath)
	if err == nil {
		t.Fatal("DownloadAsset() error = nil, want download error")
	}
	if !strings.Contains(err.Error(), "download asset") {
		t.Errorf("DownloadAsset() error = %q, want download asset error", err)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("destination file exists after failed download: stat error = %v", err)
	}
}

func TestDownloadAssetUncreatableDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "zenget asset contents\n")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "missing-dir", "widget")
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{BrowserDownloadURL: server.URL + "/widget"}, destPath)
	if err == nil {
		t.Fatal("DownloadAsset() error = nil, want create file error")
	}
	if !strings.Contains(err.Error(), "create asset file") {
		t.Errorf("DownloadAsset() error = %q, want create asset file error", err)
	}
}

func TestDownloadAssetTruncatedBodyRemovesPartialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "short")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{BrowserDownloadURL: server.URL + "/widget"}, destPath)
	if err == nil {
		t.Fatal("DownloadAsset() error = nil, want write error")
	}
	if !strings.Contains(err.Error(), "write asset file") {
		t.Errorf("DownloadAsset() error = %q, want write asset file error", err)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("destination file exists after failed download: stat error = %v", err)
	}
}

func TestDownloadAssetFailureRemovesPartialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "server failure")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{BrowserDownloadURL: server.URL + "/widget"}, destPath)
	if err == nil {
		t.Fatal("DownloadAsset() error = nil, want 500 error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("DownloadAsset() error = %q, want status 500", err)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("destination file exists after failed download: stat error = %v", err)
	}
}

func TestDownloadAssetRetriesTransientResponses(t *testing.T) {
	disableDownloadRetryWait(t)
	statuses := []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable}
	for _, status := range statuses {
		t.Run(fmt.Sprintf("status-%d", status), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					if status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "0")
					}
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "temporary failure")
					return
				}
				_, _ = io.WriteString(w, "recovered asset")
			}))
			defer server.Close()

			destPath := filepath.Join(t.TempDir(), "widget")
			client := New()
			if err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, destPath, 64); err != nil {
				t.Fatalf("DownloadAsset() error = %v", err)
			}
			if requests != 2 {
				t.Errorf("request count = %d, want 2", requests)
			}
			got, err := os.ReadFile(destPath)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(got) != "recovered asset" {
				t.Errorf("downloaded contents = %q, want recovered asset", got)
			}
		})
	}
}

func TestDownloadAssetDoesNotRetryOtherClientErrors(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not found")
	}))
	defer server.Close()

	client := New()
	err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, filepath.Join(t.TempDir(), "widget"), 64)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("DownloadAsset() error = %v, want 404 error", err)
	}
	if requests != 1 {
		t.Errorf("request count = %d, want 1", requests)
	}
}

func TestDownloadAssetRetriesTransientNetworkError(t *testing.T) {
	disableDownloadRetryWait(t)
	requests := 0
	client := New()
	client.HTTP = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return nil, errors.New("connection reset by peer")
		}
		body := "recovered after network error"
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       request,
		}, nil
	})}

	destPath := filepath.Join(t.TempDir(), "widget")
	if err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: "https://example.test/widget"}, destPath, 128); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}
	if requests != 2 {
		t.Errorf("request count = %d, want 2", requests)
	}
}

func TestDownloadAssetResumesWithContentRange(t *testing.T) {
	disableDownloadRetryWait(t)
	full := []byte("0123456789")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			if got := r.Header.Get("Range"); got != "" {
				t.Errorf("initial Range header = %q, want empty", got)
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(full)))
			_, _ = w.Write(full[:4])
		case 2:
			if got := r.Header.Get("Range"); got != "bytes=4-" {
				t.Errorf("resume Range header = %q, want bytes=4-", got)
			}
			w.Header().Set("Content-Range", "bytes 4-9/10")
			w.Header().Set("Content-Length", "6")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(full[4:])
		default:
			t.Errorf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	if err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, destPath, 64); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(full) {
		t.Errorf("downloaded contents = %q, want %q", got, full)
	}
	if requests != 2 {
		t.Errorf("request count = %d, want 2", requests)
	}
}

func TestDownloadAssetRestartsWhenRangeIsIgnored(t *testing.T) {
	disableDownloadRetryWait(t)
	full := []byte("abcdefghij")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(full)))
			_, _ = w.Write(full[:3])
			return
		}
		if got := r.Header.Get("Range"); got != "bytes=3-" {
			t.Errorf("restart Range header = %q, want bytes=3-", got)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(full)))
		_, _ = w.Write(full)
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	if err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, destPath, 64); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(full) {
		t.Errorf("downloaded contents = %q, want %q", got, full)
	}
}

func TestDownloadAssetRejectsInvalidContentRange(t *testing.T) {
	disableDownloadRetryWait(t)
	full := []byte("012345")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(full)))
			_, _ = w.Write(full[:3])
			return
		}
		w.Header().Set("Content-Range", "bytes 2-5/6")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(full[2:])
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, destPath, 64)
	if err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("DownloadAsset() error = %v, want Content-Range error", err)
	}
	if requests != 2 {
		t.Errorf("request count = %d, want 2", requests)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("destination after invalid Content-Range exists or could not be checked: %v", err)
	}
}

func TestDownloadAssetRejectsRangeNotSatisfiable(t *testing.T) {
	disableDownloadRetryWait(t)
	full := []byte("012345")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(full)))
			_, _ = w.Write(full[:3])
			return
		}
		w.Header().Set("Content-Range", "bytes */6")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		_, _ = io.WriteString(w, "range not satisfiable")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, destPath, 64)
	if err == nil || !strings.Contains(err.Error(), "416") {
		t.Fatalf("DownloadAsset() error = %v, want 416 error", err)
	}
	if requests != 2 {
		t.Errorf("request count = %d, want 2", requests)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("destination after 416 exists or could not be checked: %v", err)
	}
}

func TestDownloadAssetExhaustsRetriesForRepeatedTruncation(t *testing.T) {
	disableDownloadRetryWait(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	client := New()
	err := client.DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, destPath, 128)
	if err == nil || !strings.Contains(err.Error(), "write asset file") {
		t.Fatalf("DownloadAsset() error = %v, want exhausted body error", err)
	}
	if requests != downloadMaxAttempts {
		t.Errorf("request count = %d, want %d", requests, downloadMaxAttempts)
	}
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("destination after exhausted retries exists or could not be checked: %v", err)
	}
}

func TestDownloadAssetCancellationStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	waits := 0
	originalWait := downloadRetryWait
	downloadRetryWait = func(context.Context, time.Duration) error {
		waits++
		cancel()
		return context.Canceled
	}
	t.Cleanup(func() { downloadRetryWait = originalWait })

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	defer server.Close()

	err := New().DownloadAsset(ctx, Asset{Name: "widget", BrowserDownloadURL: server.URL}, filepath.Join(t.TempDir(), "widget"), 128)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("DownloadAsset() error = %v, want context.Canceled", err)
	}
	if waits != 1 {
		t.Errorf("retry waits = %d, want 1", waits)
	}
	if requests != 1 {
		t.Errorf("request count = %d, want 1 after cancellation", requests)
	}
}

func TestDownloadAssetRetryAfterControlsWait(t *testing.T) {
	delays := make([]time.Duration, 0, 1)
	originalWait := downloadRetryWait
	downloadRetryWait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	t.Cleanup(func() { downloadRetryWait = originalWait })

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "recovered")
	}))
	defer server.Close()

	if err := New().DownloadAsset(context.Background(), Asset{Name: "widget", BrowserDownloadURL: server.URL}, filepath.Join(t.TempDir(), "widget"), 64); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}
	if len(delays) != 1 || delays[0] != 7*time.Second {
		t.Errorf("retry delays = %v, want [7s]", delays)
	}
}

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		value       string
		wantStart   int64
		wantLength  int64
		wantTotal   int64
		wantTotalOK bool
		wantErr     bool
	}{
		{value: "bytes 4-9/10", wantStart: 4, wantLength: 6, wantTotal: 10, wantTotalOK: true},
		{value: "bytes 0-2/*", wantLength: 3},
		{value: "", wantErr: true},
		{value: "items 0-1/2", wantErr: true},
		{value: "bytes 0/2", wantErr: true},
		{value: "bytes 2-1/3", wantErr: true},
		{value: "bytes x-1/3", wantErr: true},
		{value: "bytes 0-1/1", wantErr: true},
		{value: "bytes 0-9223372036854775807/*", wantErr: true},
	}
	for _, test := range tests {
		got, err := parseContentRange(test.value)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseContentRange(%q) error = nil, want error", test.value)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseContentRange(%q) error = %v", test.value, err)
			continue
		}
		if got.start != test.wantStart || got.length != test.wantLength || got.total != test.wantTotal || got.totalSet != test.wantTotalOK {
			t.Errorf("parseContentRange(%q) = %+v, want start=%d length=%d total=%d totalSet=%t", test.value, got, test.wantStart, test.wantLength, test.wantTotal, test.wantTotalOK)
		}
	}
}

func TestRetryDelayAndRetryAfterParsing(t *testing.T) {
	if got := retryDelay(nil, 0); got != downloadRetryBase {
		t.Errorf("retryDelay(nil, 0) = %v, want %v", got, downloadRetryBase)
	}
	if got := retryDelay(nil, 2); got != 4*downloadRetryBase {
		t.Errorf("retryDelay(nil, 2) = %v, want %v", got, 4*downloadRetryBase)
	}
	if got := retryDelay(nil, 100); got != downloadRetryMaximum {
		t.Errorf("retryDelay(nil, 100) = %v, want %v", got, downloadRetryMaximum)
	}
	response := &http.Response{Header: http.Header{"Retry-After": []string{"7"}}}
	if got := retryDelay(response, 0); got != 7*time.Second {
		t.Errorf("retryDelay() with Retry-After = %v, want 7s", got)
	}

	tests := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{value: "", ok: false},
		{value: "invalid", ok: false},
		{value: "-1", ok: false},
		{value: "0", want: 0, ok: true},
		{value: "7", want: 7 * time.Second, ok: true},
		{value: "999", want: downloadRetryMaximum, ok: true},
		{value: time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat), want: 0, ok: true},
	}
	for _, test := range tests {
		got, ok := parseRetryAfter(test.value)
		if ok != test.ok || (ok && got != test.want) {
			t.Errorf("parseRetryAfter(%q) = (%v, %t), want (%v, %t)", test.value, got, ok, test.want, test.ok)
		}
	}
}

func TestDownloadAssetURLDownloadsDirectEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != githubAccept {
			t.Errorf("Accept header = %q, want %q", got, githubAccept)
		}
		_, _ = io.WriteString(w, "direct asset")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "widget")
	if err := New().DownloadAssetURL(context.Background(), server.URL, destPath, 64); err != nil {
		t.Fatalf("DownloadAssetURL() error = %v", err)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "direct asset" {
		t.Errorf("downloaded contents = %q, want direct asset", got)
	}
}

func TestDownloadAssetAcceptsEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "empty")
	if err := New().DownloadAsset(context.Background(), Asset{Name: "empty", BrowserDownloadURL: server.URL}, destPath, 64); err != nil {
		t.Fatalf("DownloadAsset() error = %v", err)
	}
	info, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("empty response size = %d, want 0", info.Size())
	}
}

func TestContextErrorPrefersContextState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := contextError(ctx, errors.New("request failed")); !errors.Is(got, context.Canceled) {
		t.Errorf("contextError() = %v, want context.Canceled", got)
	}
	want := errors.New("request failed")
	if got := contextError(context.Background(), want); got != want {
		t.Errorf("contextError() = %v, want original error", got)
	}
}

func TestReleaseByTagParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("request method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/acme/widget/releases/tags/v1.2.3" {
			t.Errorf("request path = %s, want /repos/acme/widget/releases/tags/v1.2.3", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name":"v1.2.3","published_at":"2026-08-25T10:00:00Z","assets":[{"name":"widget","browser_download_url":"https://example.com/widget"}]}`)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	release, err := client.ReleaseByTag(context.Background(), "acme", "widget", "v1.2.3")
	if err != nil {
		t.Fatalf("ReleaseByTag() error = %v", err)
	}
	if release.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want v1.2.3", release.TagName)
	}
	if len(release.Assets) != 1 {
		t.Fatalf("len(Assets) = %d, want 1", len(release.Assets))
	}
}

func TestReleaseByTagNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	_, err := client.ReleaseByTag(context.Background(), "acme", "widget", "v1.2.3")
	if err == nil {
		t.Fatal("ReleaseByTag() error = nil, want 404 error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("ReleaseByTag() error = %q, want status 404", err)
	}
}

func TestListReleasesParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("request method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/acme/widget/releases" {
			t.Errorf("request path = %s, want /repos/acme/widget/releases", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("request query = %q, want empty for the default page", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"tag_name":"v1.2.3","published_at":"2026-08-25T10:00:00Z","assets":[]},{"tag_name":"v1.2.2","published_at":"2026-08-20T10:00:00Z","assets":[]}]`)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	releases, err := client.ListReleases(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("ListReleases() error = %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("len(releases) = %d, want 2", len(releases))
	}
	if releases[0].TagName != "v1.2.3" {
		t.Errorf("first release tag = %q, want v1.2.3", releases[0].TagName)
	}
	if releases[1].TagName != "v1.2.2" {
		t.Errorf("second release tag = %q, want v1.2.2", releases[1].TagName)
	}
}

func TestListReleasesAllFetchesAllPages(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/releases" {
			t.Errorf("request path = %s, want /repos/acme/widget/releases", r.URL.Path)
		}
		page := r.URL.Query().Get("page")
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
		}
		requests = append(requests, page)
		var releases []Release
		switch page {
		case "1":
			releases = make([]Release, releasePageSize)
			for index := range releases {
				releases[index].TagName = fmt.Sprintf("v1.0.%d", index)
			}
		case "2":
			releases = []Release{{TagName: "v2.0.0"}}
		default:
			t.Errorf("unexpected release page %q", page)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(releases); err != nil {
			t.Errorf("encode release page: %v", err)
		}
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	releases, err := client.ListReleasesAll(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("ListReleasesAll() error = %v", err)
	}
	if len(releases) != releasePageSize+1 {
		t.Fatalf("len(releases) = %d, want %d", len(releases), releasePageSize+1)
	}
	if releases[releasePageSize].TagName != "v2.0.0" {
		t.Fatalf("second-page release tag = %q, want v2.0.0", releases[releasePageSize].TagName)
	}
	if strings.Join(requests, ",") != "1,2" {
		t.Fatalf("requested pages = %v, want [1 2]", requests)
	}
}

func TestListReleasesEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	releases, err := client.ListReleases(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("ListReleases() error = %v", err)
	}
	if len(releases) != 0 {
		t.Fatalf("len(releases) = %d, want 0", len(releases))
	}
}

func TestListReleasesNonOKIncludesStatusAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	client := New()
	client.BaseURL = server.URL
	_, err := client.ListReleases(context.Background(), "acme", "widget")
	if err == nil {
		t.Fatal("ListReleases() error = nil, want 403 error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("ListReleases() error = %q, want status 403", err)
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("ListReleases() error = %q, want response body", err)
	}
}

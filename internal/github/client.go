// Package github provides the GitHub API client used by zenget.
package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"zenget/internal/config"
	"zenget/internal/limits"
	"zenget/internal/provider"
)

const (
	defaultBaseURL       = "https://api.github.com"
	defaultGitLabBaseURL = "https://gitlab.com/api/v4"
	githubAccept         = "application/vnd.github+json"
	jsonAccept           = "application/json"
	binaryAccept         = "application/octet-stream"
	userAgent            = "zenget"
	downloadMaxAttempts  = 3
	downloadRetryBase    = 100 * time.Millisecond
	downloadRetryMaximum = 30 * time.Second
)

var fullCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

var downloadRetryWait = waitForDownloadRetry

// Forge identifies the release API dialect used by a Client.
type Forge string

const (
	// ForgeGitHub selects the GitHub and GitHub-compatible releases API.
	ForgeGitHub Forge = "github"
	// ForgeGitLab selects the GitLab releases API.
	ForgeGitLab Forge = "gitlab"
	// ForgeForgejo selects the Forgejo/Gitea releases API.
	ForgeForgejo Forge = "forgejo"
)

// Asset is kept as a compatibility alias for the forge-neutral provider
// model.
type Asset = provider.Asset

// Release is kept as a compatibility alias for the forge-neutral provider
// model.
type Release = provider.Release

// Client is an HTTP client for the GitHub REST API.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
	Forge   Forge

	tokenHeader string
	tokenPrefix string
}

var _ provider.Provider = (*Client)(nil)

// New creates a GitHub API client using an optional environment-provided token.
func New() *Client {
	return &Client{
		HTTP:        &http.Client{Timeout: 60 * time.Second},
		BaseURL:     defaultBaseURL,
		Token:       firstNonEmptyEnv("ZENGET_GITHUB_TOKEN", "GITHUB_TOKEN"),
		Forge:       ForgeGitHub,
		tokenHeader: "Authorization",
		tokenPrefix: "Bearer ",
	}
}

// NewConfigured creates a release API client from a base URL and optional
// forge name. An empty forge is auto-detected from the host and API path;
// GitLab defaults to gitlab.com, while Forgejo/Gitea requires an explicit
// base URL. Public releases remain usable without a token.
func NewConfigured(baseURL, forgeName string) (*Client, error) {
	forge, normalizedBase, err := resolveClientConfig(baseURL, forgeName)
	if err != nil {
		return nil, err
	}
	token, header, prefix := tokenForForge(forge)
	return &Client{
		HTTP:        &http.Client{Timeout: 60 * time.Second},
		BaseURL:     normalizedBase,
		Token:       token,
		Forge:       forge,
		tokenHeader: header,
		tokenPrefix: prefix,
	}, nil
}

// ParseForge parses a supported forge name. The gitea spelling is accepted as
// an alias for ForgeForgejo because both use the same API dialect here.
func ParseForge(value string) (Forge, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(ForgeGitHub):
		return ForgeGitHub, nil
	case string(ForgeGitLab):
		return ForgeGitLab, nil
	case string(ForgeForgejo), "gitea":
		return ForgeForgejo, nil
	default:
		return "", fmt.Errorf("unsupported forge %q; expected github, gitlab, or forgejo", value)
	}
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func tokenForForge(forge Forge) (token, header, prefix string) {
	switch forge {
	case ForgeGitLab:
		if token := os.Getenv("CI_JOB_TOKEN"); token != "" {
			return token, "JOB-TOKEN", ""
		}
		return os.Getenv("GITLAB_TOKEN"), "PRIVATE-TOKEN", ""
	case ForgeForgejo:
		return firstNonEmptyEnv("FORGEJO_TOKEN", "CODEBERG_TOKEN"), "Authorization", "token "
	default:
		return firstNonEmptyEnv("ZENGET_GITHUB_TOKEN", "GITHUB_TOKEN"), "Authorization", "Bearer "
	}
}

func resolveClientConfig(rawBaseURL, rawForge string) (Forge, string, error) {
	forgeName := strings.TrimSpace(rawForge)
	var forge Forge
	var err error
	if forgeName != "" {
		forge, err = ParseForge(forgeName)
		if err != nil {
			return "", "", err
		}
	}

	base := strings.TrimSpace(rawBaseURL)
	if base == "" {
		switch forge {
		case ForgeGitLab:
			return ForgeGitLab, defaultGitLabBaseURL, nil
		case ForgeForgejo:
			return "", "", errors.New("--api-base-url is required for forgejo/gitea")
		default:
			return ForgeGitHub, defaultBaseURL, nil
		}
	}

	parsed, err := parseBaseURL(base)
	if err != nil {
		return "", "", err
	}
	if forge == "" {
		forge = inferForge(parsed)
	}

	switch forge {
	case ForgeGitLab:
		base, err = normalizeAPIBase(parsed, ForgeGitLab, "/api/v4")
	case ForgeForgejo:
		base, err = normalizeAPIBase(parsed, ForgeForgejo, "/api/v1")
	default:
		base = strings.TrimRight(parsed.String(), "/")
	}
	if err != nil {
		return "", "", err
	}
	return forge, base, nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("API base URL must not be empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return nil, fmt.Errorf("invalid API base URL %q: whitespace is not allowed", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid API base URL %q: expected an absolute HTTP(S) URL", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("invalid API base URL %q: scheme must be http or https", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid API base URL %q: credentials, query, and fragment are not allowed", raw)
	}
	return parsed, nil
}

func inferForge(baseURL *url.URL) Forge {
	if baseURL == nil {
		return ForgeGitHub
	}
	host := strings.ToLower(baseURL.Hostname())
	path := strings.TrimRight(baseURL.Path, "/")
	if host == "gitlab.com" || host == "www.gitlab.com" || strings.HasSuffix(path, "/api/v4") {
		return ForgeGitLab
	}
	if host == "codeberg.org" || strings.Contains(host, "forgejo") || strings.Contains(host, "gitea") || strings.HasSuffix(path, "/api/v1") {
		return ForgeForgejo
	}
	return ForgeGitHub
}

func normalizeAPIBase(baseURL *url.URL, forge Forge, apiPath string) (string, error) {
	if baseURL == nil {
		return "", fmt.Errorf("%s API base URL is missing", forge)
	}
	path := strings.TrimRight(baseURL.Path, "/")
	if strings.HasSuffix(path, apiPath) {
		baseURL.Path = path
		return strings.TrimRight(baseURL.String(), "/"), nil
	}
	if strings.Contains(path, "/api/") {
		return "", fmt.Errorf("invalid %s API base URL %q: path must end with %s", forge, baseURL.String(), apiPath)
	}
	baseURL.Path = path + apiPath
	return strings.TrimRight(baseURL.String(), "/"), nil
}

// LatestRelease fetches the latest release for a GitHub repository.
func (c *Client) LatestRelease(ctx context.Context, org, repo string) (*Release, error) {
	if c.forge() == ForgeGitLab {
		releases, err := c.ListReleases(ctx, org, repo)
		if err != nil {
			return nil, err
		}
		for i := range releases {
			if !releases[i].Draft && !releases[i].Prerelease {
				return &releases[i], nil
			}
		}
		if len(releases) > 0 {
			return &releases[0], nil
		}
		return nil, fmt.Errorf("GitLab project %s/%s has no releases", org, repo)
	}
	endpoint, err := c.releaseEndpoint(org, repo, "/releases/latest")
	if err != nil {
		return nil, err
	}
	return c.decodeRelease(ctx, endpoint, fmt.Sprintf("latest release %s/%s", org, repo))
}

// ReleaseByTag fetches the release with the exact tag for a GitHub repository.
func (c *Client) ReleaseByTag(ctx context.Context, org, repo, tag string) (*Release, error) {
	tail := "/releases/tags/" + url.PathEscape(tag)
	if c.forge() == ForgeGitLab {
		tail = "/releases/" + url.PathEscape(tag)
	}
	endpoint, err := c.releaseEndpoint(org, repo, tail)
	if err != nil {
		return nil, err
	}
	return c.decodeRelease(ctx, endpoint, fmt.Sprintf("release %s/%s tag %s", org, repo, tag))
}

// ContentsAtCommit fetches one regular file from a GitHub repository at an
// exact 40-character commit SHA. The Contents API response is decoded locally;
// its download_url is deliberately never followed or returned to callers.
func (c *Client) ContentsAtCommit(ctx context.Context, org, repo, commit, filePath string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("GitHub client is nil")
	}
	if c.forge() != ForgeGitHub {
		return nil, errors.New("registry contents require the GitHub forge")
	}
	if !validRepositoryComponent(org) || !validRepositoryComponent(repo) {
		return nil, errors.New("registry contents repository is invalid")
	}
	if !fullCommitPattern.MatchString(commit) {
		return nil, errors.New("registry contents ref must be exactly 40 hexadecimal characters")
	}
	cleanPath, err := validateContentsPath(filePath)
	if err != nil {
		return nil, err
	}
	structuredLimit, err := configuredLimit(func(value limits.Limits) int64 { return value.StructuredBytes })
	if err != nil {
		return nil, err
	}
	endpoint, err := c.contentsEndpoint(org, repo, strings.ToLower(commit), cleanPath)
	if err != nil {
		return nil, err
	}
	resp, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Request == nil || !sameOrigin(resp.Request.URL.String(), c.BaseURL) {
		return nil, errors.New("GitHub contents response crossed an untrusted host")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub contents request failed with status %s", resp.Status)
	}

	var content repositoryContent
	if err := limits.DecodeJSON(resp.Body, &content, structuredLimit, "GitHub contents", cleanPath); err != nil {
		return nil, fmt.Errorf("decode GitHub contents %q: %w", cleanPath, err)
	}
	if content.Type != "file" {
		return nil, fmt.Errorf("GitHub contents %q is not a regular file", cleanPath)
	}
	if content.Path != "" && content.Path != cleanPath {
		return nil, fmt.Errorf("GitHub contents response path %q does not match requested path", content.Path)
	}
	if content.Encoding != "base64" {
		return nil, fmt.Errorf("GitHub contents %q uses unsupported encoding %q", cleanPath, content.Encoding)
	}
	encoded := strings.ReplaceAll(strings.ReplaceAll(content.Content, "\n", ""), "\r", "")
	if encoded == "" {
		return nil, fmt.Errorf("GitHub contents %q has empty content", cleanPath)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode GitHub contents %q base64: %w", cleanPath, err)
	}
	if int64(len(decoded)) > structuredLimit {
		return nil, &limits.LimitError{Kind: "GitHub contents", Limit: structuredLimit, Input: cleanPath}
	}
	if content.Size >= 0 && content.Size != 0 && content.Size != int64(len(decoded)) {
		return nil, fmt.Errorf("GitHub contents %q size %d does not match decoded content size %d", cleanPath, content.Size, len(decoded))
	}
	return decoded, nil
}

// ContentsAt is a short alias for ContentsAtCommit.
func (c *Client) ContentsAt(ctx context.Context, org, repo, commit, filePath string) ([]byte, error) {
	return c.ContentsAtCommit(ctx, org, repo, commit, filePath)
}

type repositoryContent struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
}

func (c *Client) contentsEndpoint(org, repo, commit, filePath string) (string, error) {
	if c == nil {
		return "", errors.New("GitHub client is nil")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return "", errors.New("API base URL is empty")
	}
	components := strings.Split(filePath, "/")
	escapedPath := make([]string, len(components))
	for index, component := range components {
		escapedPath[index] = url.PathEscape(component)
	}
	return base + "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) + "/contents/" + strings.Join(escapedPath, "/") + "?ref=" + url.QueryEscape(commit), nil
}

func validateContentsPath(value string) (string, error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") || strings.ContainsAny(value, "\\%?#:\x00") {
		return "", errors.New("GitHub contents path must be a clean relative path")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("GitHub contents path cannot contain dot-segments")
		}
	}
	return value, nil
}

func validRepositoryComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' && char != '-' && char != '.' {
			return false
		}
	}
	return true
}

// ListReleases fetches the first release page for a GitHub repository. The
// unpaged behavior remains the default for callers that only need the normal
// latest-release workflow.
func (c *Client) ListReleases(ctx context.Context, org, repo string) ([]Release, error) {
	return c.listReleasesPage(ctx, org, repo, 0)
}

const (
	releasePageSize    = 100
	maxReleasePageScan = 1000
)

// ListReleasesAll fetches all release pages for a GitHub-compatible
// repository. It is intended for client-side release-series filtering, where
// a matching tag may be older than the first API page.
func (c *Client) ListReleasesAll(ctx context.Context, org, repo string) ([]Release, error) {
	var all []Release
	for page := 1; page <= maxReleasePageScan; page++ {
		releases, err := c.listReleasesPage(ctx, org, repo, page)
		if err != nil {
			return nil, err
		}
		all = append(all, releases...)
		if len(releases) < releasePageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("release list %s/%s exceeded %d pages", org, repo, maxReleasePageScan)
}

func (c *Client) listReleasesPage(ctx context.Context, org, repo string, page int) ([]Release, error) {
	structuredLimit, err := configuredLimit(func(value limits.Limits) int64 { return value.StructuredBytes })
	if err != nil {
		return nil, err
	}
	endpoint, err := c.releaseEndpoint(org, repo, "/releases")
	if err != nil {
		return nil, err
	}
	if page > 0 {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("build release list page URL: %w", err)
		}
		query := parsed.Query()
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", strconv.Itoa(releasePageSize))
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
	}
	resp, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}

	var releases []Release
	if c.forge() == ForgeGitLab {
		var gitlabReleases []gitlabRelease
		if err := limits.DecodeJSON(resp.Body, &gitlabReleases, structuredLimit, "structured JSON", fmt.Sprintf("release list %s/%s", org, repo)); err != nil {
			return nil, fmt.Errorf("decode release list: %w", err)
		}
		releases = make([]Release, 0, len(gitlabReleases))
		for _, release := range gitlabReleases {
			releases = append(releases, release.toRelease())
		}
	} else if err := limits.DecodeJSON(resp.Body, &releases, structuredLimit, "structured JSON", fmt.Sprintf("release list %s/%s", org, repo)); err != nil {
		return nil, fmt.Errorf("decode release list: %w", err)
	}

	return releases, nil
}

func (c *Client) decodeRelease(ctx context.Context, endpoint, context string) (*Release, error) {
	structuredLimit, err := configuredLimit(func(value limits.Limits) int64 { return value.StructuredBytes })
	if err != nil {
		return nil, err
	}
	resp, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}

	if c.forge() == ForgeGitLab {
		var gitlabRelease gitlabRelease
		if err := limits.DecodeJSON(resp.Body, &gitlabRelease, structuredLimit, "structured JSON", context); err != nil {
			return nil, fmt.Errorf("decode %s: %w", context, err)
		}
		release := gitlabRelease.toRelease()
		return &release, nil
	}

	var release Release
	if err := limits.DecodeJSON(resp.Body, &release, structuredLimit, "structured JSON", context); err != nil {
		return nil, fmt.Errorf("decode %s: %w", context, err)
	}

	return &release, nil
}

func (c *Client) releaseEndpoint(org, repo, suffix string) (string, error) {
	if c == nil {
		return "", errors.New("GitHub client is nil")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return "", errors.New("API base URL is empty")
	}
	if c.forge() == ForgeGitLab {
		return base + "/projects/" + url.PathEscape(org+"/"+repo) + suffix, nil
	}
	return base + "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo) + suffix, nil
}

type gitlabRelease struct {
	TagName         string              `json:"tag_name"`
	Draft           bool                `json:"draft"`
	UpcomingRelease bool                `json:"upcoming_release"`
	Prerelease      bool                `json:"prerelease"`
	ReleasedAt      time.Time           `json:"released_at"`
	Assets          gitlabReleaseAssets `json:"assets"`
}

type gitlabReleaseAssets struct {
	Links []gitlabAssetLink `json:"links"`
}

type gitlabAssetLink struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	DirectAssetURL string `json:"direct_asset_url"`
}

func (r gitlabRelease) toRelease() Release {
	release := Release{
		TagName:     r.TagName,
		Draft:       r.Draft || r.UpcomingRelease,
		Prerelease:  r.Prerelease,
		PublishedAt: r.ReleasedAt,
		Assets:      make([]Asset, 0, len(r.Assets.Links)),
	}
	for _, link := range r.Assets.Links {
		assetURL := link.DirectAssetURL
		if assetURL == "" {
			assetURL = link.URL
		}
		release.Assets = append(release.Assets, Asset{
			Name:               link.Name,
			URL:                assetURL,
			BrowserDownloadURL: link.URL,
		})
	}
	return release
}

// DownloadAsset downloads a release asset to destPath. GitHub's asset API URL
// is preferred because it supports authenticated private-repository downloads;
// older metadata without that URL falls back to the browser download URL. An
// optional per-call byte limit overrides the configured release download limit.
func (c *Client) DownloadAsset(ctx context.Context, asset Asset, destPath string, maxBytes ...int64) error {
	limit, err := configuredLimitArgument(maxBytes, func(value limits.Limits) int64 { return value.DownloadBytes })
	if err != nil {
		return fmt.Errorf("download asset %q: %w", asset.Name, err)
	}
	return c.downloadAsset(ctx, asset, destPath, limit, "release download")
}

// DownloadAssetWithLimit downloads a release asset with an explicit finite
// limit and diagnostic category. It is used when a caller applies a more
// specific budget, such as the checksum-asset limit, instead of the regular
// release-download limit.
func (c *Client) DownloadAssetWithLimit(ctx context.Context, asset Asset, destPath string, maxBytes int64, kind string) error {
	if maxBytes <= 0 {
		return fmt.Errorf("%s limit must be a positive int64", kind)
	}
	return c.downloadAsset(ctx, asset, destPath, maxBytes, kind)
}

func (c *Client) downloadAsset(ctx context.Context, asset Asset, destPath string, limit int64, kind string) error {
	endpoint := asset.URL
	if endpoint == "" {
		endpoint = asset.BrowserDownloadURL
	}
	if endpoint == "" {
		return fmt.Errorf("download asset: asset %q has no download URL", asset.Name)
	}
	endpoint = c.resolveAssetURL(endpoint)
	return c.downloadWithRetry(ctx, endpoint, binaryAccept, sameOrigin(endpoint, c.BaseURL), destPath, limit, kind, asset.Name)
}

// DownloadAssetURL downloads a URL directly for compatibility with callers
// that do not have release asset metadata. Direct URLs use the regular GitHub
// media negotiation and never receive a token unless they share the API
// origin.
func (c *Client) DownloadAssetURL(ctx context.Context, endpoint, destPath string, maxBytes ...int64) error {
	limit, err := configuredLimitArgument(maxBytes, func(value limits.Limits) int64 { return value.DownloadBytes })
	if err != nil {
		return fmt.Errorf("download asset: %w", err)
	}
	endpoint = c.resolveAssetURL(endpoint)
	return c.downloadWithRetry(ctx, endpoint, githubAccept, sameOrigin(endpoint, c.BaseURL), destPath, limit, "release download", endpoint)
}

func (c *Client) get(ctx context.Context, endpoint string) (*http.Response, error) {
	accept := githubAccept
	if c.forge() == ForgeGitLab {
		accept = jsonAccept
	}
	return c.do(ctx, endpoint, accept, sameOrigin(endpoint, c.BaseURL))
}

func (c *Client) do(ctx context.Context, endpoint, accept string, authorize bool) (*http.Response, error) {
	return c.doWithRange(ctx, endpoint, accept, authorize, 0)
}

func (c *Client) doWithRange(ctx context.Context, endpoint, accept string, authorize bool, rangeStart int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub request: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", userAgent)
	if rangeStart > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rangeStart))
	}
	if authorize && c.Token != "" {
		header, value := c.authHeader()
		req.Header.Set(header, value)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	client := *httpClient
	originalRedirect := httpClient.CheckRedirect
	client.CheckRedirect = func(redirect *http.Request, via []*http.Request) error {
		if len(via) > 0 && !sameURLOrigin(via[len(via)-1].URL, redirect.URL) {
			redirect.Header.Del("Authorization")
		}
		if originalRedirect != nil {
			return originalRedirect(redirect, via)
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send GitHub request: %w", err)
	}

	return resp, nil
}

func (c *Client) downloadWithRetry(ctx context.Context, endpoint, accept string, authorize bool, destPath string, maxBytes int64, kind, input string) error {
	if maxBytes <= 0 {
		return fmt.Errorf("%s limit must be a positive int64", kind)
	}

	temporary, err := os.CreateTemp(filepath.Dir(destPath), ".zenget-download-*")
	if err != nil {
		return fmt.Errorf("create asset file %q: %w", destPath, err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	temporaryClosed := false
	defer func() {
		if !temporaryClosed {
			_ = temporary.Close()
		}
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0644); err != nil {
		return fmt.Errorf("set asset file permissions %q: %w", destPath, err)
	}

	var offset int64
	for attempt := 0; attempt < downloadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("download asset: %w", err)
		}

		resp, err := c.doWithRange(ctx, endpoint, accept, authorize, offset)
		if err != nil {
			if isContextError(err) || ctx.Err() != nil {
				return fmt.Errorf("download asset: %w", contextError(ctx, err))
			}
			if !isRetryableRequestError(err) || attempt == downloadMaxAttempts-1 {
				return fmt.Errorf("download asset: %w", err)
			}
			if err := downloadRetryWait(ctx, retryDelay(nil, attempt)); err != nil {
				return fmt.Errorf("download asset: %w", err)
			}
			continue
		}

		if !isSuccessfulDownloadStatus(resp.StatusCode) {
			statusErr := responseError(resp)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable || !isRetryableStatus(resp.StatusCode) || attempt == downloadMaxAttempts-1 {
				return statusErr
			}
			if err := downloadRetryWait(ctx, retryDelay(resp, attempt)); err != nil {
				return fmt.Errorf("download asset: %w", err)
			}
			continue
		}

		plan, err := planDownloadResponse(resp, offset, maxBytes, kind, input)
		if err != nil {
			_ = resp.Body.Close()
			return err
		}

		responseOffset := offset
		if !plan.append {
			if err := temporary.Truncate(0); err != nil {
				_ = resp.Body.Close()
				return fmt.Errorf("reset asset file %q: %w", destPath, err)
			}
			if _, err := temporary.Seek(0, io.SeekStart); err != nil {
				_ = resp.Body.Close()
				return fmt.Errorf("seek asset file %q: %w", destPath, err)
			}
			responseOffset = 0
		} else if _, err := temporary.Seek(0, io.SeekEnd); err != nil {
			_ = resp.Body.Close()
			return fmt.Errorf("seek asset file %q: %w", destPath, err)
		}

		written, bodyErr := writeDownloadBody(resp, temporary, responseOffset, maxBytes, plan, kind, input, destPath)
		_ = resp.Body.Close()
		offset = responseOffset + written
		if bodyErr != nil {
			if !isRetryableBodyError(bodyErr) || attempt == downloadMaxAttempts-1 {
				return bodyErr
			}
			if err := temporary.Sync(); err != nil {
				return fmt.Errorf("sync asset file %q: %w", destPath, err)
			}
			if err := downloadRetryWait(ctx, retryDelay(nil, attempt)); err != nil {
				return fmt.Errorf("download asset: %w", err)
			}
			continue
		}

		if err := temporary.Sync(); err != nil {
			return fmt.Errorf("sync asset file %q: %w", destPath, err)
		}
		if err := temporary.Close(); err != nil {
			temporaryClosed = true
			return fmt.Errorf("close asset file %q: %w", destPath, err)
		}
		temporaryClosed = true
		if err := os.Rename(temporaryPath, destPath); err != nil {
			return fmt.Errorf("replace asset file %q: %w", destPath, err)
		}
		removeTemporary = false
		return nil
	}

	return fmt.Errorf("download asset: retry limit exhausted")
}

type downloadResponsePlan struct {
	append      bool
	expected    int64
	expectedSet bool
	total       int64
	totalSet    bool
	rangeLength int64
}

func planDownloadResponse(resp *http.Response, offset, maxBytes int64, kind, input string) (downloadResponsePlan, error) {
	switch resp.StatusCode {
	case http.StatusOK:
		if resp.ContentLength > maxBytes {
			return downloadResponsePlan{}, &limits.LimitError{Kind: kind, Limit: maxBytes, Input: input}
		}
		plan := downloadResponsePlan{}
		if resp.ContentLength >= 0 {
			plan.expected = resp.ContentLength
			plan.expectedSet = true
		}
		return plan, nil
	case http.StatusPartialContent:
		contentRange, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return downloadResponsePlan{}, fmt.Errorf("invalid Content-Range %q: %w", resp.Header.Get("Content-Range"), err)
		}
		if contentRange.start != offset {
			return downloadResponsePlan{}, fmt.Errorf("invalid Content-Range %q: starts at %d, want %d", resp.Header.Get("Content-Range"), contentRange.start, offset)
		}
		if contentRange.totalSet && contentRange.total > maxBytes {
			return downloadResponsePlan{}, &limits.LimitError{Kind: kind, Limit: maxBytes, Input: input}
		}
		if contentRange.length > maxBytes-offset {
			return downloadResponsePlan{}, &limits.LimitError{Kind: kind, Limit: maxBytes, Input: input}
		}
		if resp.ContentLength >= 0 && resp.ContentLength != contentRange.length {
			return downloadResponsePlan{}, fmt.Errorf("invalid Content-Range %q: Content-Length=%d, want %d", resp.Header.Get("Content-Range"), resp.ContentLength, contentRange.length)
		}
		return downloadResponsePlan{
			append:      true,
			expected:    contentRange.length,
			expectedSet: true,
			total:       contentRange.total,
			totalSet:    contentRange.totalSet,
			rangeLength: contentRange.length,
		}, nil
	default:
		return downloadResponsePlan{}, fmt.Errorf("unexpected download response status %s", resp.Status)
	}
}

type parsedContentRange struct {
	start    int64
	length   int64
	total    int64
	totalSet bool
}

func parseContentRange(value string) (parsedContentRange, error) {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bytes") {
		return parsedContentRange{}, errors.New("expected bytes start-end/total")
	}
	rangeAndTotal := strings.Split(parts[1], "/")
	if len(rangeAndTotal) != 2 {
		return parsedContentRange{}, errors.New("expected start-end/total")
	}
	startAndEnd := strings.Split(rangeAndTotal[0], "-")
	if len(startAndEnd) != 2 {
		return parsedContentRange{}, errors.New("expected start-end")
	}
	start, err := parseNonNegativeInt(startAndEnd[0])
	if err != nil {
		return parsedContentRange{}, fmt.Errorf("invalid start: %w", err)
	}
	end, err := parseNonNegativeInt(startAndEnd[1])
	if err != nil {
		return parsedContentRange{}, fmt.Errorf("invalid end: %w", err)
	}
	if end < start || end-start == int64(^uint64(0)>>1) {
		return parsedContentRange{}, errors.New("range end must be at or after start")
	}

	rangeLength := end - start + 1
	result := parsedContentRange{start: start, length: rangeLength}
	if rangeAndTotal[1] == "*" {
		return result, nil
	}
	total, err := parseNonNegativeInt(rangeAndTotal[1])
	if err != nil {
		return parsedContentRange{}, fmt.Errorf("invalid total: %w", err)
	}
	if total <= end {
		return parsedContentRange{}, errors.New("total must be greater than range end")
	}
	result.total = total
	result.totalSet = true
	return result, nil
}

func parseNonNegativeInt(value string) (int64, error) {
	if value == "" {
		return 0, errors.New("value is empty")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("value must be a non-negative integer")
	}
	return parsed, nil
}

type trackingReader struct {
	source io.Reader
	err    error
}

func (r *trackingReader) Read(buffer []byte) (int, error) {
	n, err := r.source.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

type retryableDownloadError struct {
	err error
}

func (e *retryableDownloadError) Error() string { return e.err.Error() }

func (e *retryableDownloadError) Unwrap() error { return e.err }

func writeDownloadBody(resp *http.Response, temporary *os.File, offset, maxBytes int64, plan downloadResponsePlan, kind, input, destPath string) (int64, error) {
	remaining := maxBytes - offset
	if remaining <= 0 {
		return 0, &limits.LimitError{Kind: kind, Limit: maxBytes, Input: input}
	}
	readLimit := remaining
	if plan.expectedSet && plan.expected < readLimit {
		readLimit = plan.expected
	}
	if plan.expectedSet && plan.expected == 0 {
		var probe [1]byte
		read, probeErr := io.ReadFull(resp.Body, probe[:])
		if read > 0 || probeErr == nil {
			return 0, fmt.Errorf("write asset file %q: response body exceeds declared length", destPath)
		}
		if !errors.Is(probeErr, io.EOF) {
			if isContextError(probeErr) {
				return 0, fmt.Errorf("write asset file %q: %w", destPath, probeErr)
			}
			return 0, &retryableDownloadError{err: fmt.Errorf("write asset file %q: %w", destPath, probeErr)}
		}
		return 0, nil
	}
	bounded := limits.NewReader(resp.Body, readLimit, kind, input)
	reader := &trackingReader{source: bounded}
	written, err := io.Copy(temporary, reader)
	if err != nil {
		var limitErr *limits.LimitError
		if errors.As(reader.err, &limitErr) {
			if plan.rangeLength > 0 && plan.rangeLength < remaining {
				return written, fmt.Errorf("write asset file %q: response body exceeds Content-Range length", destPath)
			}
			return written, fmt.Errorf("write asset file %q: %w", destPath, limitErr)
		}
		if reader.err != nil {
			if isContextError(reader.err) {
				return written, fmt.Errorf("write asset file %q: %w", destPath, reader.err)
			}
			return written, &retryableDownloadError{err: fmt.Errorf("write asset file %q: %w", destPath, reader.err)}
		}
		return written, fmt.Errorf("write asset file %q: %w", destPath, err)
	}
	if plan.expectedSet && written != plan.expected {
		return written, &retryableDownloadError{err: fmt.Errorf("write asset file %q: truncated response body: received %d bytes, want %d", destPath, written, plan.expected)}
	}
	if plan.totalSet && offset+written < plan.total {
		return written, &retryableDownloadError{err: fmt.Errorf("write asset file %q: incomplete ranged response: received %d of %d bytes", destPath, offset+written, plan.total)}
	}
	return written, nil
}

func isSuccessfulDownloadStatus(status int) bool {
	return status == http.StatusOK || status == http.StatusPartialContent
}

func isRetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= http.StatusInternalServerError && status <= 599)
}

func isRetryableRequestError(err error) bool {
	return strings.Contains(err.Error(), "send GitHub request")
}

func isRetryableBodyError(err error) bool {
	var retryable *retryableDownloadError
	return errors.As(err, &retryable)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		if delay, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			return delay
		}
	}
	delay := downloadRetryBase
	for i := 0; i < attempt && delay < downloadRetryMaximum; i++ {
		if delay > downloadRetryMaximum/2 {
			return downloadRetryMaximum
		}
		delay *= 2
	}
	if delay > downloadRetryMaximum {
		return downloadRetryMaximum
	}
	return delay
}

func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		delay := time.Duration(seconds) * time.Second
		if seconds > int64(downloadRetryMaximum/time.Second) || delay > downloadRetryMaximum {
			return downloadRetryMaximum, true
		}
		return delay, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := time.Until(when)
	if delay < 0 {
		delay = 0
	}
	if delay > downloadRetryMaximum {
		delay = downloadRetryMaximum
	}
	return delay, true
}

func waitForDownloadRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) forge() Forge {
	if c == nil {
		return ForgeGitHub
	}
	if c.Forge != "" {
		return c.Forge
	}
	baseURL, err := parseBaseURL(c.BaseURL)
	if err != nil {
		return ForgeGitHub
	}
	return inferForge(baseURL)
}

// ProviderName returns the canonical forge name used by this client.
func (c *Client) ProviderName() string {
	return string(c.forge())
}

func (c *Client) authHeader() (header, value string) {
	if c.tokenHeader != "" {
		return c.tokenHeader, c.tokenPrefix + c.Token
	}
	switch c.forge() {
	case ForgeGitLab:
		return "PRIVATE-TOKEN", c.Token
	case ForgeForgejo:
		return "Authorization", "token " + c.Token
	default:
		return "Authorization", "Bearer " + c.Token
	}
}

func (c *Client) resolveAssetURL(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.IsAbs() || c == nil {
		return endpoint
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return endpoint
	}
	return base.ResolveReference(parsed).String()
}

func configuredLimit(selectLimit func(limits.Limits) int64) (int64, error) {
	cfg, err := config.Load()
	if err != nil {
		return 0, fmt.Errorf("load config limits: %w", err)
	}
	limit := selectLimit(cfg.EffectiveLimits())
	if limit <= 0 {
		return 0, fmt.Errorf("configured resource limit must be positive")
	}
	return limit, nil
}

func configuredLimitArgument(arguments []int64, defaultLimit func(limits.Limits) int64) (int64, error) {
	if len(arguments) > 1 {
		return 0, errors.New("at most one download limit may be supplied")
	}
	if len(arguments) == 1 {
		if arguments[0] <= 0 {
			return 0, errors.New("download limit must be a positive int64")
		}
		return arguments[0], nil
	}
	return configuredLimit(defaultLimit)
}

func sameOrigin(left, right string) bool {
	leftURL, leftErr := url.Parse(left)
	rightURL, rightErr := url.Parse(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return sameURLOrigin(leftURL, rightURL)
}

func sameURLOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("GitHub request failed with status %s", resp.Status)
	}
	return fmt.Errorf("GitHub request failed with status %s: %s", resp.Status, message)
}

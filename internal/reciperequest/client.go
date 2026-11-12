// Package reciperequest submits explicitly approved GitHub registry recipe requests.
package reciperequest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"zenget/internal/github"
	"zenget/internal/limits"
	"zenget/internal/registry"
)

// Client is independent of the installed tool's release provider. Its API
// endpoint is fixed to GitHub.com; HTTP can be replaced for offline tests.
type Client struct {
	HTTP  *http.Client
	Token string
}

// New uses zenget's existing GitHub token priority and refuses redirects.
func New() *Client {
	c := github.New()
	c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{HTTP: c.HTTP, Token: c.Token}
}

// Marker returns the exact, normalized project identifier stored in requests.
func Marker(repository string) string {
	return "<!-- zenget-recipe-request: " + strings.ToLower(repository) + " -->"
}

func (c *Client) request(ctx context.Context, method, endpoint string, payload any, result any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return errors.New("cannot encode recipe request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+endpoint, body)
	if err != nil {
		return errors.New("cannot prepare GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "zenget")
	req.Header.Set("Content-Type", "application/json")
	// Copy the injected client so a caller cannot enable cross-host redirects.
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("GitHub request did not return a confirmed response")
	}
	defer func() { _ = resp.Body.Close() }()
	expected := http.StatusOK
	if method == http.MethodPost {
		expected = http.StatusCreated
	}
	if resp.StatusCode != expected {
		return fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	if err := limits.DecodeJSON(resp.Body, result, limits.DefaultStructuredBytes, "recipe request response", "GitHub"); err != nil {
		return errors.New("GitHub returned an invalid or oversized response")
	}
	return nil
}

// RecipeExists validates the current default-branch index under the selected
// root. It only checks existence and never changes the pinned local snapshot.
func (c *Client) RecipeExists(ctx context.Context, source registry.Source, repository string) (bool, error) {
	indexPath, err := source.IndexPath()
	if err != nil {
		return false, err
	}
	var content struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int64  `json:"size"`
	}
	endpoint := "/repos/" + source.RepositoryName() + "/contents/" + indexPath
	if err := c.request(ctx, http.MethodGet, endpoint, nil, &content); err != nil {
		return false, err
	}
	if content.Type != "file" || content.Path != indexPath || content.Encoding != "base64" {
		return false, errors.New("current registry index is not a confirmed file under the selected root")
	}
	data, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(content.Content))
	if err != nil || len(data) == 0 || int64(len(data)) != content.Size {
		return false, errors.New("current registry index content is invalid")
	}
	index, err := registry.DecodeIndex(data)
	if err != nil {
		return false, errors.New("current registry index failed strict validation")
	}
	for _, entry := range index.Entries {
		if strings.EqualFold(entry.Repository, repository) {
			return true, nil
		}
	}
	return false, nil
}

type issue struct {
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	URL         string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func issueURL(value, target string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	prefix := "/" + strings.ToLower(target) + "/issues/"
	path := strings.ToLower(parsed.Path)
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	number, err := strconv.Atoi(strings.TrimPrefix(path, prefix))
	return err == nil && number > 0
}

// FindOpen returns a confirmed exact open request URL. Truncated or incomplete
// search results abort submission instead of risking a duplicate.
func (c *Client) FindOpen(ctx context.Context, target, repository string) (string, error) {
	query := "repo:" + target + " is:issue is:open \"" + strings.ToLower(repository) + "\" in:title,body"
	var all []issue
	for page := 1; page <= 10; page++ {
		var result struct {
			Total      *int     `json:"total_count"`
			Incomplete *bool    `json:"incomplete_results"`
			Items      *[]issue `json:"items"`
		}
		endpoint := "/search/issues?q=" + url.QueryEscape(query) + "&per_page=100&page=" + strconv.Itoa(page)
		if err := c.request(ctx, http.MethodGet, endpoint, nil, &result); err != nil {
			return "", err
		}
		if result.Total == nil || result.Incomplete == nil || result.Items == nil || *result.Incomplete || *result.Total < 0 || *result.Total > 1000 {
			return "", errors.New("duplicate check is incomplete")
		}
		all = append(all, (*result.Items)...)
		if len(all) < *result.Total {
			if len(*result.Items) != 100 {
				return "", errors.New("duplicate check is incomplete")
			}
			continue
		}
		if len(all) != *result.Total {
			return "", errors.New("duplicate check is inconsistent")
		}
		for _, item := range all {
			if len(item.PullRequest) > 0 && string(item.PullRequest) != "null" {
				continue
			}
			if item.State != "open" || item.Title == "" {
				return "", errors.New("duplicate check returned incomplete issue metadata")
			}
			exact := strings.EqualFold(strings.TrimSpace(item.Title), "Recipe request: "+repository)
			for _, line := range strings.Split(item.Body, "\n") {
				if strings.EqualFold(strings.TrimSpace(line), Marker(repository)) {
					exact = true
				}
			}
			if exact {
				if !issueURL(item.URL, target) {
					return "", errors.New("existing recipe request URL could not be confirmed")
				}
				return item.URL, nil
			}
		}
		return "", nil
	}
	return "", errors.New("duplicate check exceeded GitHub search limits")
}

// Create sends exactly one POST and returns only a confirmed issue URL. It
// deliberately never retries an uncertain response.
func (c *Client) Create(ctx context.Context, target, title, body string) (string, error) {
	var result issue
	err := c.request(ctx, http.MethodPost, "/repos/"+target+"/issues", map[string]string{"title": title, "body": body}, &result)
	if err != nil {
		return "", err
	}
	if !issueURL(result.URL, target) {
		return "", errors.New("created issue URL could not be confirmed")
	}
	return result.URL, nil
}

package reciperequest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"zenget/internal/registry"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL)
	c := New()
	c.Token = "test-token"
	c.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" {
			t.Errorf("unexpected API: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing authentication")
		}
		clone := r.Clone(r.Context())
		clone.URL.Scheme = endpoint.Scheme
		clone.URL.Host = endpoint.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	return c
}

func source(t *testing.T) registry.Source {
	t.Helper()
	s, err := registry.ParseSource("github:acme/catalog@" + strings.Repeat("a", 40) + ":recipes")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewTokenPriority(t *testing.T) {
	t.Setenv("ZENGET_GITHUB_TOKEN", "primary")
	t.Setenv("GITHUB_TOKEN", "secondary")
	t.Setenv("GITLAB_TOKEN", "wrong")
	if New().Token != "primary" {
		t.Fatal("priority")
	}
	t.Setenv("ZENGET_GITHUB_TOKEN", "")
	if New().Token != "secondary" {
		t.Fatal("fallback")
	}
	t.Setenv("GITHUB_TOKEN", "")
	if New().Token != "" {
		t.Fatal("forge token leaked")
	}
}

func TestCurrentIndex(t *testing.T) {
	cases := []struct {
		name    string
		content any
		exists  bool
		fail    bool
	}{
		{"missing", map[string]any{"schema_version": 1, "entries": []any{}}, false, false},
		{"exists", map[string]any{"schema_version": 1, "entries": []any{map[string]any{"repository": "ACME/Widget", "path": "widget.json", "sha256": strings.Repeat("a", 64)}}}, true, false},
		{"unknown-field", map[string]any{"schema_version": 1, "entries": []any{}, "extra": true}, false, true},
		{"invalid-schema", map[string]any{"schema_version": 2, "entries": []any{}}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/catalog/contents/recipes/registry.json" || r.URL.RawQuery != "" {
					t.Errorf("not default branch/root: %s", r.URL)
				}
				data, _ := json.Marshal(tc.content)
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "path": "recipes/registry.json", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(data), "size": len(data)})
			})
			exists, err := c.RecipeExists(context.Background(), source(t), "acme/widget")
			if exists != tc.exists || (err != nil) != tc.fail {
				t.Fatalf("exists=%v err=%v", exists, err)
			}
		})
	}
	for _, content := range []map[string]any{
		{"type": "dir"},
		{"type": "file", "path": "elsewhere/registry.json", "encoding": "base64"},
		{"type": "file", "path": "recipes/registry.json", "encoding": "none"},
		{"type": "file", "path": "recipes/registry.json", "encoding": "base64", "content": "!!!"},
		{"type": "file", "path": "recipes/registry.json", "encoding": "base64", "content": "e30=", "size": 3},
	} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(content) })
		if _, err := c.RecipeExists(context.Background(), source(t), "acme/widget"); err == nil {
			t.Fatal("invalid contents accepted")
		}
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid source sent") })
	if _, err := c.RecipeExists(context.Background(), registry.Source{}, "acme/widget"); err == nil {
		t.Fatal("invalid source accepted")
	}
}

func TestDuplicateExactness(t *testing.T) {
	for _, tc := range []struct {
		name  string
		item  issue
		match bool
		fail  bool
	}{
		{"marker", issue{Title: "Please add a recipe", Body: Marker("ACME/widget"), State: "open", URL: "https://github.com/acme/catalog/issues/12"}, true, false},
		{"title", issue{Title: "Recipe request: Acme/Widget", State: "open", URL: "https://github.com/acme/catalog/issues/12"}, true, false},
		{"similar", issue{Title: "Recipe request: acme/widget-plus", Body: Marker("other/widget"), State: "open"}, false, false},
		{"pr", issue{Title: "Recipe request: acme/widget", State: "open", PullRequest: json.RawMessage(`{}`)}, false, false},
		{"closed", issue{Title: "Recipe request: acme/widget", State: "closed"}, false, true},
		{"invalid-url", issue{Title: "Recipe request: acme/widget", State: "open", URL: "https://github.com/other/catalog/issues/12"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/search/issues" || !strings.Contains(r.URL.Query().Get("q"), "repo:acme/catalog is:issue is:open") {
					t.Errorf("query: %s", r.URL)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "incomplete_results": false, "items": []issue{tc.item}})
			})
			found, err := c.FindOpen(context.Background(), "acme/catalog", "acme/widget")
			if (found != "") != tc.match || (err != nil) != tc.fail {
				t.Fatalf("found=%s err=%v", found, err)
			}
		})
	}
}

func TestIncompleteSearch(t *testing.T) {
	for _, value := range []string{`{}`, `{"total_count":0,"incomplete_results":true,"items":[]}`, `{"total_count":1001,"incomplete_results":false,"items":[]}`, `{"total_count":2,"incomplete_results":false,"items":[]}`, `{"total_count":0,"incomplete_results":false,"items":[{}]}`, `{"total_count":-1,"incomplete_results":false,"items":[]}`} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, value) })
		if _, err := c.FindOpen(context.Background(), "acme/catalog", "acme/widget"); err == nil {
			t.Fatalf("accepted incomplete: %s", value)
		}
	}
}

func TestSearchPagination(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		items := make([]issue, 100)
		for i := range items {
			items[i] = issue{Title: "unrelated", State: "open"}
		}
		if calls == 2 {
			items = []issue{{Title: "Recipe request: acme/widget", State: "open", URL: "https://github.com/acme/catalog/issues/2"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "incomplete_results": false, "items": items})
	})
	found, err := c.FindOpen(context.Background(), "acme/catalog", "acme/widget")
	if err != nil || found == "" || calls != 2 {
		t.Fatalf("%s %v calls=%d", found, err, calls)
	}
}

func TestCreateConfirmedOnlyAndNeverRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		response string
		fail     bool
	}{
		{"created", 201, `{"html_url":"https://github.com/acme/catalog/issues/3"}`, false},
		{"forbidden", 403, `secret`, true}, {"server", 500, `secret`, true}, {"malformed", 201, `{`, true},
		{"wrong-url", 201, `{"html_url":"https://evil.example/issues/3"}`, true},
		{"redirect", 302, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/repos/acme/catalog/issues" {
					t.Errorf("request %s %s", r.Method, r.URL)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["title"] != "title" || body["body"] != "approved" {
					t.Errorf("body=%v", body)
				}
				w.Header().Set("Location", "https://evil.example")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			})
			found, err := c.Create(context.Background(), "acme/catalog", "title", "approved")
			if (err != nil) != tc.fail || calls != 1 {
				t.Fatalf("%s %v calls=%d", found, err, calls)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("response secret leaked")
			}
		})
	}
	c := New()
	calls := 0
	c.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("secret token") })
	if _, err := c.Create(context.Background(), "acme/catalog", "title", "body"); err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestIssueURL(t *testing.T) {
	for _, value := range []string{"http://github.com/acme/catalog/issues/1", "https://u@github.com/acme/catalog/issues/1", "https://github.com/acme/catalog/issues/1?q=1", "https://github.com/acme/catalog/issues/1#x", "https://github.com/acme/catalog/issues/no", "https://github.com/acme/catalog/pull/1", "https://github.com/acme/catalog/issues/0"} {
		if issueURL(value, "acme/catalog") {
			t.Fatalf("accepted %s", value)
		}
	}
}

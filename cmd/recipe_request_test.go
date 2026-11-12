package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/github"
	"zenget/internal/recipepolicy"
	"zenget/internal/reciperequest"
	"zenget/internal/registry"
	"zenget/internal/wizard"
)

type requestTransport func(*http.Request) (*http.Response, error)

func (f requestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type requestFixture struct {
	releaseFail  bool
	exists       bool
	duplicate    bool
	incomplete   bool
	postFail     bool
	indexFail    bool
	ambiguous    bool
	ticketCalls  int
	releaseCalls int
	posts        int
	payload      map[string]string
	target       string
}

func recipeRequestFixture(t *testing.T, f *requestFixture) *github.Client {
	t.Helper()
	archive := tarGzAsset(t, "widget", []byte("#!/bin/sh\necho widget\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/widget/releases/latest":
			f.releaseCalls++
			if f.releaseFail {
				w.WriteHeader(503)
				return
			}
			assets := []map[string]string{{"name": "widget.tar.gz", "browser_download_url": "http://" + r.Host + "/asset"}}
			if f.ambiguous {
				assets = append(assets, map[string]string{"name": "widget-other.tar.gz", "browser_download_url": "http://" + r.Host + "/asset"})
			}
			_ = jsonEncode(w, map[string]any{"tag_name": "v1.0.0", "assets": assets})
		case r.URL.Path == "/asset":
			_, _ = w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/contents/recipes/registry.json"):
			f.ticketCalls++
			if r.Method != "GET" || r.URL.RawQuery != "" {
				t.Errorf("index request %s %s", r.Method, r.URL)
			}
			if f.indexFail {
				w.WriteHeader(403)
				return
			}
			entries := []registry.IndexEntry{}
			if f.exists {
				entries = append(entries, registry.IndexEntry{Repository: "acme/widget", Path: "widget.json", SHA256: strings.Repeat("a", 64)})
			}
			data, err := registry.MarshalIndex(registry.Index{SchemaVersion: 1, Entries: entries})
			if err != nil {
				t.Error(err)
			}
			writeContentsResponse(w, "recipes/registry.json", data)
		case r.URL.Path == "/search/issues":
			f.ticketCalls++
			items := []map[string]any{}
			if f.duplicate {
				items = append(items, map[string]any{"title": "Recipe request: acme/widget", "state": "open", "html_url": "https://github.com/acme/catalog/issues/8"})
			}
			_ = jsonEncode(w, map[string]any{"total_count": len(items), "incomplete_results": f.incomplete, "items": items})
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == "POST":
			f.ticketCalls++
			f.posts++
			f.target = r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&f.payload); err != nil {
				t.Error(err)
			}
			if r.Header.Get("Authorization") != "Bearer request-token" {
				t.Errorf("issue auth %s", r.Header.Get("Authorization"))
			}
			if f.postFail {
				w.WriteHeader(500)
				_, _ = io.WriteString(w, "secret error")
				return
			}
			w.WriteHeader(201)
			_ = jsonEncode(w, map[string]string{"html_url": "https://github.com/" + strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/issues") + "/issues/9"})
		default:
			t.Errorf("unexpected network request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL)
	transport := requestTransport(func(r *http.Request) (*http.Response, error) {
		cloned := r.Clone(r.Context())
		if cloned.URL.Host == "api.github.com" {
			cloned.URL.Scheme = endpoint.Scheme
			cloned.URL.Host = endpoint.Host
		}
		return http.DefaultTransport.RoundTrip(cloned)
	})
	original := newRecipeRequestClient
	newRecipeRequestClient = func() *reciperequest.Client { c := reciperequest.New(); c.HTTP.Transport = transport; return c }
	t.Cleanup(func() { newRecipeRequestClient = original })
	c := github.New()
	c.HTTP.Transport = transport
	return c
}

func requestEnv(t *testing.T) {
	t.Helper()
	setupAutoRegistryEnv(t)
	t.Setenv("CI", "")
	t.Setenv("ZENGET_GITHUB_TOKEN", "request-token")
	t.Setenv("GITHUB_TOKEN", "")
	seedRegistrySnapshot(t, "stable", "acme/other", "other")
}
func requestInstall(t *testing.T, c *github.Client, input string, options installOptions) (string, error) {
	t.Helper()
	options.allowRecipeRequest = true
	options.input = strings.NewReader(input)
	options.interactive = true
	var output strings.Builder
	err := runInstallWithOptions(context.Background(), c, "acme/widget", options, &output)
	return output.String(), err
}

func TestRecipeRequestAfterInstall(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{true: "failed", false: "success"}[failed], func(t *testing.T) {
			requestEnv(t)
			f := &requestFixture{releaseFail: failed}
			c := recipeRequestFixture(t, f)
			output, err := requestInstall(t, c, "yes\n", installOptions{goos: "linux", goarch: "arm64"})
			if (err != nil) != failed || f.posts != 1 || !strings.Contains(output, "Created recipe request: https://github.com/acme/catalog/issues/9") {
				t.Fatalf("err=%v posts=%d output=%s", err, f.posts, output)
			}
			if f.target != "/repos/acme/catalog/issues" || f.payload["title"] != "Recipe request: acme/widget" {
				t.Fatalf("target/payload: %s %v", f.target, f.payload)
			}
			body := f.payload["body"]
			for _, expected := range []string{reciperequest.Marker("acme/widget"), "https://github.com/acme/widget", "linux/arm64", "Registry name: stable", "Registry root: recipes", commandRegistryCommit, "zenget version:"} {
				if !strings.Contains(body, expected) {
					t.Errorf("missing %s in %s", expected, body)
				}
			}
			tag := "v1.0.0"
			outcome := "succeeded"
			if failed {
				tag = "unknown"
				outcome = "failed"
			}
			if !strings.Contains(body, tag) || !strings.Contains(body, "Installation: "+outcome) {
				t.Fatalf("body=%s", body)
			}
			for _, secret := range []string{"request-token", "secret", os.Getenv("HOME"), "503"} {
				if strings.Contains(body, secret) {
					t.Fatalf("sensitive body=%s", body)
				}
			}
			if !failed && strings.Index(output, "Installed widget") > strings.Index(output, "Proposed GitHub issue") {
				t.Fatal("prompt before install")
			}
			if strings.Index(output, body) > strings.Index(output, "Submit this recipe request") {
				t.Fatal("consent before full preview")
			}
		})
	}
}

func TestExplicitRegistryMissingPreservesError(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{}
	c := recipeRequestFixture(t, f)
	output, err := requestInstall(t, c, "y\n", installOptions{registryName: "stable", registryNameSet: true, noRegistry: true})
	if !errors.Is(err, registry.ErrRecipeNotFound) || f.releaseCalls != 0 || f.posts != 1 || !strings.Contains(output, "Installation: failed") {
		t.Fatalf("%v calls=%d posts=%d output=%s", err, f.releaseCalls, f.posts, output)
	}
}

func TestRecipeRequestDeclinedMakesNoRequests(t *testing.T) {
	for _, input := range []string{"\n", "n\n", "", "yes", "garbage\n"} {
		t.Run("input="+input, func(t *testing.T) {
			requestEnv(t)
			f := &requestFixture{}
			c := recipeRequestFixture(t, f)
			output, err := requestInstall(t, c, input, installOptions{})
			if err != nil || f.ticketCalls != 0 || !strings.Contains(output, "Title: Recipe request: acme/widget") {
				t.Fatalf("%v calls=%d output=%s", err, f.ticketCalls, output)
			}
		})
	}
}

func TestRecipeRequestChecksAndErrorsPreserveInstall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		f       requestFixture
		message string
		calls   int
		noToken bool
	}{
		{"already-present", requestFixture{exists: true}, "commit pin", 1, false},
		{"duplicate", requestFixture{duplicate: true}, "already exists: https://github.com/acme/catalog/issues/8", 2, false},
		{"incomplete", requestFixture{incomplete: true}, "duplicate check is incomplete", 2, false},
		{"index-failure", requestFixture{indexFail: true}, "HTTP 403", 1, false},
		{"post-uncertain", requestFixture{postFail: true}, "creation is not confirmed", 3, false},
		{"no-token", requestFixture{}, "ZENGET_GITHUB_TOKEN", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestEnv(t)
			if tc.noToken {
				t.Setenv("ZENGET_GITHUB_TOKEN", "")
			}
			c := recipeRequestFixture(t, &tc.f)
			configBefore, _ := registry.LoadConfig()
			output, err := requestInstall(t, c, "yes\n", installOptions{})
			if err != nil || tc.f.ticketCalls != tc.calls || !strings.Contains(output, tc.message) {
				t.Fatalf("%v calls=%d output=%s", err, tc.f.ticketCalls, output)
			}
			if tc.f.posts > 1 {
				t.Fatal("retry")
			}
			if tc.f.postFail && strings.Contains(output, "secret error") {
				t.Fatal("API body leaked")
			}
			if !tc.f.exists && !tc.f.duplicate && !strings.Contains(output, "https://github.com/acme/catalog/issues/new") {
				t.Fatal("manual URL missing")
			}
			configAfter, _ := registry.LoadConfig()
			before, _ := registry.MarshalConfig(configBefore)
			after, _ := registry.MarshalConfig(configAfter)
			if string(before) != string(after) {
				t.Fatal("pin changed")
			}
		})
	}
}

func TestRecipeRequestSuppressed(t *testing.T) {
	for _, mode := range []string{"noninteractive", "CI", "offline", "no-registry", "local-recipe", "no-config", "unsynced", "denied", "corrupt", "matching", "other-flow", "gitlab", "enterprise"} {
		t.Run(mode, func(t *testing.T) {
			requestEnv(t)
			f := &requestFixture{}
			c := recipeRequestFixture(t, f)
			options := installOptions{allowRecipeRequest: true, interactive: true, input: strings.NewReader("yes\n")}
			switch mode {
			case "noninteractive":
				options.interactive = false
			case "CI":
				t.Setenv("CI", "true")
			case "offline":
				options.offline = true
				options.registryName = "stable"
			case "no-registry":
				options.noRegistry = true
			case "local-recipe":
				options.recipePathSet = true
				options.recipePath = filepath.Join(t.TempDir(), "absent.json")
			case "no-config":
				requireRecipeRequestSetup(t, registry.SaveConfig(registry.DefaultConfig()))
			case "unsynced":
				t.Setenv("XDG_CACHE_HOME", t.TempDir())
			case "denied":
				requireRecipeRequestSetup(t, recipepolicy.Save(recipepolicy.Default()))
			case "corrupt":
				config, _ := registry.LoadConfig()
				s, _ := registry.ParseSource(config.Registries[0].Source)
				cache, _ := registry.NewCache()
				root := filepath.Join(cache.Root(), sourceNamespaceForCommandTest(s))
				requireRecipeRequestSetup(t, os.WriteFile(filepath.Join(root, registry.SnapshotPointerFileName), []byte("bad"), 0600))
			case "matching":
				seedRegistrySnapshot(t, "stable", "acme/widget", "widget")
			case "other-flow":
				options.allowRecipeRequest = false
			case "gitlab":
				c.Forge = github.ForgeGitLab
				options.registryName = "stable"
			case "enterprise":
				c.BaseURL = "https://enterprise.invalid/api/v3"
				options.registryName = "stable"
			}
			var output strings.Builder
			_ = runInstallWithOptions(context.Background(), c, "acme/widget", options, &output)
			if f.ticketCalls != 0 || strings.Contains(output.String(), "Proposed GitHub issue") {
				t.Fatalf("calls=%d output=%s", f.ticketCalls, output.String())
			}
		})
	}
}

func TestRecipeRequestSharesWizardBuffer(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{ambiguous: true}
	c := recipeRequestFixture(t, f)
	output, err := requestInstall(t, c, "2\ncustom-widget\nyes\n", installOptions{})
	if err != nil || f.posts != 1 || !strings.Contains(output, "/.local/bin/custom-widget") {
		t.Fatalf("%v posts=%d output=%s", err, f.posts, output)
	}
}

func TestRecipeRequestDoesNotPromptAfterAbortedWizard(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{ambiguous: true}
	c := recipeRequestFixture(t, f)
	for _, input := range []string{"", "bad\n"} {
		output, err := requestInstall(t, c, input, installOptions{})
		if err == nil || f.ticketCalls != 0 || strings.Contains(output, "Proposed GitHub issue") {
			t.Fatalf("%v output=%s", err, output)
		}
	}
}

func TestMultipleRecipeRequestTargets(t *testing.T) {
	requestEnv(t)
	config, _ := registry.LoadConfig()
	value := "github:beta/recipes@" + commandRegistryCommit + ":recipes"
	if _, err := config.Add("second", value); err != nil {
		t.Fatal(err)
	}
	requireRecipeRequestSetup(t, registry.SaveConfig(config))
	second, _ := registry.ParseSource(value)
	cache, _ := registry.NewCache()
	requireRecipeRequestSetup(t, recipepolicy.Save(recipepolicy.Policy{SchemaVersion: 1, Allow: []recipepolicy.Rule{{Host: "github.com", Owner: "acme", Repository: "catalog", PathPrefix: "recipes"}, {Host: "github.com", Owner: "beta", Repository: "recipes", PathPrefix: "recipes"}}}))
	if err := cache.Save(second, registry.Index{SchemaVersion: 1, Entries: []registry.IndexEntry{}}, map[string][]byte{}); err != nil {
		t.Fatal(err)
	}
	f := &requestFixture{}
	c := recipeRequestFixture(t, f)
	// Config is sorted: second comes before stable.
	output, err := requestInstall(t, c, "1\nyes\n", installOptions{})
	if err != nil || f.posts != 1 || f.target != "/repos/beta/recipes/issues" || !strings.Contains(output, "second (beta/recipes)") || !strings.Contains(output, "stable (acme/catalog)") {
		t.Fatalf("%v posts=%d target=%s output=%s", err, f.posts, f.target, output)
	}
}

func TestCanceledInstallNoRecipeRequest(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{}
	c := recipeRequestFixture(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output strings.Builder
	_ = runInstallWithOptions(ctx, c, "acme/widget", installOptions{allowRecipeRequest: true, interactive: true, input: strings.NewReader("yes\n")}, &output)
	if f.ticketCalls != 0 || strings.Contains(output.String(), "Proposed GitHub issue") {
		t.Fatalf("calls=%d output=%s", f.ticketCalls, output.String())
	}
}

func TestPreviewUnknownRootAndSafeTag(t *testing.T) {
	_, body := recipeRequestPreview("acme/widget", installOptions{goos: "darwin", goarch: "amd64"}, missingRegistryRecipe{name: "test", source: registry.Source{Owner: "acme", Repository: "catalog", Commit: commandRegistryCommit}}, "v1\nmetadata", true)
	if !strings.Contains(body, "(repository root)") || !strings.Contains(body, `"v1\nmetadata"`) {
		t.Fatalf("body=%s", body)
	}
}

func TestPolicyRecheckedAfterConsent(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{}
	c := recipeRequestFixture(t, f)
	// Revoke after the local resolution, at the start of the approved request flow.
	original := newRecipeRequestClient
	newRecipeRequestClient = func() *reciperequest.Client {
		requireRecipeRequestSetup(t, recipepolicy.Save(recipepolicy.Default()))
		return original()
	}
	output, err := requestInstall(t, c, "yes\n", installOptions{})
	if err != nil || f.ticketCalls != 0 || !strings.Contains(output, "policy no longer allows") {
		t.Fatalf("%v calls=%d output=%s", err, f.ticketCalls, output)
	}
}

func TestAbortedSessionDoesNotOffer(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{}
	c := recipeRequestFixture(t, f)
	session := wizard.New(strings.NewReader(""), io.Discard)
	_, _ = session.Ask("earlier", "value")
	output, err := requestInstall(t, c, "yes\n", installOptions{prompter: session})
	if err != nil || f.ticketCalls != 0 || strings.Contains(output, "Proposed GitHub issue") {
		t.Fatalf("%v output=%s", err, output)
	}
}

func TestNonTerminalInputs(t *testing.T) {
	if commandInputIsTerminal(strings.NewReader("yes\n")) || fileIsTerminal(nil) {
		t.Fatal("nonterminal treated as interactive")
	}
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if commandInputIsTerminal(file) {
		t.Fatal("null device treated as terminal")
	}
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	defer func() { stdinIsTerminal = old }()
	if !commandInputIsTerminal(os.Stdin) {
		t.Fatal("stdin detection ignored")
	}
}

func TestEmptyExplicitRegistryDoesNotFallBack(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{}
	c := recipeRequestFixture(t, f)
	output, err := requestInstall(t, c, "yes\n", installOptions{registryNameSet: true})
	if err == nil || !strings.Contains(err.Error(), "--registry must name") || f.releaseCalls != 0 || f.ticketCalls != 0 || strings.Contains(output, "Proposed GitHub issue") {
		t.Fatalf("%v output=%s", err, output)
	}
}

func TestMultipleRegistrySelectionDeclined(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{}
	c := recipeRequestFixture(t, f)
	config, _ := registry.LoadConfig()
	s, _ := registry.ParseSource(config.Registries[0].Source)
	var output strings.Builder
	options := installOptions{allowRecipeRequest: true, interactive: true, prompter: wizard.New(strings.NewReader(""), &output)}
	offerRecipeRequest(context.Background(), c, "acme/widget", options, []missingRegistryRecipe{{name: "a", source: s}, {name: "b", source: s}}, "", nil, &output)
	if f.ticketCalls != 0 || strings.Contains(output.String(), "Proposed GitHub issue") {
		t.Fatalf("output=%s", output.String())
	}
}

func TestIssueFailurePreservesOriginalInstallError(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{releaseFail: true, postFail: true}
	c := recipeRequestFixture(t, f)
	output, err := requestInstall(t, c, "yes\n", installOptions{})
	if err == nil || !strings.Contains(err.Error(), "503") || f.posts != 1 || !strings.Contains(output, "creation is not confirmed") {
		t.Fatalf("%v posts=%d output=%s", err, f.posts, output)
	}
}

func TestActualInstallCommandNoninteractiveRegistryMiss(t *testing.T) {
	requestEnv(t)
	f := &requestFixture{}
	recipeRequestFixture(t, f)
	output, _, err := executeRootCommand(t, "install", "acme/widget", "--registry", "stable")
	if !errors.Is(err, registry.ErrRecipeNotFound) || f.ticketCalls != 0 || strings.Contains(output, "Proposed GitHub issue") {
		t.Fatalf("%v output=%s", err, output)
	}
}

func requireRecipeRequestSetup(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/github"
	"zenget/internal/state"
)

func TestNewCommandProviderReadsForgeFlags(t *testing.T) {
	cmd := &cobra.Command{Use: "install"}
	cmd.Flags().String("api-base-url", "", "")
	cmd.Flags().String("forge", "", "")
	if err := cmd.Flags().Set("api-base-url", "https://gitlab.example.test/api/v4"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("forge", "gitlab"); err != nil {
		t.Fatal(err)
	}

	configured, err := newCommandProvider(cmd)
	if err != nil {
		t.Fatalf("newCommandProvider() error = %v", err)
	}
	client, ok := configured.(*github.Client)
	if !ok {
		t.Fatalf("provider type = %T, want *github.Client", configured)
	}
	if client.Forge != github.ForgeGitLab || client.BaseURL != "https://gitlab.example.test/api/v4" {
		t.Fatalf("configured client = forge %q, base %q", client.Forge, client.BaseURL)
	}
}

func TestRunListUsesConfiguredForgeAndAPIBaseURL(t *testing.T) {
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0", InstallPath: "/home/u/.local/bin/widget"},
	})

	var sawReleaseRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/acme/widget/releases" {
			t.Errorf("request path = %q, want GitLab releases path", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		sawReleaseRequest = true
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"tag_name":    "v2.0.0",
			"released_at": "2026-08-25T10:00:00Z",
			"assets":      map[string]any{"links": []map[string]string{}},
		}})
	}))
	defer server.Close()

	cmd := &cobra.Command{Use: "list"}
	cmd.Flags().String("api-base-url", "", "")
	cmd.Flags().String("forge", "", "")
	if err := cmd.Flags().Set("api-base-url", server.URL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("forge", "gitlab"); err != nil {
		t.Fatal(err)
	}
	listUpdates = true
	listJSON = false
	listCheck = false
	t.Cleanup(func() {
		listUpdates = false
		listJSON = false
		listCheck = false
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runList(cmd, nil); err != nil {
		t.Fatalf("runList() error = %v", err)
	}
	if !sawReleaseRequest || !strings.Contains(output.String(), "v2.0.0") {
		t.Fatalf("list request/output = (%v, %q)", sawReleaseRequest, output.String())
	}
}

func TestRunInstallUsesConfiguredForgeAndAPIBaseURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/acme/widget/releases":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"tag_name":    "v1.0.0",
				"released_at": "2026-08-25T10:00:00Z",
				"assets": map[string]any{"links": []map[string]string{{
					"name":             "widget",
					"url":              server.URL + "/assets/widget",
					"direct_asset_url": server.URL + "/assets/widget",
				}}},
			}})
		case "/assets/widget":
			_, _ = io.WriteString(w, "#!/bin/sh\necho widget\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cmd := &cobra.Command{Use: "install"}
	cmd.Flags().String("asset", "", "")
	cmd.Flags().String("bin-name", "", "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().String("tag", "", "")
	cmd.Flags().String("system", "", "")
	cmd.Flags().Int("min-age-days", 0, "")
	cmd.Flags().String("api-base-url", "", "")
	cmd.Flags().String("forge", "", "")
	if err := cmd.Flags().Set("api-base-url", server.URL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("forge", "gitlab"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runInstall(cmd, []string{"acme/widget"}); err != nil {
		t.Fatalf("runInstall() error = %v", err)
	}
	if !strings.Contains(output.String(), "Installed widget v1.0.0 to ") {
		t.Fatalf("install output = %q", output.String())
	}
}

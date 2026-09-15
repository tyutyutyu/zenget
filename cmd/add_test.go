package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/appcfg"
	"zenget/internal/manifest"
	"zenget/internal/provider"
	"zenget/internal/state"
)

type addTestProvider struct {
	latest       *provider.Release
	tagged       *provider.Release
	latestCalls  int
	taggedCalls  int
	requestedTag string
}

func (p *addTestProvider) ProviderName() string { return manifest.ProviderGitHub }

func (p *addTestProvider) LatestRelease(context.Context, string, string) (*provider.Release, error) {
	p.latestCalls++
	return p.latest, nil
}

func (p *addTestProvider) ReleaseByTag(_ context.Context, _, _, tag string) (*provider.Release, error) {
	p.taggedCalls++
	p.requestedTag = tag
	return p.tagged, nil
}

func (p *addTestProvider) ListReleases(context.Context, string, string) ([]provider.Release, error) {
	return nil, nil
}

func (p *addTestProvider) DownloadAsset(context.Context, provider.Asset, string, ...int64) error {
	return fmt.Errorf("add must not download assets")
}

func addTestRelease(tag string) *provider.Release {
	return &provider.Release{
		TagName: tag,
		Assets: []provider.Asset{
			{Name: "widget-linux-amd64.tar.gz", BrowserDownloadURL: "https://example.test/widget.tar.gz"},
			{Name: "widget-linux-arm64.tar.gz", BrowserDownloadURL: "https://example.test/widget-arm64.tar.gz"},
		},
	}
}

func newAddTestCommand(manifestPath string) (*cobra.Command, *bytes.Buffer) {
	command, output, _ := newBufferedCommand()
	command.Flags().String("manifest", manifestPath, "")
	command.Flags().String("tag", "", "")
	command.Flags().String("asset", "", "")
	command.Flags().String("bin-name", "", "")
	command.Flags().String("name", "", "")
	command.Flags().String("system", "", "")
	return command, output
}

func useAddTestProvider(t *testing.T, client provider.Provider) {
	t.Helper()
	original := addClientFactory
	addClientFactory = func(*cobra.Command) (provider.Provider, error) { return client, nil }
	t.Cleanup(func() { addClientFactory = original })
}

func setAddTestFlag(t *testing.T, command *cobra.Command, name, value string) {
	t.Helper()
	if err := command.Flags().Set(name, value); err != nil {
		t.Fatalf("set --%s: %v", name, err)
	}
}

func TestRunAddResolvesLatestAndWritesOnlyManifestChoices(t *testing.T) {
	setupManifestEnvironment(t)
	path := saveManifestFile(t, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps: []state.ManifestApp{{
			Repository: "zeta/tool",
			Provider:   manifest.ProviderGitHub,
			Tag:        "v9.0.0",
		}},
	})
	client := &addTestProvider{latest: addTestRelease("v2.3.4")}
	useAddTestProvider(t, client)
	command, output := newAddTestCommand(path)
	setAddTestFlag(t, command, "asset", "widget-linux-amd64.tar.gz")
	setAddTestFlag(t, command, "bin-name", "bin/widget")
	setAddTestFlag(t, command, "name", "widget-tool")
	setAddTestFlag(t, command, "system", "linux/amd64")

	if err := runAdd(command, []string{"acme/widget"}); err != nil {
		t.Fatalf("add latest: %v", err)
	}
	if client.latestCalls != 1 || client.taggedCalls != 0 {
		t.Fatalf("release calls = latest %d, tagged %d", client.latestCalls, client.taggedCalls)
	}
	loaded, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("load added manifest: %v", err)
	}
	if got := []string{loaded.Apps[0].Repository, loaded.Apps[1].Repository}; strings.Join(got, ",") != "acme/widget,zeta/tool" {
		t.Fatalf("manifest order = %v", got)
	}
	entry := loaded.Apps[0]
	if entry.Tag != "v2.3.4" || entry.Asset != "widget-linux-amd64.tar.gz" || entry.PlatformOS != "linux" || entry.PlatformArch != "amd64" || entry.ArchiveBinary != "bin/widget" || entry.TargetName != "widget-tool" {
		t.Fatalf("added entry = %#v", entry)
	}
	if got, want := output.String(), "Added acme/widget v2.3.4 in "+path+"\n"; got != want {
		t.Fatalf("add output = %q, want %q", got, want)
	}
	if loaded.SchemaURL != manifest.SchemaURL {
		t.Fatalf("schema URL = %q, want %q", loaded.SchemaURL, manifest.SchemaURL)
	}
	if loadedState, err := state.Load(); err != nil {
		t.Fatal(err)
	} else if len(loadedState.Apps) != 0 {
		t.Fatalf("add changed installed state: %#v", loadedState.Apps)
	}
	if configPath, err := appcfg.Path("acme/widget"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("add changed app config %q: %v", configPath, err)
	}
}

func TestRunAddUsesExactRequestedTag(t *testing.T) {
	setupManifestEnvironment(t)
	path := saveManifestFile(t, manifest.New())
	client := &addTestProvider{tagged: addTestRelease("v1.2.3")}
	useAddTestProvider(t, client)
	command, _ := newAddTestCommand(path)
	setAddTestFlag(t, command, "tag", "v1.2.3")
	setAddTestFlag(t, command, "asset", "widget-linux-amd64.tar.gz")

	if err := runAdd(command, []string{"acme/widget"}); err != nil {
		t.Fatalf("add exact tag: %v", err)
	}
	if client.latestCalls != 0 || client.taggedCalls != 1 || client.requestedTag != "v1.2.3" {
		t.Fatalf("release calls = latest %d, tagged %d, requested %q", client.latestCalls, client.taggedCalls, client.requestedTag)
	}
	loaded, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Apps) != 1 || loaded.Apps[0].Tag != "v1.2.3" {
		t.Fatalf("manifest after exact add = %#v", loaded.Apps)
	}
}

func TestRunAddRejectsExistingBeforeProviderAndReplacesCanonically(t *testing.T) {
	setupManifestEnvironment(t)
	path := saveManifestFile(t, state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		Apps: []state.ManifestApp{
			{Repository: "zeta/tool", Provider: manifest.ProviderGitHub, Tag: "v9.0.0"},
			{Repository: "acme/widget", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", Asset: "old.tar.gz"},
		},
	})
	originalBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	client := &addTestProvider{tagged: addTestRelease("v2.0.0")}
	useAddTestProvider(t, client)

	previousReplace := addReplace
	addReplace = false
	t.Cleanup(func() { addReplace = previousReplace })
	command, _ := newAddTestCommand(path)
	setAddTestFlag(t, command, "tag", "v2.0.0")
	setAddTestFlag(t, command, "asset", "widget-linux-amd64.tar.gz")
	if err := runAdd(command, []string{"acme/widget"}); err == nil || !strings.Contains(err.Error(), "use --replace") {
		t.Fatalf("existing add error = %v, want replace guidance", err)
	}
	if client.taggedCalls != 0 {
		t.Fatalf("provider called before existing-entry rejection: %d", client.taggedCalls)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(originalBytes, unchanged) {
		t.Fatalf("manifest changed after existing-entry rejection")
	}

	addReplace = true
	replaceCommand, output := newAddTestCommand(path)
	setAddTestFlag(t, replaceCommand, "tag", "v2.0.0")
	setAddTestFlag(t, replaceCommand, "asset", "widget-linux-amd64.tar.gz")
	if err := runAdd(replaceCommand, []string{"acme/widget"}); err != nil {
		t.Fatalf("replace add: %v", err)
	}
	loaded, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Apps) != 2 || loaded.Apps[0].Repository != "acme/widget" || loaded.Apps[1].Repository != "zeta/tool" {
		t.Fatalf("replaced order = %#v", loaded.Apps)
	}
	if loaded.Apps[0].Tag != "v2.0.0" || loaded.Apps[0].Asset != "widget-linux-amd64.tar.gz" || loaded.Apps[1].Tag != "v9.0.0" {
		t.Fatalf("replaced entries = %#v", loaded.Apps)
	}
	if !strings.Contains(output.String(), "Replaced acme/widget v2.0.0") {
		t.Fatalf("replace output = %q", output)
	}
}

func TestRunAddRejectsInvalidManifestBeforeProviderOrMutation(t *testing.T) {
	setupManifestEnvironment(t)
	for name, contents := range map[string]string{
		"unknown field":   `{"schema_version":1,"apps":[],"secret":"nope"}`,
		"duplicate field": `{"schema_version":1,"schema_version":1,"apps":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "zenget.json")
			if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			factoryCalls := 0
			original := addClientFactory
			addClientFactory = func(*cobra.Command) (provider.Provider, error) {
				factoryCalls++
				return &addTestProvider{tagged: addTestRelease("v1.0.0")}, nil
			}
			t.Cleanup(func() { addClientFactory = original })
			command, _ := newAddTestCommand(path)
			if err := runAdd(command, []string{"acme/widget"}); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
			if factoryCalls != 0 {
				t.Fatalf("provider factory called %d times", factoryCalls)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("invalid manifest was mutated")
			}
		})
	}
}

func TestRunAddRejectsInvalidRepositoryWithoutReadingManifest(t *testing.T) {
	command, _ := newAddTestCommand(filepath.Join(t.TempDir(), "missing.json"))
	if err := runAdd(command, []string{"not-a-repository"}); err == nil || !strings.Contains(err.Error(), "expected org/repo") {
		t.Fatalf("invalid repository error = %v", err)
	}
}

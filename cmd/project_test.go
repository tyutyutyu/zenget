package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/install"
	"zenget/internal/manifest"
	"zenget/internal/shim"
	"zenget/internal/state"
	"zenget/internal/trust"
)

func safeProjectDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	return directory
}

func projectCommandWithManifest(t *testing.T, manifestPath string) *cobra.Command {
	t.Helper()
	command, _, _ := newBufferedCommand()
	command.Flags().String("manifest", manifestPath, "")
	return command
}

func TestProjectTrustUntrustAndList(t *testing.T) {
	setupManifestEnvironment(t)
	project := safeProjectDirectory(t)
	manifestPath := filepath.Join(project, "zenget.json")
	writeCommandManifest(t, manifestPath, emptyCommandManifest())

	trustCommand := projectCommandWithManifest(t, manifestPath)
	if err := runProjectTrust(trustCommand, nil); err != nil {
		t.Fatalf("runProjectTrust() error = %v", err)
	}
	store, err := trust.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Find(manifestPath); err != nil || !ok {
		t.Fatalf("trusted manifest lookup = ok %t, err %v", ok, err)
	}
	if err := runProjectTrust(projectCommandWithManifest(t, manifestPath), nil); err != nil {
		t.Fatalf("repeat runProjectTrust() error = %v", err)
	}

	previousJSON := projectTrustListJSON
	projectTrustListJSON = false
	t.Cleanup(func() { projectTrustListJSON = previousJSON })
	listCommand, listOutput, _ := newBufferedCommand()
	if err := runProjectTrustList(listCommand, nil); err != nil {
		t.Fatalf("human trust list error = %v", err)
	}
	if !strings.Contains(listOutput.String(), manifestPath) || !strings.Contains(listOutput.String(), project) {
		t.Fatalf("human trust list = %q", listOutput.String())
	}

	projectTrustListJSON = true
	jsonCommand, jsonOutput, _ := newBufferedCommand()
	if err := runProjectTrustList(jsonCommand, nil); err != nil {
		t.Fatalf("JSON trust list error = %v", err)
	}
	var listed trust.Store
	if err := json.Unmarshal(jsonOutput.Bytes(), &listed); err != nil {
		t.Fatalf("decode trust list: %v", err)
	}
	if len(listed.Projects) != 1 || listed.Projects[0].ManifestPath != manifestPath {
		t.Fatalf("JSON trust list = %#v", listed)
	}

	untrustCommand := projectCommandWithManifest(t, manifestPath)
	if err := runProjectUntrust(untrustCommand, nil); err != nil {
		t.Fatalf("runProjectUntrust() error = %v", err)
	}
	store, err = trust.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Find(manifestPath); err != nil || ok {
		t.Fatalf("manifest after untrust = ok %t, err %v", ok, err)
	}
	if err := runProjectUntrust(projectCommandWithManifest(t, manifestPath), nil); err != nil {
		t.Fatalf("repeat runProjectUntrust() error = %v", err)
	}
}

func TestProjectTrustRejectsUnsafeManifestPath(t *testing.T) {
	setupManifestEnvironment(t)
	project := safeProjectDirectory(t)
	manifestPath := filepath.Join(project, "zenget.json")
	writeCommandManifest(t, manifestPath, emptyCommandManifest())
	if err := os.Chmod(project, 0775); err != nil {
		t.Fatal(err)
	}
	if err := runProjectTrust(projectCommandWithManifest(t, manifestPath), nil); err == nil || !strings.Contains(err.Error(), "group/world writable") {
		t.Fatalf("unsafe trust error = %v, want permission error", err)
	}
	store, err := trust.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Projects) != 0 {
		t.Fatalf("unsafe trust changed store: %#v", store.Projects)
	}
}

func TestProjectActivateCreatesOnlyShimWrappersAfterFullPreflight(t *testing.T) {
	setupManifestEnvironment(t)
	project := safeProjectDirectory(t)
	manifestPath := filepath.Join(project, "zenget.json")
	file := state.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Apps: []state.ManifestApp{
			{Repository: "acme/one", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", TargetName: "one"},
			{Repository: "acme/two", Provider: manifest.ProviderGitHub, Tag: "v2.0.0", TargetName: "two"},
		},
	}
	writeCommandManifest(t, manifestPath, file)
	if err := runProjectTrust(projectCommandWithManifest(t, manifestPath), nil); err != nil {
		t.Fatal(err)
	}

	if err := runProjectActivate(projectCommandWithManifest(t, manifestPath), nil); err != nil {
		t.Fatalf("runProjectActivate() error = %v", err)
	}
	for _, target := range []struct{ repo, name string }{{"acme/one", "one"}, {"acme/two", "two"}} {
		path, err := install.TargetPathFor(target.repo, target.name)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read activated wrapper %q: %v", path, err)
		}
		if !strings.Contains(string(data), "shim_runtime=") || !strings.Contains(string(data), "shim_target=\""+target.name+"\"") {
			t.Fatalf("wrapper %q is not a shim: %s", path, data)
		}
	}
	statePath, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("project activation changed global state: %v", err)
	}
	if runtimePath, err := shim.RuntimePath(); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(runtimePath); err != nil {
		t.Fatalf("shim runtime missing: %v", err)
	}
}

func TestProjectActivateEmptyManifestDoesNotPrepareRuntime(t *testing.T) {
	setupManifestEnvironment(t)
	project := safeProjectDirectory(t)
	manifestPath := filepath.Join(project, "zenget.json")
	writeCommandManifest(t, manifestPath, emptyCommandManifest())
	if err := runProjectTrust(projectCommandWithManifest(t, manifestPath), nil); err != nil {
		t.Fatal(err)
	}
	command, output, _ := newBufferedCommand()
	command.Flags().String("manifest", manifestPath, "")
	if err := runProjectActivate(command, nil); err != nil {
		t.Fatalf("empty activation error = %v", err)
	}
	if !strings.Contains(output.String(), "0 target shims") {
		t.Fatalf("empty activation output = %q", output.String())
	}
	runtimePath, err := shim.RuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runtimePath); !os.IsNotExist(err) {
		t.Fatalf("empty activation prepared runtime: %v", err)
	}
}

func TestProjectActivateForeignTargetFailsBeforeAnyMutation(t *testing.T) {
	setupManifestEnvironment(t)
	project := safeProjectDirectory(t)
	manifestPath := filepath.Join(project, "zenget.json")
	writeCommandManifest(t, manifestPath, state.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Apps: []state.ManifestApp{
			{Repository: "acme/one", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", TargetName: "one"},
			{Repository: "acme/two", Provider: manifest.ProviderGitHub, Tag: "v2.0.0", TargetName: "two"},
		},
	})
	if err := runProjectTrust(projectCommandWithManifest(t, manifestPath), nil); err != nil {
		t.Fatal(err)
	}
	foreignPath, err := install.TargetPathFor("acme/two", "two")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(foreignPath), 0755); err != nil {
		t.Fatal(err)
	}
	foreignData := []byte("#!/bin/sh\necho foreign\n")
	if err := os.WriteFile(foreignPath, foreignData, 0755); err != nil {
		t.Fatal(err)
	}
	firstPath, err := install.TargetPathFor("acme/one", "one")
	if err != nil {
		t.Fatal(err)
	}
	runtimePath, err := shim.RuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := runProjectActivate(projectCommandWithManifest(t, manifestPath), nil); err == nil || !strings.Contains(err.Error(), "foreign target") {
		t.Fatalf("foreign activation error = %v", err)
	}
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Fatalf("first target was mutated after foreign preflight: %v", err)
	}
	if _, err := os.Stat(runtimePath); !os.IsNotExist(err) {
		t.Fatalf("runtime was mutated after foreign preflight: %v", err)
	}
	got, err := os.ReadFile(foreignPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, foreignData) {
		t.Fatalf("foreign target changed: %q", got)
	}
}

func TestProjectActivateDuplicateTargetsFailsBeforeMutation(t *testing.T) {
	setupManifestEnvironment(t)
	project := safeProjectDirectory(t)
	manifestPath := filepath.Join(project, "zenget.json")
	writeCommandManifest(t, manifestPath, state.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Apps: []state.ManifestApp{
			{Repository: "acme/one", Provider: manifest.ProviderGitHub, Tag: "v1.0.0", TargetName: "same"},
			{Repository: "acme/two", Provider: manifest.ProviderGitHub, Tag: "v2.0.0", TargetName: "same"},
		},
	})
	if err := runProjectTrust(projectCommandWithManifest(t, manifestPath), nil); err != nil {
		t.Fatal(err)
	}
	runtimePath, err := shim.RuntimePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := runProjectActivate(projectCommandWithManifest(t, manifestPath), nil); err == nil || !strings.Contains(err.Error(), `target "same"`) {
		t.Fatalf("duplicate target error = %v", err)
	}
	if _, err := os.Stat(runtimePath); !os.IsNotExist(err) {
		t.Fatalf("runtime was mutated after duplicate-target preflight: %v", err)
	}
}

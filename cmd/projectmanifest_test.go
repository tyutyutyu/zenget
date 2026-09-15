package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/manifest"
	"zenget/internal/projectmanifest"
	"zenget/internal/state"
)

func changeWorkingDirectory(t *testing.T, directory string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

func writeCommandManifest(t *testing.T, path string, file state.Manifest) {
	t.Helper()
	data, err := manifest.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func emptyCommandManifest() state.Manifest { return manifest.New() }

func TestRunManifestPathReportsDiscoveredManifestHumanAndJSON(t *testing.T) {
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "project", "nested")
	if err := os.MkdirAll(workingDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "project", projectmanifest.DefaultFileName)
	writeCommandManifest(t, manifestPath, emptyCommandManifest())
	changeWorkingDirectory(t, workingDirectory)
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "")

	previousDiscovery := noManifestDiscovery
	noManifestDiscovery = false
	t.Cleanup(func() { noManifestDiscovery = previousDiscovery })
	previousJSON := manifestPathJSON
	manifestPathJSON = false
	t.Cleanup(func() { manifestPathJSON = previousJSON })

	humanCommand, humanOutput, _ := newBufferedCommand()
	if err := runManifestPath(humanCommand, nil); err != nil {
		t.Fatalf("human manifest path: %v", err)
	}
	for _, expected := range []string{
		"Manifest path: " + manifestPath,
		"Source: discovered",
		"Schema version: 1",
	} {
		if !strings.Contains(humanOutput.String(), expected) {
			t.Errorf("human output missing %q: %s", expected, humanOutput)
		}
	}

	manifestPathJSON = true
	jsonCommand, jsonOutput, _ := newBufferedCommand()
	if err := runManifestPath(jsonCommand, nil); err != nil {
		t.Fatalf("JSON manifest path: %v", err)
	}
	var report manifestPathReport
	if err := json.Unmarshal(jsonOutput.Bytes(), &report); err != nil {
		t.Fatalf("parse JSON path report: %v", err)
	}
	if report.Path != manifestPath || report.Source != string(projectmanifest.SourceDiscovered) || report.SchemaVersion != manifest.SchemaVersion {
		t.Fatalf("path report = %#v", report)
	}
}

func TestRunManifestPathExplicitOverrideWorksWhenDiscoveryDisabled(t *testing.T) {
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "project", "nested")
	if err := os.MkdirAll(workingDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	explicitPath := filepath.Join(root, "explicit.json")
	writeCommandManifest(t, explicitPath, emptyCommandManifest())
	changeWorkingDirectory(t, workingDirectory)
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "")

	previousDiscovery := noManifestDiscovery
	noManifestDiscovery = true
	t.Cleanup(func() { noManifestDiscovery = previousDiscovery })
	previousJSON := manifestPathJSON
	manifestPathJSON = true
	t.Cleanup(func() { manifestPathJSON = previousJSON })

	command, output, _ := newBufferedCommand()
	command.Flags().String("manifest", explicitPath, "")
	if err := runManifestPath(command, nil); err != nil {
		t.Fatalf("explicit manifest path: %v", err)
	}
	var report manifestPathReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Path != explicitPath || report.Source != string(projectmanifest.SourceFlag) {
		t.Fatalf("explicit path report = %#v", report)
	}
}

func TestRunManifestPathReturnsExitCodeTwoWhenAbsent(t *testing.T) {
	changeWorkingDirectory(t, t.TempDir())
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "")
	previousDiscovery := noManifestDiscovery
	noManifestDiscovery = false
	t.Cleanup(func() { noManifestDiscovery = previousDiscovery })

	command, output, _ := newBufferedCommand()
	err := runManifestPath(command, nil)
	var notFound *projectmanifest.NoManifestError
	if !errors.As(err, &notFound) || commandExitCode(err) != 2 {
		t.Fatalf("missing manifest error = %v, code %d", err, commandExitCode(err))
	}
	if output.Len() != 0 || !command.SilenceErrors || !command.SilenceUsage {
		t.Fatalf("missing manifest output/silence = %q/%t/%t", output, command.SilenceErrors, command.SilenceUsage)
	}
}

func TestRunApplyUsesDiscoveredManifestWithoutNetworkOrMutation(t *testing.T) {
	setupManifestEnvironment(t)
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "project", "nested")
	if err := os.MkdirAll(workingDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "project", projectmanifest.DefaultFileName)
	writeCommandManifest(t, manifestPath, emptyCommandManifest())
	changeWorkingDirectory(t, workingDirectory)
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "")
	previousDiscovery := noManifestDiscovery
	noManifestDiscovery = false
	t.Cleanup(func() { noManifestDiscovery = previousDiscovery })
	previousDryRun := applyDryRun
	applyDryRun = true
	t.Cleanup(func() { applyDryRun = previousDryRun })

	command, output, _ := newBufferedCommand()
	if err := runApply(command, nil); err != nil {
		t.Fatalf("apply discovered empty manifest: %v", err)
	}
	if got, want := output.String(), "Apply summary: 0 install, 0 restore, 0 unchanged, 0 failed\n"; got != want {
		t.Fatalf("apply output = %q, want %q", got, want)
	}
	statePath, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("discovery-only apply created state: %v", err)
	}
}

func TestRunAddUsesDiscoveredManifest(t *testing.T) {
	setupManifestEnvironment(t)
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "project", "nested")
	if err := os.MkdirAll(workingDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "project", projectmanifest.DefaultFileName)
	writeCommandManifest(t, manifestPath, emptyCommandManifest())
	changeWorkingDirectory(t, workingDirectory)
	t.Setenv("ZENGET_MANIFEST", "")
	t.Setenv("ZENGET_NO_MANIFEST_DISCOVERY", "")
	previousDiscovery := noManifestDiscovery
	noManifestDiscovery = false
	t.Cleanup(func() { noManifestDiscovery = previousDiscovery })

	client := &addTestProvider{tagged: addTestRelease("v1.2.3")}
	useAddTestProvider(t, client)
	command, _ := newAddTestCommand("")
	setAddTestFlag(t, command, "tag", "v1.2.3")
	setAddTestFlag(t, command, "asset", "widget-linux-amd64.tar.gz")
	if err := runAdd(command, []string{"acme/widget"}); err != nil {
		t.Fatalf("add discovered manifest: %v", err)
	}
	loaded, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Apps) != 1 || loaded.Apps[0].Repository != "acme/widget" || client.taggedCalls != 1 {
		t.Fatalf("discovered add = %#v, calls=%d", loaded.Apps, client.taggedCalls)
	}
	if _, err := state.Load(); err != nil {
		t.Fatal(err)
	}
}

func TestRunInitDoesNotDiscoverParentManifest(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "project")
	child := filepath.Join(parent, "nested")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(parent, projectmanifest.DefaultFileName)
	writeCommandManifest(t, parentPath, state.Manifest{
		SchemaVersion: manifest.SchemaVersion,
		Apps:          []state.ManifestApp{{Repository: "parent/tool", Provider: manifest.ProviderGitHub, Tag: "v1.0.0"}},
	})
	parentBefore, err := os.ReadFile(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	changeWorkingDirectory(t, child)

	previousForce := initForce
	initForce = false
	t.Cleanup(func() { initForce = previousForce })
	command, _, _ := newBufferedCommand()
	if err := runInit(command, nil); err != nil {
		t.Fatalf("init in child: %v", err)
	}
	childPath := filepath.Join(child, projectmanifest.DefaultFileName)
	childManifest, err := manifest.Load(childPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(childManifest.Apps) != 0 {
		t.Fatalf("init inherited parent entries: %#v", childManifest.Apps)
	}
	parentAfter, err := os.ReadFile(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parentBefore, parentAfter) {
		t.Fatal("init changed parent manifest")
	}
}

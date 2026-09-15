package projectmanifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zenget/internal/manifest"
	"zenget/internal/state"
)

func writeTestManifest(t *testing.T, path, repository string) {
	t.Helper()
	file := manifest.New()
	if repository != "" {
		file.Apps = []state.ManifestApp{{
			Repository: repository,
			Provider:   manifest.ProviderGitHub,
			Tag:        "v1.0.0",
		}}
	}
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

func TestResolveUsesFlagEnvironmentThenNearestDiscovery(t *testing.T) {
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "project", "nested")
	flagPath := filepath.Join(root, "flag.json")
	environmentPath := filepath.Join(root, "environment.json")
	writeTestManifest(t, flagPath, "flag/repository")
	writeTestManifest(t, environmentPath, "environment/repository")

	t.Setenv(manifestEnv, environmentPath)
	flagResolution, err := Resolve(Options{
		ExplicitPath:     filepath.Join("..", "..", "flag.json"),
		WorkingDirectory: workingDirectory,
	})
	if err != nil {
		t.Fatalf("flag resolution: %v", err)
	}
	if flagResolution.Source != SourceFlag || flagResolution.Path != filepath.Clean(flagPath) || flagResolution.Manifest.Apps[0].Repository != "flag/repository" {
		t.Fatalf("flag resolution = %#v", flagResolution)
	}

	environmentResolution, err := Resolve(Options{WorkingDirectory: workingDirectory})
	if err != nil {
		t.Fatalf("environment resolution: %v", err)
	}
	if environmentResolution.Source != SourceEnvironment || environmentResolution.Path != environmentPath || environmentResolution.Manifest.Apps[0].Repository != "environment/repository" {
		t.Fatalf("environment resolution = %#v", environmentResolution)
	}

	writeTestManifest(t, filepath.Join(root, "project", DefaultFileName), "project/repository")
	writeTestManifest(t, filepath.Join(workingDirectory, DefaultFileName), "nested/repository")
	t.Setenv(manifestEnv, "")
	discoveredResolution, err := Resolve(Options{WorkingDirectory: workingDirectory})
	if err != nil {
		t.Fatalf("discovery resolution: %v", err)
	}
	wantPath := filepath.Join(workingDirectory, DefaultFileName)
	if discoveredResolution.Source != SourceDiscovered || discoveredResolution.Path != wantPath || discoveredResolution.Manifest.Apps[0].Repository != "nested/repository" {
		t.Fatalf("discovered resolution = %#v", discoveredResolution)
	}
}

func TestResolveTreatsEnvironmentRelativePathsAsWorkingDirectoryRelative(t *testing.T) {
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "project", "nested")
	environmentPath := filepath.Join(root, "environment.json")
	writeTestManifest(t, environmentPath, "environment/repository")
	t.Setenv(manifestEnv, filepath.Join("..", "..", "environment.json"))

	resolution, err := Resolve(Options{WorkingDirectory: workingDirectory})
	if err != nil {
		t.Fatalf("relative environment resolution: %v", err)
	}
	if resolution.Source != SourceEnvironment || resolution.Path != environmentPath {
		t.Fatalf("relative environment resolution = %#v", resolution)
	}
}

func TestDiscoverSkipsDirectoriesAndSymlinkCandidates(t *testing.T) {
	tests := []struct {
		name   string
		create func(t *testing.T, path, parentManifest string)
	}{
		{
			name: "directory",
			create: func(t *testing.T, path, _ string) {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			create: func(t *testing.T, path, parentManifest string) {
				if err := os.Symlink(parentManifest, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			child := filepath.Join(parent, "child")
			if err := os.MkdirAll(child, 0755); err != nil {
				t.Fatal(err)
			}
			parentManifest := filepath.Join(parent, DefaultFileName)
			writeTestManifest(t, parentManifest, "parent/repository")
			test.create(t, filepath.Join(child, DefaultFileName), parentManifest)

			resolution, err := Resolve(Options{WorkingDirectory: child})
			if err != nil {
				t.Fatalf("discovery with %s candidate: %v", test.name, err)
			}
			if resolution.Path != parentManifest || resolution.Manifest.Apps[0].Repository != "parent/repository" {
				t.Fatalf("discovery with %s candidate = %#v", test.name, resolution)
			}
		})
	}
}

func TestResolveNoManifestStopsAtRootWithDistinctExitCode(t *testing.T) {
	root := t.TempDir()
	resolution, err := Resolve(Options{WorkingDirectory: filepath.Join(root, "missing", "nested")})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve() error = %v, want ErrNotFound", err)
	}
	var notFound *NoManifestError
	if !errors.As(err, &notFound) {
		t.Fatalf("Resolve() error type = %T, want NoManifestError", err)
	}
	if notFound.ExitCode() != 2 || notFound.Disabled {
		t.Fatalf("no-manifest error = %#v", notFound)
	}
	if resolution.Path != "" || !strings.Contains(err.Error(), DefaultFileName) || !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("no-manifest result = %#v, error %v", resolution, err)
	}
}

func TestResolveOptOutStillHonorsExplicitPaths(t *testing.T) {
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "project")
	if err := os.MkdirAll(workingDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	discoveredPath := filepath.Join(workingDirectory, DefaultFileName)
	explicitPath := filepath.Join(root, "explicit.json")
	writeTestManifest(t, discoveredPath, "discovered/repository")
	writeTestManifest(t, explicitPath, "explicit/repository")
	t.Setenv(manifestEnv, "")

	for name, options := range map[string]Options{
		"flag":   {ExplicitPath: explicitPath, WorkingDirectory: workingDirectory, NoDiscovery: true},
		"option": {WorkingDirectory: workingDirectory, NoDiscovery: true},
	} {
		t.Run(name, func(t *testing.T) {
			resolution, err := Resolve(options)
			if name == "flag" {
				if err != nil || resolution.Source != SourceFlag || resolution.Manifest.Apps[0].Repository != "explicit/repository" {
					t.Fatalf("explicit opt-out resolution = %#v, %v", resolution, err)
				}
				return
			}
			var notFound *NoManifestError
			if !errors.As(err, &notFound) || !notFound.Disabled {
				t.Fatalf("option opt-out error = %v", err)
			}
		})
	}

	t.Setenv(noDiscoveryEnv, "1")
	if _, err := Resolve(Options{WorkingDirectory: workingDirectory}); err == nil {
		t.Fatal("environment opt-out was ignored")
	}
	if resolution, err := Resolve(Options{ExplicitPath: explicitPath, WorkingDirectory: workingDirectory}); err != nil || resolution.Source != SourceFlag {
		t.Fatalf("explicit path with environment opt-out = %#v, %v", resolution, err)
	}
}

func TestResolveStrictValidationReportsSourceAndCanonicalPath(t *testing.T) {
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "project", "nested")
	if err := os.MkdirAll(workingDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	invalidPath := filepath.Join(root, "invalid.json")
	if err := os.WriteFile(invalidPath, []byte(`{"schema_version":1,"apps":[],"unknown":true}`), 0644); err != nil {
		t.Fatal(err)
	}

	resolution, err := Resolve(Options{
		ExplicitPath:     filepath.Join("..", "..", "invalid.json"),
		WorkingDirectory: workingDirectory,
	})
	if resolution.Path != "" {
		t.Fatalf("invalid resolution = %#v", resolution)
	}
	var selectionError *SelectionError
	if !errors.As(err, &selectionError) {
		t.Fatalf("Resolve() error type = %T, want SelectionError", err)
	}
	if selectionError.Source != SourceFlag || selectionError.Path != invalidPath {
		t.Fatalf("selection error = %#v", selectionError)
	}
	if !strings.Contains(err.Error(), "source=flag") || !strings.Contains(err.Error(), invalidPath) || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("diagnostic = %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "project", DefaultFileName), []byte(`{"schema_version":1,"apps":[] {}`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(Options{WorkingDirectory: workingDirectory})
	if !strings.Contains(err.Error(), "source=discovered") || !strings.Contains(err.Error(), filepath.Join(root, "project", DefaultFileName)) {
		t.Fatalf("discovery diagnostic = %v", err)
	}
}

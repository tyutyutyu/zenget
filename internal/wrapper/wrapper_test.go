package wrapper

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRealPathHonorsXDGDataHome(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	got, err := RealPath("org/repo")
	if err != nil {
		t.Fatalf("RealPath() error = %v", err)
	}
	want := filepath.Join(dataHome, "zenget", "bin", "repo")
	if got != want {
		t.Errorf("RealPath() = %q, want %q", got, want)
	}
}

func TestVersionedRealPathNamespacesRepositoryAndVersion(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	got, err := VersionedRealPath("github", "acme/tool", "v1.2.3", "tool")
	if err != nil {
		t.Fatalf("VersionedRealPath() error = %v", err)
	}
	want := filepath.Join(dataHome, "zenget", "bin", "github", "acme", "tool", "v1.2.3", "tool")
	if got != want {
		t.Errorf("VersionedRealPath() = %q, want %q", got, want)
	}
}

func TestVersionedRealPathEscapesUnsafeComponents(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	got, err := VersionedRealPath("git lab", "acme/tools", "../nightly/1", "tool name")
	if err != nil {
		t.Fatalf("VersionedRealPath() error = %v", err)
	}
	if strings.Contains(got, string(filepath.Separator)+".."+string(filepath.Separator)) {
		t.Fatalf("VersionedRealPath() contains traversal segment: %q", got)
	}
	for _, want := range []string{"git%20lab", "nightly%2F1", "tool%20name"} {
		if !strings.Contains(got, want) {
			t.Errorf("VersionedRealPath() = %q, want escaped component %q", got, want)
		}
	}
}

func TestVersionedRealPathSeparatesSameBasename(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	first, err := VersionedRealPath("github", "acme/tool", "v1", "tool")
	if err != nil {
		t.Fatalf("first VersionedRealPath() error = %v", err)
	}
	second, err := VersionedRealPath("github", "other/tool", "v1", "tool")
	if err != nil {
		t.Fatalf("second VersionedRealPath() error = %v", err)
	}
	if first == second {
		t.Fatalf("same-basename repositories share artifact path %q", first)
	}
}

func TestCacheArtifactPathUsesVersionedStorageComponents(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	got, err := CacheArtifactPath("github", "acme/tool", "v1.2.3", "variant", "tool")
	if err != nil {
		t.Fatalf("CacheArtifactPath() error = %v", err)
	}
	want := filepath.Join(dataHome, "zenget", "cache", "github", "acme", "tool", "v1.2.3", "variant", "tool")
	if got != want {
		t.Errorf("CacheArtifactPath() = %q, want %q", got, want)
	}
}

func TestCacheArtifactPathEscapesUnsafeVariant(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	got, err := CacheArtifactPath("github", "acme/tool", "v1", "../variant", "tool")
	if err != nil {
		t.Fatalf("CacheArtifactPath() error = %v", err)
	}
	if strings.Contains(got, string(filepath.Separator)+".."+string(filepath.Separator)) {
		t.Fatalf("CacheArtifactPath() contains traversal segment: %q", got)
	}
}

func TestVersionedRealPathRejectsUnsafeRepositoryAndTarget(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, test := range []struct {
		name    string
		repo    string
		version string
		target  string
	}{
		{name: "repository traversal", repo: "../tool", version: "v1", target: "tool"},
		{name: "repository missing owner", repo: "tool", version: "v1", target: "tool"},
		{name: "empty version", repo: "acme/tool", version: "", target: "tool"},
		{name: "target traversal", repo: "acme/tool", version: "v1", target: "../tool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := VersionedRealPath("github", test.repo, test.version, test.target); err == nil {
				t.Fatal("VersionedRealPath() error = nil, want validation error")
			}
		})
	}
}

func TestUsageLogPathHonorsXDGDataHome(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	got, err := UsageLogPath("org/repo")
	if err != nil {
		t.Fatalf("UsageLogPath() error = %v", err)
	}
	want := filepath.Join(dataHome, "zenget", "usage", "github", "org", "repo.jsonl")
	if got != want {
		t.Errorf("UsageLogPath() = %q, want %q", got, want)
	}
}

func TestUsageLogPathsIncludeLegacyFallback(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	paths, err := UsageLogPaths("org/repo")
	if err != nil {
		t.Fatalf("UsageLogPaths() error = %v", err)
	}
	want := []string{
		filepath.Join(dataHome, "zenget", "usage", "github", "org", "repo.jsonl"),
		filepath.Join(dataHome, "zenget", "usage", "repo.jsonl"),
	}
	if strings.Join(paths, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("UsageLogPaths() = %q, want %q", paths, want)
	}
}

func TestUsageLogPathSeparatesSameBasenameRepositories(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	first, err := UsageLogPath("acme/tool")
	if err != nil {
		t.Fatal(err)
	}
	second, err := UsageLogPath("other/tool")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("same-basename repositories share usage log path %q", first)
	}
}

func TestIsManagedArtifactPath(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	inside := filepath.Join(dataHome, "zenget", "bin", "github", "org", "repo", "v1", "repo")
	managed, err := IsManagedArtifactPath(inside)
	if err != nil {
		t.Fatalf("IsManagedArtifactPath() error = %v", err)
	}
	if !managed {
		t.Fatal("IsManagedArtifactPath() = false for path inside artifact root")
	}
	outside := filepath.Join(dataHome, "zenget", "usage", "repo.jsonl")
	managed, err = IsManagedArtifactPath(outside)
	if err != nil {
		t.Fatalf("IsManagedArtifactPath() error = %v", err)
	}
	if managed {
		t.Fatal("IsManagedArtifactPath() = true for path outside artifact root")
	}
}

func TestTargetPathUsesInstallDir(t *testing.T) {
	installDir := t.TempDir()

	got, err := TargetPath("org/repo", installDir)
	if err != nil {
		t.Fatalf("TargetPath() error = %v", err)
	}
	want := filepath.Join(installDir, "repo")
	if got != want {
		t.Errorf("TargetPath() = %q, want %q", got, want)
	}
}

func TestTargetPathForUsesCustomName(t *testing.T) {
	installDir := t.TempDir()

	got, err := TargetPathFor("org/repo", "custom", installDir)
	if err != nil {
		t.Fatalf("TargetPathFor() error = %v", err)
	}
	if want := filepath.Join(installDir, "custom"); got != want {
		t.Errorf("TargetPathFor() = %q, want %q", got, want)
	}
}

func TestTargetPathForRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../escape", `sub\\name`, "sub/name", ".", ".."} {
		if _, err := TargetPathFor("org/repo", name, t.TempDir()); err == nil {
			t.Errorf("TargetPathFor(%q) error = nil, want validation error", name)
		}
	}
}

func TestTargetPathRejectsEmptyInstallDir(t *testing.T) {
	if _, err := TargetPath("org/repo", ""); err == nil {
		t.Fatal("TargetPath() error = nil, want error for empty install directory")
	}
}

func TestGenerateContainsMarker(t *testing.T) {
	script := string(Generate("org/repo", "/real/repo", "/log/repo.jsonl"))
	if !strings.Contains(script, Marker) {
		t.Errorf("generated wrapper missing marker %q", Marker)
	}
	if !strings.Contains(script, "/real/repo") {
		t.Error("generated wrapper missing real binary path")
	}
	if !strings.Contains(script, "/log/repo.jsonl") {
		t.Error("generated wrapper missing usage log path")
	}
	if !strings.Contains(script, "exec \"$real_bin\"") {
		t.Error("generated wrapper missing forwarding exec")
	}
	if !strings.Contains(script, "zenget flush") {
		t.Error("generated wrapper missing flush invocation")
	}
}

func TestGenerateShimUsesAbsoluteRuntimeAndTarget(t *testing.T) {
	script := string(GenerateShim("org/repo", "/data/zenget/shim/runtime", "custom-tool"))
	for _, want := range []string{Marker, "/data/zenget/shim/runtime", "custom-tool", `exec "$shim_runtime" shim "$shim_target" "$@"`} {
		if !strings.Contains(script, want) {
			t.Errorf("generated shim wrapper missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "usage_log") || strings.Contains(script, "real_bin") {
		t.Errorf("generated shim wrapper contains legacy forwarding state:\n%s", script)
	}
}

func TestIsMarkerLine(t *testing.T) {
	if !IsMarkerLine("# zenget-managed wrapper for org/repo") {
		t.Error("IsMarkerLine() = false for marker line, want true")
	}
	if IsMarkerLine("#!/bin/sh") {
		t.Error("IsMarkerLine() = true for unrelated line, want false")
	}
}

func TestDataHomeFallsBackToLocalShare(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", home)

	got, err := dataHome()
	if err != nil {
		t.Fatalf("dataHome() error = %v", err)
	}
	want := filepath.Join(home, ".local", "share")
	if got != want {
		t.Errorf("dataHome() = %q, want %q", got, want)
	}
}

func TestDataHomeHonorsEnvVar(t *testing.T) {
	dataHomeDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHomeDir)

	got, err := dataHome()
	if err != nil {
		t.Fatalf("dataHome() error = %v", err)
	}
	if got != dataHomeDir {
		t.Errorf("dataHome() = %q, want %q", got, dataHomeDir)
	}
}

func TestPathsWithoutHomeReturnError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "")

	if _, err := RealPath("org/repo"); err == nil {
		t.Error("RealPath() error = nil, want user home directory error")
	}
	if _, err := UsageLogPath("org/repo"); err == nil {
		t.Error("UsageLogPath() error = nil, want user home directory error")
	}
}

// Package wrapper generates POSIX shell wrappers that forward invocations to
// the real installed binary and optionally record usage events.
package wrapper

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Marker is embedded in generated wrappers so zenget can recognise its own
// files and distinguish them from foreign binaries.
const Marker = "zenget-managed wrapper"

// DefaultProvider is the provider namespace used by the GitHub-backed CLI.
// Keeping it here lets storage paths include the provider even while the CLI
// has only one provider implementation.
const DefaultProvider = "github"

// IsMarkerLine reports whether line contains the zenget wrapper marker.
func IsMarkerLine(line string) bool {
	return strings.Contains(line, Marker)
}

// RealPath returns the legacy real binary path for repo. New installations use
// VersionedRealPath; this helper remains for reading and migrating older state
// files.
func RealPath(repo string) (string, error) {
	return LegacyRealPath(repo)
}

// LegacyRealPath returns the pre-versioned real binary path for repo.
func LegacyRealPath(repo string) (string, error) {
	dataHome, err := dataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataHome, "zenget", "bin", path.Base(repo)), nil
}

// IsManagedArtifactPath reports whether candidate is lexically below the
// zenget artifact directory. It is used before deleting paths read from state
// so a corrupted registry cannot redirect cleanup outside the data root.
func IsManagedArtifactPath(candidate string) (bool, error) {
	if candidate == "" {
		return false, nil
	}
	dataHome, err := dataHome()
	if err != nil {
		return false, err
	}
	root, err := filepath.Abs(filepath.Join(dataHome, "zenget", "bin"))
	if err != nil {
		return false, fmt.Errorf("resolve artifact root: %w", err)
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return false, fmt.Errorf("resolve artifact path %q: %w", candidate, err)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil {
		return false, fmt.Errorf("compare artifact path %q: %w", candidate, err)
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false, nil
	}
	return true, nil
}

// VersionedRealPath returns the internal path for an installed artifact. Each
// provider, repository, release version, and target name occupies its own
// validated path component below the XDG data directory.
func VersionedRealPath(provider, repo, version, targetName string) (string, error) {
	dataHome, err := dataHome()
	if err != nil {
		return "", err
	}
	providerPart, ownerPart, repositoryPart, versionPart, targetPart, err := storageParts(provider, repo, version, targetName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataHome, "zenget", "bin", providerPart, ownerPart, repositoryPart, versionPart, targetPart), nil
}

// CacheRoot returns the directory that stores immutable downloaded artifacts
// and their index. The cache deliberately lives beside, but separately from,
// installed binaries so uninstalling an application never removes reusable
// release content.
func CacheRoot() (string, error) {
	dataHome, err := dataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataHome, "zenget", "cache"), nil
}

// ShimRuntimePath returns the absolute path of the managed dispatcher runtime.
// The runtime lives below the XDG data directory so wrappers do not depend on
// a zenget executable being discoverable through PATH.
func ShimRuntimePath() (string, error) {
	dataHome, err := dataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataHome, "zenget", "shim", "runtime"), nil
}

// CacheArtifactPath returns the path for one cache artifact. It reuses the
// same provider/repository/version/target component encoding as
// VersionedRealPath; variant is an additional validated cache identity
// component (normally a digest of the complete cache key).
func CacheArtifactPath(provider, repo, version, variant, targetName string) (string, error) {
	root, err := CacheRoot()
	if err != nil {
		return "", err
	}
	return CacheArtifactPathAt(root, provider, repo, version, variant, targetName)
}

// CacheArtifactPathAt is CacheArtifactPath with an explicit cache root. It is
// useful to the cache package's isolated tests and keeps all path validation in
// this package's established storage model.
func CacheArtifactPathAt(root, provider, repo, version, variant, targetName string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("cache root is empty")
	}
	providerPart, ownerPart, repositoryPart, versionPart, targetPart, err := storageParts(provider, repo, version, targetName)
	if err != nil {
		return "", err
	}
	variantPart, err := encodeStorageComponent(variant, "cache variant")
	if err != nil {
		return "", err
	}
	return filepath.Join(root, providerPart, ownerPart, repositoryPart, versionPart, variantPart, targetPart), nil
}

// UsageLogPath returns the namespaced per-app JSONL usage log path.
func UsageLogPath(repo string) (string, error) {
	return UsageLogPathFor(DefaultProvider, repo)
}

// UsageLogPathFor returns the namespaced per-app JSONL usage log path for a
// provider and full repository identity.
func UsageLogPathFor(provider, repo string) (string, error) {
	dataHome, err := dataHome()
	if err != nil {
		return "", err
	}
	providerPart, ownerPart, repositoryPart, err := usageParts(provider, repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataHome, "zenget", "usage", providerPart, ownerPart, repositoryPart+".jsonl"), nil
}

// LegacyUsageLogPath returns the pre-namespaced usage log path. It is used as
// a read fallback for wrappers and state entries created by older releases.
func LegacyUsageLogPath(repo string) (string, error) {
	dataHome, err := dataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataHome, "zenget", "usage", path.Base(repo)+".jsonl"), nil
}

// UsageLogPaths returns the current namespaced path followed by the legacy
// path when it differs. Callers can aggregate both to preserve usage history
// while new wrappers write only to the namespaced location.
func UsageLogPaths(repo string) ([]string, error) {
	current, err := UsageLogPath(repo)
	if err != nil {
		return nil, err
	}
	legacy, err := LegacyUsageLogPath(repo)
	if err != nil {
		return nil, err
	}
	if current == legacy {
		return []string{current}, nil
	}
	return []string{current, legacy}, nil
}

// TargetPath returns the user-facing wrapper path inside installDir.
// installDir must already be expanded to an absolute path.
func TargetPath(repo, installDir string) (string, error) {
	return TargetPathFor(repo, path.Base(repo), installDir)
}

// TargetPathFor returns the user-facing wrapper path for repo and targetName.
// targetName must be a single path component. This keeps a custom executable
// name inside the configured install directory.
func TargetPathFor(repo, targetName, installDir string) (string, error) {
	if installDir == "" {
		return "", fmt.Errorf("install directory is empty")
	}
	if targetName == "" {
		targetName = path.Base(repo)
	}
	if targetName == "." || targetName == ".." ||
		filepath.Base(targetName) != targetName || strings.ContainsAny(targetName, "/\\") || strings.ContainsRune(targetName, '\x00') {
		return "", fmt.Errorf("invalid target name %q", targetName)
	}
	return filepath.Join(installDir, targetName), nil
}

// Generate returns the contents of a POSIX shell wrapper for repo.
func Generate(repo, realBin, usageLog string) []byte {
	script := fmt.Sprintf(`#!/bin/sh
# %s for %s
real_bin=%q
usage_log=%q
config_file="${XDG_CONFIG_HOME:-$HOME/.config}/zenget/config.json"

is_tracking_enabled() {
	[ -f "$config_file" ] || return 1
	grep -q '"usage_tracking"[[:space:]]*:[[:space:]]*true' "$config_file"
}

if ! is_tracking_enabled; then
	exec "$real_bin" "$@"
fi

start=$(date -u +"%%Y-%%m-%%dT%%H:%%M:%%SZ")
start_epoch=$(date +%%s)
"$real_bin" "$@"
code=$?
end_epoch=$(date +%%s)
duration=$((end_epoch - start_epoch))

mkdir -p "$(dirname "$usage_log")"
printf '{"start":"%%s","duration_s":%%s,"exit_code":%%s}\n' "$start" "$duration" "$code" >> "$usage_log"
(zenget flush >/dev/null 2>&1) || true
exit $code
`, Marker, repo, realBin, usageLog)
	return []byte(script)
}

// GenerateShim returns a POSIX shell wrapper that invokes the absolute
// managed dispatcher runtime with the normalized target name. Usage tracking
// is handled by the dispatcher, so the wrapper only forwards the invocation.
func GenerateShim(repo, runtimePath, targetName string) []byte {
	if targetName == "" {
		targetName = path.Base(repo)
	}
	script := fmt.Sprintf(`#!/bin/sh
# %s for %s
shim_runtime=%q
shim_target=%q
exec "$shim_runtime" shim "$shim_target" "$@"
`, Marker, repo, runtimePath, targetName)
	return []byte(script)
}

func dataHome() (string, error) {
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return dataHome, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}

	return filepath.Join(home, ".local", "share"), nil
}

func storageParts(provider, repo, version, targetName string) (string, string, string, string, string, error) {
	providerPart, err := encodeStorageComponent(provider, "provider")
	if err != nil {
		return "", "", "", "", "", err
	}
	owner, repository, err := splitRepository(repo)
	if err != nil {
		return "", "", "", "", "", err
	}
	ownerPart, err := encodeStorageComponent(owner, "repository owner")
	if err != nil {
		return "", "", "", "", "", err
	}
	repositoryPart, err := encodeStorageComponent(repository, "repository name")
	if err != nil {
		return "", "", "", "", "", err
	}
	versionPart, err := encodeStorageComponent(version, "release version")
	if err != nil {
		return "", "", "", "", "", err
	}
	if targetName == "" || targetName == "." || targetName == ".." ||
		strings.ContainsAny(targetName, "/\\") || strings.ContainsRune(targetName, '\x00') {
		return "", "", "", "", "", fmt.Errorf("invalid target name %q", targetName)
	}
	targetPart, err := encodeStorageComponent(targetName, "target name")
	if err != nil {
		return "", "", "", "", "", err
	}
	return providerPart, ownerPart, repositoryPart, versionPart, targetPart, nil
}

func usageParts(provider, repo string) (string, string, string, error) {
	providerPart, err := encodeStorageComponent(provider, "provider")
	if err != nil {
		return "", "", "", err
	}
	owner, repository, err := splitRepository(repo)
	if err != nil {
		return "", "", "", err
	}
	ownerPart, err := encodeStorageComponent(owner, "repository owner")
	if err != nil {
		return "", "", "", err
	}
	repositoryPart, err := encodeStorageComponent(repository, "repository name")
	if err != nil {
		return "", "", "", err
	}
	return providerPart, ownerPart, repositoryPart, nil
}

func splitRepository(repo string) (string, string, error) {
	if strings.Count(repo, "/") != 1 {
		return "", "", fmt.Errorf("invalid repository %q: expected owner/name", repo)
	}
	owner, repository, _ := strings.Cut(repo, "/")
	if owner == "" || repository == "" || owner == "." || owner == ".." || repository == "." || repository == ".." ||
		strings.ContainsRune(owner, '\x00') || strings.ContainsRune(repository, '\x00') ||
		strings.ContainsRune(owner, '\\') || strings.ContainsRune(repository, '\\') {
		return "", "", fmt.Errorf("invalid repository %q: expected safe owner/name", repo)
	}
	return owner, repository, nil
}

func encodeStorageComponent(value, label string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("invalid %s %q", label, value)
	}

	var encoded strings.Builder
	for i := 0; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			encoded.WriteByte(character)
			continue
		}
		fmt.Fprintf(&encoded, "%%%02X", character)
	}
	if encoded.String() == "." || encoded.String() == ".." {
		return "", fmt.Errorf("invalid %s %q", label, value)
	}
	return encoded.String(), nil
}

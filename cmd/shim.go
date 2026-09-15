package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/artifactcache"
	"zenget/internal/config"
	"zenget/internal/hashx"
	"zenget/internal/projectmanifest"
	"zenget/internal/state"
	"zenget/internal/upload"
	"zenget/internal/usage"
	"zenget/internal/wrapper"
)

// SelectionSource identifies whether a target came from a project manifest or
// the globally active state registry.
type SelectionSource string

const (
	selectionProject SelectionSource = "project"
	selectionGlobal  SelectionSource = "global"
)

// TargetSelection describes the side-effect-free result of resolving one
// target. ArtifactPath is populated for a valid selection and for known
// damaged cache entries; a missing project entry may not have a deterministic
// path because the cache variant can include asset metadata absent from a
// manifest.
type TargetSelection struct {
	Target         string                        `json:"target"`
	ManifestPath   string                        `json:"manifest,omitempty"`
	Source         SelectionSource               `json:"source"`
	Provider       string                        `json:"provider"`
	Repository     string                        `json:"repository"`
	Tag            string                        `json:"tag"`
	ArtifactPath   string                        `json:"artifact_path,omitempty"`
	Integrity      artifactcache.IntegrityStatus `json:"integrity"`
	CacheIntegrity artifactcache.IntegrityStatus `json:"cache_integrity"`
	Reason         string                        `json:"reason,omitempty"`
	entry          *artifactcache.Entry
}

// TargetResolveOptions controls target resolution. An empty working directory
// uses the process cwd. The environment opt-out is always evaluated by the
// shared project manifest resolver.
type TargetResolveOptions struct {
	WorkingDirectory    string
	NoManifestDiscovery bool
}

var shimCmd = &cobra.Command{
	Use:    "shim <target> [args...]",
	Short:  "Dispatch an installed target through the managed runtime",
	Hidden: true,
	Args:   cobra.MinimumNArgs(1),
	RunE:   runShim,
}

func init() {
	rootCmd.AddCommand(shimCmd)
}

func runShim(cmd *cobra.Command, args []string) error {
	return dispatchTarget(cmd.Context(), args[0], args[1:])
}

// ResolveTarget resolves target without downloading, installing, or touching
// cache usage timestamps.
func ResolveTarget(target string, options TargetResolveOptions) (TargetSelection, error) {
	normalized, err := normalizeTarget(target)
	if err != nil {
		return TargetSelection{}, err
	}

	resolution, err := projectmanifest.Resolve(projectmanifest.Options{
		NoDiscovery:      options.NoManifestDiscovery,
		WorkingDirectory: options.WorkingDirectory,
	})
	if err == nil {
		matches := matchingManifestApps(resolution.Manifest, normalized)
		switch len(matches) {
		case 0:
			// A valid project manifest without this target intentionally falls
			// back to the global active version.
		case 1:
			return resolveProjectTarget(resolution, matches[0], normalized)
		default:
			return TargetSelection{}, fmt.Errorf("target %q matches multiple entries in manifest %q; set unique target_name values", normalized, resolution.Path)
		}
	} else {
		var notFound *projectmanifest.NoManifestError
		if !errors.As(err, &notFound) {
			return TargetSelection{}, err
		}
	}

	return resolveGlobalTarget(normalized)
}

// ResolveTargetDefault resolves target from the process working directory.
func ResolveTargetDefault(target string) (TargetSelection, error) {
	return ResolveTarget(target, TargetResolveOptions{})
}

func matchingManifestApps(file state.Manifest, target string) []state.ManifestApp {
	var matches []state.ManifestApp
	for _, app := range file.Apps {
		if effectiveManifestTarget(app) == target {
			matches = append(matches, app)
		}
	}
	return matches
}

func effectiveManifestTarget(app state.ManifestApp) string {
	if app.TargetName != "" {
		return app.TargetName
	}
	return path.Base(app.Repository)
}

func resolveProjectTarget(resolution projectmanifest.Resolution, app state.ManifestApp, target string) (TargetSelection, error) {
	goos, goarch := app.PlatformOS, app.PlatformArch
	if goos == "" {
		goos, goarch = runtime.GOOS, runtime.GOARCH
	}
	key := artifactcache.Key{
		Provider: app.Provider, Repository: app.Repository, Tag: app.Tag,
		PlatformOS: goos, PlatformArch: goarch, PlatformLibc: libcIdentity(goos),
		AssetName: app.Asset, ArchiveBinary: app.ArchiveBinary, TargetName: target,
	}
	selection := TargetSelection{
		Target: target, ManifestPath: resolution.Path, Source: selectionProject,
		Provider: app.Provider, Repository: app.Repository, Tag: app.Tag,
		Integrity: artifactcache.IntegrityMissing, CacheIntegrity: artifactcache.IntegrityMissing,
	}

	cache, err := artifactcache.New()
	if err != nil {
		selection.Integrity = artifactcache.IntegrityCorrupt
		selection.CacheIntegrity = artifactcache.IntegrityCorrupt
		selection.Reason = fmt.Sprintf("open artifact cache: %v", err)
		return selection, nil
	}
	entry, _, hit, err := cache.LookupReadOnly(key, "")
	if err != nil {
		selection.Integrity = artifactcache.IntegrityCorrupt
		selection.CacheIntegrity = artifactcache.IntegrityCorrupt
		selection.Reason = err.Error()
		return selection, nil
	}
	if hit {
		selection.ArtifactPath = filepath.Join(cache.Root(), filepath.FromSlash(entry.RelativePath))
		selection.Integrity = artifactcache.IntegrityValid
		selection.CacheIntegrity = artifactcache.IntegrityValid
		selection.entry = entry
		if err := validateCacheArtifact(selection.ArtifactPath, *entry); err != nil {
			selection.Integrity = artifactcache.IntegrityCorrupt
			selection.CacheIntegrity = artifactcache.IntegrityCorrupt
			selection.Reason = err.Error()
		}
		return selection, nil
	}

	inspections, err := cache.List(artifactcache.Protection{})
	if err != nil {
		selection.Integrity = artifactcache.IntegrityCorrupt
		selection.CacheIntegrity = artifactcache.IntegrityCorrupt
		selection.Reason = err.Error()
		return selection, nil
	}
	var candidates []artifactcache.Inspection
	for _, inspection := range inspections {
		if cacheKeyMatches(key, inspection.Entry.Key()) {
			candidates = append(candidates, inspection)
		}
	}
	switch len(candidates) {
	case 0:
		selection.Reason = "no matching verified cache artifact"
	case 1:
		selection.ArtifactPath = filepath.Join(cache.Root(), filepath.FromSlash(candidates[0].Entry.RelativePath))
		selection.Integrity = candidates[0].Integrity
		selection.CacheIntegrity = candidates[0].Integrity
		selection.Reason = candidates[0].Reason
		entry := candidates[0].Entry
		selection.entry = &entry
	default:
		selection.Integrity = artifactcache.IntegrityCorrupt
		selection.CacheIntegrity = artifactcache.IntegrityCorrupt
		selection.Reason = fmt.Sprintf("multiple cache artifacts match %s", key.Identity())
	}
	return selection, nil
}

func resolveGlobalTarget(target string) (TargetSelection, error) {
	registry, err := state.Load()
	if err != nil {
		return TargetSelection{}, fmt.Errorf("load global state: %w", err)
	}
	names := make([]string, 0, len(registry.Apps))
	for name := range registry.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	var matches []globalTargetMatch
	for _, name := range names {
		app := registry.Apps[name]
		if globalTargetName(name, app) == target {
			matches = append(matches, globalTargetMatch{name: name, app: app})
		}
	}
	if len(matches) == 0 {
		return TargetSelection{}, fmt.Errorf("no installed target %q; run zenget install or zenget apply", target)
	}
	if len(matches) > 1 {
		return TargetSelection{}, fmt.Errorf("target %q matches multiple globally installed applications: %s", target, globalMatchNames(matches))
	}

	match := matches[0]
	provider := match.app.Provider
	if provider == "" {
		provider = wrapper.DefaultProvider
	}
	artifact := match.app.Active()
	if artifact == nil {
		return TargetSelection{}, fmt.Errorf("global application %q has no active artifact", match.name)
	}
	version := artifact.Version
	if version == "" {
		version = match.app.Version
	}
	goos, goarch := match.app.PlatformOS, match.app.PlatformArch
	if goos == "" || goarch == "" {
		goos, goarch = runtime.GOOS, runtime.GOARCH
	}
	integrity, reason := validateManagedArtifact(artifact.RealPath, artifact.XXH3_64)
	selection := TargetSelection{
		Target: target, Source: selectionGlobal, Provider: provider,
		Repository: match.name, Tag: version, ArtifactPath: artifact.RealPath,
		Integrity: integrity, CacheIntegrity: artifactcache.IntegrityMissing, Reason: reason,
	}
	selection.CacheIntegrity = globalCacheIntegrity(provider, match.name, version, goos, goarch, target)
	return selection, nil
}

type globalTargetMatch struct {
	name string
	app  state.App
}

func globalTargetName(repository string, app state.App) string {
	if app.InstallPath != "" {
		return filepath.Base(app.InstallPath)
	}
	return path.Base(repository)
}

func globalMatchNames(matches []globalTargetMatch) string {
	names := make([]string, len(matches))
	for index, match := range matches {
		names[index] = match.name
	}
	return strings.Join(names, ", ")
}

func globalCacheIntegrity(provider, repository, tag, goos, goarch, target string) artifactcache.IntegrityStatus {
	cache, err := artifactcache.New()
	if err != nil {
		return artifactcache.IntegrityCorrupt
	}
	key := artifactcache.Key{
		Provider: provider, Repository: repository, Tag: tag,
		PlatformOS: goos, PlatformArch: goarch, PlatformLibc: libcIdentity(goos), TargetName: target,
	}
	_, _, hit, err := cache.LookupReadOnly(key, "")
	if err != nil {
		return artifactcache.IntegrityCorrupt
	}
	if hit {
		return artifactcache.IntegrityValid
	}
	inspections, err := cache.List(artifactcache.Protection{})
	if err != nil {
		return artifactcache.IntegrityCorrupt
	}
	for _, inspection := range inspections {
		if cacheKeyMatches(key, inspection.Entry.Key()) {
			return inspection.Integrity
		}
	}
	return artifactcache.IntegrityMissing
}

func validateManagedArtifact(filePath, expectedHash string) (artifactcache.IntegrityStatus, string) {
	if filePath == "" || expectedHash == "" {
		return artifactcache.IntegrityCorrupt, "active artifact path or hash is missing"
	}
	managed, err := wrapper.IsManagedArtifactPath(filePath)
	if err != nil {
		return artifactcache.IntegrityCorrupt, err.Error()
	}
	if !managed {
		return artifactcache.IntegrityCorrupt, "active artifact is outside the zenget data directory"
	}
	info, err := os.Lstat(filePath)
	if os.IsNotExist(err) {
		return artifactcache.IntegrityMissing, "active artifact file is missing"
	}
	if err != nil {
		return artifactcache.IntegrityCorrupt, err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return artifactcache.IntegrityCorrupt, "active artifact is not a regular file"
	}
	actual, err := hashx.File(filePath)
	if err != nil {
		return artifactcache.IntegrityCorrupt, err.Error()
	}
	if !strings.EqualFold(actual, expectedHash) {
		return artifactcache.IntegrityCorrupt, "active artifact hash does not match state"
	}
	return artifactcache.IntegrityValid, ""
}

func validateCacheArtifact(filePath string, entry artifactcache.Entry) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return fmt.Errorf("validate cache artifact %q: %w", filePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("validate cache artifact %q: not a regular file", filePath)
	}
	if info.Size() != entry.BinarySize {
		return fmt.Errorf("validate cache artifact %q: size mismatch", filePath)
	}
	actual, err := hashx.File(filePath)
	if err != nil {
		return fmt.Errorf("hash cache artifact %q: %w", filePath, err)
	}
	if !strings.EqualFold(actual, entry.BinaryXXH3_64) {
		return fmt.Errorf("cache artifact %q: XXH3-64 mismatch", filePath)
	}
	return nil
}

func cacheKeyMatches(pattern, candidate artifactcache.Key) bool {
	if pattern.Provider != candidate.Provider || pattern.Repository != candidate.Repository || pattern.Tag != candidate.Tag ||
		pattern.PlatformOS != candidate.PlatformOS || pattern.PlatformArch != candidate.PlatformArch {
		return false
	}
	if pattern.PlatformLibc != "" && pattern.PlatformLibc != candidate.PlatformLibc {
		return false
	}
	if pattern.AssetID != 0 && pattern.AssetID != candidate.AssetID {
		return false
	}
	if pattern.AssetName != "" && pattern.AssetName != candidate.AssetName {
		return false
	}
	if pattern.ArchiveBinary != "" && pattern.ArchiveBinary != candidate.ArchiveBinary {
		return false
	}
	return pattern.TargetName == "" || pattern.TargetName == candidate.TargetName
}

func normalizeTarget(target string) (string, error) {
	if target == "" || target == "." || target == ".." || filepath.Base(target) != target || strings.ContainsAny(target, "/\\") || strings.ContainsRune(target, '\x00') {
		return "", fmt.Errorf("invalid target %q", target)
	}
	return target, nil
}

func libcIdentity(goos string) string {
	if goos != "linux" || runtime.GOOS != "linux" {
		return "unknown"
	}
	for _, pattern := range []string{
		"/lib/ld-musl-*.so.1",
		"/lib64/ld-musl-*.so.1",
		"/usr/lib/ld-musl-*.so.1",
	} {
		matches, err := filepath.Glob(pattern)
		if err == nil && len(matches) > 0 {
			return "musl"
		}
	}
	return "glibc"
}

// ShimExitError carries a child process exit status through Cobra's error
// path.
type ShimExitError struct {
	Code int
}

func (e *ShimExitError) Error() string {
	return fmt.Sprintf("managed artifact exited with status %d", e.Code)
}

// ExitCode implements the root command's typed command-error contract.
func (e *ShimExitError) ExitCode() int {
	if e == nil || e.Code < 0 {
		return 1
	}
	return e.Code
}

func dispatchTarget(ctx context.Context, target string, args []string) error {
	selection, err := ResolveTargetDefault(target)
	if err != nil {
		return err
	}
	if selection.Source == selectionProject && (selection.Integrity != artifactcache.IntegrityValid || selection.ArtifactPath == "") {
		cfg, configErr := config.Load()
		if configErr != nil {
			return fmt.Errorf("load config for project lazy install: %w", configErr)
		}
		if !cfg.LazyInstallEnabled() {
			return fmt.Errorf("cannot execute project target %q: %s; run zenget apply or enable lazy install with zenget config set lazy_install true", selection.Target, selectionReason(selection))
		}
		selection, err = lazyInstallProjectTarget(ctx, selection)
		if err != nil {
			return err
		}
	}
	if selection.Integrity != artifactcache.IntegrityValid || selection.ArtifactPath == "" {
		if selection.Source == selectionProject {
			return fmt.Errorf("cannot execute project target %q: %s; run zenget apply", selection.Target, selectionReason(selection))
		}
		return fmt.Errorf("cannot execute global target %q: %s; run zenget install or zenget apply", selection.Target, selectionReason(selection))
	}
	if selection.Source == selectionProject {
		// The read-only lookup verifies the file, and this second check closes
		// the gap between resolution and process start for ordinary tampering.
		if selection.entry != nil {
			if err := validateCacheArtifact(selection.ArtifactPath, *selection.entry); err != nil {
				return err
			}
		} else if err := validatePathIsRegular(selection.ArtifactPath); err != nil {
			return fmt.Errorf("validate selected project artifact: %w", err)
		}
	}
	return executeArtifact(ctx, selection, args)
}

func selectionReason(selection TargetSelection) string {
	if selection.Reason != "" {
		return selection.Reason
	}
	return string(selection.Integrity)
}

func validatePathIsRegular(filePath string) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", filePath)
	}
	return nil
}

func executeArtifact(ctx context.Context, selection TargetSelection, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	command := exec.CommandContext(ctx, selection.ArtifactPath, args...)
	command.Args = append([]string{selection.Target}, args...)
	command.Env = os.Environ()
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start selected artifact %q: %w", selection.ArtifactPath, err)
	}
	forwarded := make(chan os.Signal, 4)
	signal.Notify(forwarded, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT)
	done := make(chan struct{})
	go forwardSignals(command, forwarded, done)
	waitErr := command.Wait()
	close(done)
	signal.Stop(forwarded)
	code := processExitCode(command, waitErr)
	recordInvocation(selection, code, time.Since(started))
	if waitErr != nil || code != 0 {
		return &ShimExitError{Code: code}
	}
	return nil
}

func forwardSignals(command *exec.Cmd, signals <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case sig := <-signals:
			if command.Process != nil {
				_ = command.Process.Signal(sig)
			}
		case <-done:
			return
		}
	}
}

func processExitCode(command *exec.Cmd, waitErr error) int {
	if command.ProcessState != nil {
		if code := command.ProcessState.ExitCode(); code >= 0 {
			return code
		}
		if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
	}
	if waitErr != nil {
		return 1
	}
	return 0
}

func recordInvocation(selection TargetSelection, exitCode int, duration time.Duration) {
	cfg, err := config.Load()
	if err != nil || !cfg.TrackingEnabled() {
		return
	}
	logPath, err := wrapper.UsageLogPathFor(selection.Provider, selection.Repository)
	if err == nil {
		_ = usage.Record(logPath, selection.Repository, selection.Tag, time.Now().Add(-duration), duration, exitCode)
	}
	if cfg.UploadEnabled() {
		_ = upload.MaybeUploadNow(context.Background(), cfg)
	}
}

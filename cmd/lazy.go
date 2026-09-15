package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"zenget/internal/artifactcache"
	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/install"
	manifestfile "zenget/internal/manifest"
	"zenget/internal/projectmanifest"
	"zenget/internal/provider"
	"zenget/internal/verify"
)

// lazyProviderFactory keeps the runtime path injectable for deterministic
// integration tests while production always uses the normal GitHub client.
var lazyProviderFactory = func() (provider.Provider, error) {
	return github.New(), nil
}

func lazyInstallProjectTarget(ctx context.Context, selection TargetSelection) (TargetSelection, error) {
	if selection.Source != selectionProject {
		return TargetSelection{}, fmt.Errorf("lazy install requires a project target")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cfg, err := config.Load()
	if err != nil {
		return TargetSelection{}, fmt.Errorf("load config for lazy install: %w", err)
	}
	if !cfg.LazyInstallEnabled() {
		return TargetSelection{}, fmt.Errorf("lazy install is disabled; run zenget config set lazy_install true and activate the project")
	}

	resolution, err := projectmanifest.Resolve(projectmanifest.Options{
		ExplicitPath: selection.ManifestPath,
		NoDiscovery:  true,
	})
	if err != nil {
		return TargetSelection{}, fmt.Errorf("reload project manifest for lazy install: %w", err)
	}
	if resolution.Path != selection.ManifestPath {
		return TargetSelection{}, fmt.Errorf("project manifest path changed during lazy install")
	}
	if _, err := requireTrustedProject(resolution); err != nil {
		return TargetSelection{}, err
	}

	matches := matchingManifestApps(resolution.Manifest, selection.Target)
	if len(matches) != 1 {
		if len(matches) == 0 {
			return TargetSelection{}, fmt.Errorf("target %q is not present in trusted manifest %q", selection.Target, resolution.Path)
		}
		return TargetSelection{}, fmt.Errorf("target %q matches multiple entries in manifest %q; set unique target_name values", selection.Target, resolution.Path)
	}
	app := matches[0]
	if err := manifestfile.ValidateEntry(app); err != nil {
		return TargetSelection{}, fmt.Errorf("validate lazy manifest entry: %w", err)
	}
	if app.Provider != manifestfile.ProviderGitHub {
		return TargetSelection{}, fmt.Errorf("lazy install supports only the GitHub provider, got %q", app.Provider)
	}
	if effectiveManifestTarget(app) != selection.Target {
		return TargetSelection{}, fmt.Errorf("manifest target changed during lazy install")
	}
	if app.Tag == "" {
		return TargetSelection{}, fmt.Errorf("lazy manifest entry for %s has no exact tag", app.Repository)
	}

	goos, goarch := app.PlatformOS, app.PlatformArch
	if goos == "" {
		goos, goarch = runtime.GOOS, runtime.GOARCH
	}
	org, repo, ok := strings.Cut(app.Repository, "/")
	if !ok || org == "" || repo == "" {
		return TargetSelection{}, fmt.Errorf("invalid lazy manifest repository %q", app.Repository)
	}
	cache, err := artifactcache.New()
	if err != nil {
		return TargetSelection{}, fmt.Errorf("open artifact cache for lazy install: %w", err)
	}
	lookupKey := artifactcache.Key{
		Provider: manifestfile.ProviderGitHub, Repository: app.Repository, Tag: app.Tag,
		PlatformOS: goos, PlatformArch: goarch, PlatformLibc: libcIdentity(goos),
		AssetName: app.Asset, ArchiveBinary: app.ArchiveBinary, TargetName: selection.Target,
	}
	if result, hit, err := lazyCacheLookup(cache, lookupKey, "", selection); err != nil {
		return TargetSelection{}, err
	} else if hit {
		return result, nil
	}

	client, err := lazyProviderFactory()
	if err != nil {
		return TargetSelection{}, fmt.Errorf("configure GitHub provider for lazy install: %w", err)
	}
	if client == nil {
		return TargetSelection{}, fmt.Errorf("GitHub provider is nil")
	}
	if providerNameFor(client) != manifestfile.ProviderGitHub {
		return TargetSelection{}, fmt.Errorf("lazy install provider is %q; only github is supported", providerNameFor(client))
	}

	release, err := client.ReleaseByTag(ctx, org, repo, app.Tag)
	if err != nil {
		return TargetSelection{}, fmt.Errorf("fetch exact release for lazy install %s@%s: %w", app.Repository, app.Tag, err)
	}
	if release == nil || release.TagName != app.Tag {
		return TargetSelection{}, fmt.Errorf("provider returned tag %q for requested exact tag %q", releaseTag(release), app.Tag)
	}
	asset, err := resolveManifestAsset(release, app, goos, goarch)
	if err != nil {
		return TargetSelection{}, fmt.Errorf("resolve exact manifest asset: %w", err)
	}
	if asset.Name == "" {
		return TargetSelection{}, fmt.Errorf("lazy manifest asset resolved to an empty name")
	}

	key := lookupKey
	key.Provider = providerNameFor(client)
	key.AssetID = asset.ID
	key.AssetName = asset.Name
	if result, hit, err := lazyCacheLookup(cache, key, asset.Digest, selection); err != nil {
		return TargetSelection{}, err
	} else if hit {
		return result, nil
	}

	lock, err := cache.Acquire(ctx, key)
	if err != nil {
		return TargetSelection{}, fmt.Errorf("acquire lazy artifact lock: %w", err)
	}
	defer func() { _ = lock.Release() }()

	if result, hit, err := lazyCacheLookup(cache, key, asset.Digest, selection); err != nil {
		return TargetSelection{}, err
	} else if hit {
		return result, nil
	}

	limits := cfg.EffectiveLimits()
	if err := limits.Validate(); err != nil {
		return TargetSelection{}, fmt.Errorf("validate resource limits for lazy install: %w", err)
	}
	temporary, err := os.CreateTemp("", "zenget-lazy-asset-*")
	if err != nil {
		return TargetSelection{}, fmt.Errorf("create lazy asset staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Close(); err != nil {
		return TargetSelection{}, fmt.Errorf("close lazy asset staging file %q: %w", temporaryPath, err)
	}
	if err := client.DownloadAsset(ctx, asset, temporaryPath, limits.DownloadBytes); err != nil {
		return TargetSelection{}, fmt.Errorf("download exact lazy asset %q: %w", asset.Name, err)
	}
	policy, err := resolveInstallChecksumPolicy(installOptions{})
	if err != nil {
		return TargetSelection{}, err
	}
	if err := verify.VerifyWithPolicy(ctx, client, io.Discard, asset, release, temporaryPath, app.Repository, policy); err != nil {
		return TargetSelection{}, err
	}

	format := install.DetectFormat(asset.Name)
	archiveBinary, _, err := resolveArchiveBinaryWithRecipe(
		temporaryPath, format, repo, app.ArchiveBinary, "", "", false, nil,
	)
	if err != nil {
		return TargetSelection{}, fmt.Errorf("resolve lazy archive binary for %q: %w", asset.Name, err)
	}
	binary, err := install.ExtractBinaryByName(temporaryPath, format, archiveBinary)
	if err != nil {
		return TargetSelection{}, fmt.Errorf("extract lazy release asset %q: %w", asset.Name, err)
	}
	archiveSHA256, err := verify.SHA256File(temporaryPath)
	if err != nil {
		return TargetSelection{}, fmt.Errorf("hash lazy release asset %q: %w", asset.Name, err)
	}
	key.ArchiveBinary = archiveBinary
	entry, err := cache.PublishLocked(key, archiveSHA256, binary)
	if err != nil {
		return TargetSelection{}, fmt.Errorf("publish lazy artifact cache: %w", err)
	}
	removeTemporary = true
	result, hit, err := lazyCacheLookup(cache, entry.Key(), archiveSHA256, selection)
	if err != nil {
		return TargetSelection{}, err
	}
	if !hit {
		return TargetSelection{}, fmt.Errorf("published lazy artifact failed cache revalidation")
	}
	return result, nil
}

func lazyCacheLookup(cache *artifactcache.Cache, key artifactcache.Key, expectedArchiveSHA256 string, selection TargetSelection) (TargetSelection, bool, error) {
	entry, _, hit, err := cache.Lookup(key, expectedArchiveSHA256)
	if err != nil {
		return TargetSelection{}, false, fmt.Errorf("verify lazy artifact cache: %w", err)
	}
	if !hit {
		return TargetSelection{}, false, nil
	}
	if entry == nil || entry.RelativePath == "" {
		return TargetSelection{}, false, fmt.Errorf("lazy artifact cache returned an incomplete entry")
	}
	selection.ArtifactPath = filepathFromCache(cache, entry.RelativePath)
	selection.Integrity = artifactcache.IntegrityValid
	selection.CacheIntegrity = artifactcache.IntegrityValid
	selection.Reason = ""
	selection.entry = entry
	if err := validateCacheArtifact(selection.ArtifactPath, *entry); err != nil {
		return TargetSelection{}, false, fmt.Errorf("validate lazy artifact cache: %w", err)
	}
	return selection, true, nil
}

func filepathFromCache(cache *artifactcache.Cache, relative string) string {
	return filepath.Join(cache.Root(), filepath.FromSlash(relative))
}

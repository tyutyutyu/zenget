package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/artifactcache"
	"zenget/internal/github"
	"zenget/internal/install"
	"zenget/internal/lockfile"
	"zenget/internal/state"
	"zenget/internal/verify"
)

var lockForce bool

var lockCmd = &cobra.Command{
	Use:   "lock <manifest-file>",
	Short: "Resolve a manifest into an artifact lockfile",
	Args:  cobra.ExactArgs(1),
	RunE:  runLock,
}

func init() {
	lockCmd.Flags().StringP("output", "o", "", "Write the lockfile to a local file")
	lockCmd.Flags().BoolVar(&lockForce, "force", false, "Allow replacing an existing lockfile")
	rootCmd.AddCommand(lockCmd)
}

func runLock(cmd *cobra.Command, args []string) error {
	output, err := getOptionalStringFlag(cmd, "output")
	if err != nil {
		return fmt.Errorf("read --output flag: %w", err)
	}
	if output == "" {
		return fmt.Errorf("--output is required because lockfiles are written only to local files")
	}
	if !isLocalManifestPath(output) {
		return fmt.Errorf("lockfile output must be a local path")
	}
	manifest, err := loadManifest(args[0])
	if err != nil {
		return err
	}
	client, err := manifestClientFactory(manifestProvider(manifest))
	if err != nil {
		return err
	}
	locked, err := buildLockfile(cmd.Context(), client, manifest)
	if err != nil {
		return err
	}
	data, err := lockfile.Marshal(locked)
	if err != nil {
		return fmt.Errorf("encode lockfile: %w", err)
	}
	if err := lockfile.WriteFile(output, data, lockForce); err != nil {
		return err
	}
	writer := cmd.OutOrStdout()
	if writer == nil {
		writer = io.Discard
	}
	_, err = fmt.Fprintf(writer, "Wrote lockfile %s\n", output)
	return err
}

func buildLockfile(ctx context.Context, client *github.Client, manifest state.Manifest) (lockfile.Lockfile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		return lockfile.Lockfile{}, fmt.Errorf("GitHub client is nil")
	}
	locked := lockfile.Lockfile{
		SchemaVersion: lockfile.SchemaVersion,
		Apps:          make([]lockfile.Entry, 0, len(manifest.Apps)),
	}
	for _, manifestEntry := range manifest.Apps {
		if err := ctx.Err(); err != nil {
			return lockfile.Lockfile{}, fmt.Errorf("lock %s: %w", manifestEntry.Repository, err)
		}
		goos, goarch := manifestEntry.PlatformOS, manifestEntry.PlatformArch
		if goos == "" {
			goos, goarch = runtime.GOOS, runtime.GOARCH
		}
		release, err := client.ReleaseByTag(ctx, repositoryParts(manifestEntry.Repository), repositoryName(manifestEntry.Repository), manifestEntry.Tag)
		if err != nil {
			return lockfile.Lockfile{}, fmt.Errorf("resolve release %s %s: %w", manifestEntry.Repository, manifestEntry.Tag, err)
		}
		if release == nil || release.TagName != manifestEntry.Tag {
			return lockfile.Lockfile{}, fmt.Errorf("resolve release %s: provider returned tag %q for requested tag %q", manifestEntry.Repository, releaseTag(release), manifestEntry.Tag)
		}
		asset, err := resolveManifestAsset(release, manifestEntry, goos, goarch)
		if err != nil {
			return lockfile.Lockfile{}, fmt.Errorf("resolve asset %s %s: %w", manifestEntry.Repository, manifestEntry.Tag, err)
		}
		if asset.ID <= 0 {
			return lockfile.Lockfile{}, fmt.Errorf("lock %s: asset %q has no provider asset ID", manifestEntry.Repository, asset.Name)
		}
		fingerprint, err := lockfile.Fingerprint(manifestEntry)
		if err != nil {
			return lockfile.Lockfile{}, fmt.Errorf("fingerprint %s: %w", manifestEntry.Repository, err)
		}
		providerDigest, digest, err := normalizedProviderDigest(asset.Digest)
		if err != nil {
			return lockfile.Lockfile{}, fmt.Errorf("lock %s asset %q: %w", manifestEntry.Repository, asset.Name, err)
		}
		archiveSHA256, assetSize, err := lockArtifactDigest(ctx, client, release, asset, digest)
		if err != nil {
			return lockfile.Lockfile{}, fmt.Errorf("hash asset %s: %w", manifestEntry.Repository, err)
		}
		locked.Apps = append(locked.Apps, lockfile.Entry{
			Repository:           manifestEntry.Repository,
			Provider:             manifestEntry.Provider,
			SelectionFingerprint: fingerprint,
			Tag:                  manifestEntry.Tag,
			PlatformOS:           goos,
			PlatformArch:         goarch,
			PlatformLibc:         lockfile.UnknownLibc,
			AssetID:              asset.ID,
			AssetName:            asset.Name,
			AssetSize:            assetSize,
			ProviderDigest:       providerDigest,
			ArchiveSHA256:        archiveSHA256,
		})
	}
	return lockfile.Canonicalize(locked), nil
}

func normalizedProviderDigest(value string) (canonical, digest string, err error) {
	if value == "" {
		return "", "", nil
	}
	digest, err = verify.ParseSHA256Digest(value)
	if err != nil {
		return "", "", err
	}
	return "sha256:" + digest, digest, nil
}

func lockArtifactDigest(ctx context.Context, client *github.Client, release *github.Release, asset github.Asset, providerDigest string) (digest string, size int64, err error) {
	if providerDigest != "" && asset.Size > 0 {
		return providerDigest, asset.Size, nil
	}
	temporary, err := os.CreateTemp("", "zenget-lock-asset-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temporary artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Close(); err != nil {
		return "", 0, fmt.Errorf("close temporary artifact %q: %w", temporaryPath, err)
	}
	if err := client.DownloadAsset(ctx, asset, temporaryPath); err != nil {
		return "", 0, fmt.Errorf("download asset %q: %w", asset.Name, err)
	}
	info, err := os.Stat(temporaryPath)
	if err != nil {
		return "", 0, fmt.Errorf("stat temporary artifact %q: %w", temporaryPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("temporary artifact %q is not a regular file", temporaryPath)
	}
	size = info.Size()
	if asset.Size > 0 && size != asset.Size {
		return "", 0, fmt.Errorf("asset size drift: provider reports %d bytes, downloaded %d", asset.Size, size)
	}
	digest, err = verify.SHA256File(temporaryPath)
	if err != nil {
		return "", 0, fmt.Errorf("compute SHA-256: %w", err)
	}
	if providerDigest != "" && !strings.EqualFold(digest, providerDigest) {
		return "", 0, fmt.Errorf("provider digest mismatch: expected %s, got %s", providerDigest, digest)
	}
	if err := verify.Verify(ctx, client, io.Discard, asset, release, temporaryPath); err != nil {
		return "", 0, err
	}
	return digest, size, nil
}

func validateLockAgainstPlan(manifest state.Manifest, plan []applyPlanEntry, locked lockfile.Lockfile) error {
	if err := lockfile.Validate(locked); err != nil {
		return fmt.Errorf("validate lockfile: %w", err)
	}
	if len(locked.Apps) != len(manifest.Apps) || len(plan) != len(manifest.Apps) {
		return fmt.Errorf("lockfile entry count %d does not match manifest entry count %d", len(locked.Apps), len(manifest.Apps))
	}
	for index, planEntry := range plan {
		lockedEntry := locked.Apps[index]
		if lockedEntry.Repository != planEntry.manifest.Repository {
			return fmt.Errorf("lockfile repository mismatch at index %d: got %q, want %q", index, lockedEntry.Repository, planEntry.manifest.Repository)
		}
		if planEntry.status == applyFailed {
			return fmt.Errorf("lock preflight cannot resolve %s: %s", planEntry.manifest.Repository, planEntry.reason)
		}
		fingerprint, err := lockfile.Fingerprint(planEntry.manifest)
		if err != nil {
			return fmt.Errorf("fingerprint %s: %w", planEntry.manifest.Repository, err)
		}
		if lockedEntry.SelectionFingerprint != fingerprint {
			return fmt.Errorf("lock drift for %s: selection fingerprint differs", planEntry.manifest.Repository)
		}
		if lockedEntry.Provider != planEntry.manifest.Provider || lockedEntry.Tag != planEntry.manifest.Tag ||
			lockedEntry.PlatformOS != planEntry.goos || lockedEntry.PlatformArch != planEntry.goarch ||
			lockedEntry.PlatformLibc != lockfile.UnknownLibc {
			return fmt.Errorf("lock drift for %s: provider, tag, or platform differs", planEntry.manifest.Repository)
		}
		currentProviderDigest, _, err := normalizedProviderDigest(planEntry.asset.Digest)
		if err != nil {
			return fmt.Errorf("lock drift for %s: invalid provider digest: %w", planEntry.manifest.Repository, err)
		}
		if lockedEntry.AssetID != planEntry.asset.ID || lockedEntry.AssetName != planEntry.asset.Name ||
			lockedEntry.AssetSize != planEntry.asset.Size || lockedEntry.ProviderDigest != currentProviderDigest {
			return fmt.Errorf("lock drift for %s: provider asset metadata differs", planEntry.manifest.Repository)
		}
	}
	return nil
}

func printLockedContentPending(out io.Writer, plan []applyPlanEntry) error {
	_, err := fmt.Fprintf(out, "Lock content verification: pending for %d artifact(s) (dry-run does not download artifact content)\n", countLockablePlanEntries(plan))
	return err
}

func countLockablePlanEntries(plan []applyPlanEntry) int {
	count := 0
	for _, entry := range plan {
		if entry.status != applyFailed {
			count++
		}
	}
	return count
}

func preflightLockedAssets(ctx context.Context, client *github.Client, plan []applyPlanEntry, locked lockfile.Lockfile) (map[string]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	staged := make(map[string]string, len(plan))
	cleanup := func() {
		cleanupStagedAssets(staged)
	}
	var failures []string
	for index := range plan {
		entry := plan[index]
		if entry.status == applyFailed {
			failures = append(failures, fmt.Sprintf("%s: %s", entry.manifest.Repository, entry.reason))
			continue
		}
		if index >= len(locked.Apps) {
			failures = append(failures, fmt.Sprintf("%s: lock entry is missing", entry.manifest.Repository))
			continue
		}
		lockedEntry := locked.Apps[index]
		cache, cacheErr := artifactcache.New()
		if cacheErr == nil {
			cacheKey := artifactcache.Key{
				Provider: providerNameFor(client), Repository: entry.manifest.Repository, Tag: entry.manifest.Tag,
				PlatformOS: entry.goos, PlatformArch: entry.goarch, PlatformLibc: install.LibcIdentity(entry.goos),
				AssetID: entry.asset.ID, AssetName: entry.asset.Name,
				ArchiveBinary: entry.manifest.ArchiveBinary, TargetName: entry.manifest.TargetName,
			}
			if _, _, hit, lookupErr := cache.Lookup(cacheKey, lockedEntry.ArchiveSHA256); lookupErr == nil && hit {
				// The lock's archive digest is the cache identity check. The
				// execution phase performs the same lookup before deciding whether
				// it needs this staged path.
				continue
			}
		}
		temporary, err := os.CreateTemp("", "zenget-apply-lock-*")
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: create staging file: %v", entry.manifest.Repository, err))
			continue
		}
		temporaryPath := temporary.Name()
		if err := temporary.Close(); err != nil {
			_ = os.Remove(temporaryPath)
			failures = append(failures, fmt.Sprintf("%s: close staging file: %v", entry.manifest.Repository, err))
			continue
		}
		if err := client.DownloadAsset(ctx, entry.asset, temporaryPath); err != nil {
			_ = os.Remove(temporaryPath)
			failures = append(failures, fmt.Sprintf("%s: download artifact: %v", entry.manifest.Repository, err))
			continue
		}
		info, err := os.Stat(temporaryPath)
		if err != nil {
			_ = os.Remove(temporaryPath)
			failures = append(failures, fmt.Sprintf("%s: stat staged artifact: %v", entry.manifest.Repository, err))
			continue
		}
		if info.Size() != lockedEntry.AssetSize {
			_ = os.Remove(temporaryPath)
			failures = append(failures, fmt.Sprintf("%s: artifact size mismatch: expected %d, got %d", entry.manifest.Repository, lockedEntry.AssetSize, info.Size()))
			continue
		}
		actualDigest, err := verify.SHA256File(temporaryPath)
		if err != nil {
			_ = os.Remove(temporaryPath)
			failures = append(failures, fmt.Sprintf("%s: compute artifact SHA-256: %v", entry.manifest.Repository, err))
			continue
		}
		if !strings.EqualFold(actualDigest, lockedEntry.ArchiveSHA256) {
			_ = os.Remove(temporaryPath)
			failures = append(failures, fmt.Sprintf("%s: artifact SHA-256 mismatch: expected %s, got %s", entry.manifest.Repository, lockedEntry.ArchiveSHA256, actualDigest))
			continue
		}
		if err := verify.Verify(ctx, client, io.Discard, entry.asset, entry.release, temporaryPath); err != nil {
			_ = os.Remove(temporaryPath)
			failures = append(failures, fmt.Sprintf("%s: checksum verification: %v", entry.manifest.Repository, err))
			continue
		}
		staged[entry.manifest.Repository] = temporaryPath
	}
	if len(failures) > 0 {
		cleanup()
		return nil, fmt.Errorf("lock artifact preflight failed: %s", strings.Join(failures, "; "))
	}
	return staged, nil
}

func cleanupStagedAssets(staged map[string]string) {
	for _, path := range staged {
		_ = os.Remove(path)
	}
}

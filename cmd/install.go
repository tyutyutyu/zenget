package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zenget/internal/appcfg"
	"zenget/internal/artifactcache"
	"zenget/internal/config"
	"zenget/internal/github"
	"zenget/internal/hashx"
	"zenget/internal/install"
	"zenget/internal/provider"
	"zenget/internal/recipe"
	"zenget/internal/registry"
	"zenget/internal/state"
	"zenget/internal/verify"
	"zenget/internal/wizard"
	"zenget/internal/wrapper"
)

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

var installCmd = &cobra.Command{
	Use:     "install <org/repo>",
	Aliases: []string{"i"},
	Short:   "Install the latest release of a GitHub repository",
	Long:    "Install a release using normal provider resolution, an explicit local --recipe, or exactly one synchronized --registry snapshot. A plain install consults synchronized registry snapshots automatically unless --no-registry is given and silently falls back to heuristic resolution on a registry miss. --recipe and --registry are mutually exclusive; --offline permits only a policy-authorized registry snapshot and verified local artifact cache.",
	Args:    cobra.ExactArgs(1),
	RunE:    runInstall,
}

func init() {
	rootCmd.AddCommand(installCmd)
	installCmd.Flags().String("asset", "", "Explicitly select a release asset by exact name or substring")
	installCmd.Flags().String("asset-match", "", "Select assets whose names contain this reusable case-insensitive substring")
	installCmd.Flags().String("asset-match-regex", "", "Select assets whose names match this reusable RE2 regular expression")
	installCmd.Flags().String("bin-name", "", "Explicitly select an executable inside an archive")
	installCmd.Flags().String("name", "", "Name of the installed wrapper and command")
	installCmd.Flags().String("tag", "", "Install a specific release tag; falls back to the most recent tag containing the string")
	installCmd.Flags().String("release-filter", "", "Only consider release tags matching this path.Match glob")
	installCmd.Flags().String("system", "", "Target platform as os/arch (e.g. linux/amd64, darwin/arm64); the binary is still installed locally and may not run on this host")
	installCmd.Flags().Int("min-age-days", 0, "Only install releases at least N days old; cannot be combined with --tag")
	installCmd.Flags().String("api-base-url", "", "Release API base URL (e.g. GitHub Enterprise /api/v3, Forgejo /api/v1, GitLab /api/v4)")
	installCmd.Flags().String("forge", "", "Release forge: github, gitlab, or forgejo; auto-detected from API host/path when omitted")
	installCmd.Flags().String("recipe", "", "Apply an explicit local schema version 1 install recipe JSON file")
	installCmd.Flags().String("registry", "", "Apply one synchronized named remote recipe registry snapshot")
	installCmd.Flags().Bool("offline", false, "Do not make network requests; use only a verified registry snapshot and artifact cache")
	installCmd.Flags().Bool("no-registry", false, "Do not consult synchronized registry snapshots during a plain install")
	installCmd.Flags().String("checksum-policy", "", "Checksum policy: if-present (default) or required; there is no off mode")
}

func runInstall(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || !repoPattern.MatchString(args[0]) {
		return fmt.Errorf("invalid repository %q: expected org/repo", firstArg(args))
	}

	assetOverride, err := cmd.Flags().GetString("asset")
	if err != nil {
		return fmt.Errorf("read --asset flag: %w", err)
	}
	assetMatch, err := getOptionalStringFlag(cmd, "asset-match")
	if err != nil {
		return fmt.Errorf("read --asset-match flag: %w", err)
	}
	assetMatchRegex, err := getOptionalStringFlag(cmd, "asset-match-regex")
	if err != nil {
		return fmt.Errorf("read --asset-match-regex flag: %w", err)
	}
	binName, err := getOptionalStringFlag(cmd, "bin-name")
	if err != nil {
		return fmt.Errorf("read --bin-name flag: %w", err)
	}
	targetName, err := getOptionalStringFlag(cmd, "name")
	if err != nil {
		return fmt.Errorf("read --name flag: %w", err)
	}

	tag, err := cmd.Flags().GetString("tag")
	if err != nil {
		return fmt.Errorf("read --tag flag: %w", err)
	}
	releaseFilter, err := getOptionalStringFlag(cmd, "release-filter")
	if err != nil {
		return fmt.Errorf("read --release-filter flag: %w", err)
	}
	releaseFilterSet := flagChanged(cmd, "release-filter")
	if err := validateReleaseFilter(releaseFilter); err != nil {
		return err
	}

	minAgeDays := 0
	minAgeFlag := cmd.Flags().Lookup("min-age-days")
	if minAgeFlag != nil {
		minAgeDays, err = cmd.Flags().GetInt("min-age-days")
		if err != nil {
			return fmt.Errorf("read --min-age-days flag: %w", err)
		}
		if err := validateReleaseSelection(tag, minAgeDays, cmd.Flags().Changed("min-age-days")); err != nil {
			return err
		}
	}

	system, err := cmd.Flags().GetString("system")
	if err != nil {
		return fmt.Errorf("read --system flag: %w", err)
	}
	goos, goarch, err := parseSystem(system)
	if err != nil {
		return err
	}
	checksumPolicy, err := getOptionalStringFlag(cmd, "checksum-policy")
	if err != nil {
		return fmt.Errorf("read --checksum-policy flag: %w", err)
	}
	checksumPolicySet := flagChanged(cmd, "checksum-policy")
	if checksumPolicySet {
		if _, err := config.ParseChecksumPolicy(checksumPolicy); err != nil {
			return fmt.Errorf("invalid value %q for --checksum-policy: %w", checksumPolicy, err)
		}
	}
	if _, err := assetSelectorFromInstallOptions(installOptions{
		assetOverride:      assetOverride,
		assetOverrideSet:   flagChanged(cmd, "asset"),
		assetMatch:         assetMatch,
		assetMatchSet:      flagChanged(cmd, "asset-match"),
		assetMatchRegex:    assetMatchRegex,
		assetMatchRegexSet: flagChanged(cmd, "asset-match-regex"),
	}); err != nil {
		return err
	}
	recipePath, err := getOptionalStringFlag(cmd, "recipe")
	if err != nil {
		return fmt.Errorf("read --recipe flag: %w", err)
	}
	registryName, err := getOptionalStringFlag(cmd, "registry")
	if err != nil {
		return fmt.Errorf("read --registry flag: %w", err)
	}
	registrySet := flagChanged(cmd, "registry")
	recipeSet := flagChanged(cmd, "recipe")
	if registrySet && recipeSet {
		return fmt.Errorf("--recipe and --registry are mutually exclusive")
	}
	offline, err := getOptionalBoolFlag(cmd, "offline")
	if err != nil {
		return fmt.Errorf("read --offline flag: %w", err)
	}
	if offline && !registrySet {
		return fmt.Errorf("--offline requires --registry; local --recipe is not an offline snapshot")
	}
	noRegistry, err := getOptionalBoolFlag(cmd, "no-registry")
	if err != nil {
		return fmt.Errorf("read --no-registry flag: %w", err)
	}
	var installRecipe *recipe.Recipe
	var autoRegistryName string
	var autoRegistrySource registry.Source
	if registrySet {
		installRecipe, err = loadRegistryInstallRecipe(registryName, args[0])
	} else {
		installRecipe, err = loadInstallRecipe(recipePath, recipeSet)
		if err == nil && installRecipe == nil && !noRegistry {
			installRecipe, autoRegistryName, autoRegistrySource, _ = resolveAutomaticRegistryRecipe(args[0])
		}
	}
	if err != nil {
		return err
	}
	if _, err := prepareInstallRecipe(installOptions{
		goos:          goos,
		goarch:        goarch,
		installRecipe: installRecipe,
	}, args[0]); err != nil {
		return err
	}

	client, err := newCommandProvider(cmd)
	if err != nil {
		return err
	}
	return runInstallWithOptions(cmd.Context(), client, args[0], installOptions{
		assetOverride:      assetOverride,
		assetOverrideSet:   flagChanged(cmd, "asset"),
		assetMatch:         assetMatch,
		assetMatchSet:      flagChanged(cmd, "asset-match"),
		assetMatchRegex:    assetMatchRegex,
		assetMatchRegexSet: flagChanged(cmd, "asset-match-regex"),
		checksumPolicy:     checksumPolicy,
		checksumPolicySet:  checksumPolicySet,
		installRecipe:      installRecipe,
		registryName:       registryName,
		registryNameSet:    registrySet,
		registryAutoName:   autoRegistryName,
		registryAutoSource: autoRegistrySource,
		offline:            offline,
		noRegistry:         noRegistry,
		binName:            binName,
		targetName:         targetName,
		tag:                tag,
		releaseFilter:      releaseFilter,
		releaseFilterSet:   releaseFilterSet,
		goos:               goos,
		goarch:             goarch,
		minAgeDays:         minAgeDays,
		input:              cmd.InOrStdin(),
		interactive:        stdinIsTerminal(),
	}, cmd.OutOrStdout())
}

func getOptionalStringFlag(cmd *cobra.Command, name string) (string, error) {
	if cmd == nil || cmd.Flags().Lookup(name) == nil {
		return "", nil
	}
	return cmd.Flags().GetString(name)
}

func getOptionalBoolFlag(cmd *cobra.Command, name string) (bool, error) {
	if cmd == nil || cmd.Flags().Lookup(name) == nil {
		return false, nil
	}
	return cmd.Flags().GetBool(name)
}

func loadInstallRecipe(path string, provided bool) (*recipe.Recipe, error) {
	if !provided && path == "" {
		return nil, nil
	}
	if path == "" {
		return nil, fmt.Errorf("--recipe must name a local schema version 1 JSON file")
	}
	loaded, err := recipe.Load(path)
	if err != nil {
		return nil, err
	}
	return &loaded, nil
}

func resolveInstallChecksumPolicy(options installOptions) (config.ChecksumPolicy, error) {
	if options.checksumPolicySet || options.checksumPolicy != "" {
		policy, err := config.ParseChecksumPolicy(options.checksumPolicy)
		if err != nil {
			return "", fmt.Errorf("invalid checksum policy %q: %w", options.checksumPolicy, err)
		}
		return policy, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("load config for checksum policy: %w", err)
	}
	return cfg.EffectiveChecksumPolicy(), nil
}

func flagChanged(cmd *cobra.Command, name string) bool {
	return cmd != nil && cmd.Flags().Lookup(name) != nil && cmd.Flags().Changed(name)
}

var stdinIsTerminal = func() bool {
	if os.Stdin == nil {
		return false
	}
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

var persistState = func(st *state.State) error {
	return st.Save()
}

// parseSystem validates a --system value and returns the os and arch parts.
// An empty value falls back to the host platform. Malformed values and
// unsupported os/arch values return an error before any network call.
func parseSystem(system string) (goos, goarch string, err error) {
	if system == "" {
		return runtime.GOOS, runtime.GOARCH, nil
	}

	goos, goarch, ok := strings.Cut(system, "/")
	if !ok || goos == "" || goarch == "" || strings.Contains(goarch, "/") {
		return "", "", fmt.Errorf("invalid --system %q: expected os/arch", system)
	}

	if !install.ValidPlatform(goos, goarch) {
		supportedOS, supportedArch := install.SupportedPlatforms()
		return "", "", fmt.Errorf(
			"unsupported --system %q: supported os values are %s; supported arch values are %s",
			system,
			strings.Join(supportedOS, ", "),
			strings.Join(supportedArch, ", "),
		)
	}

	return goos, goarch, nil
}

// runInstallWithClient performs an install using client and writes user-facing
// status messages to out. Keeping the client injectable makes the command
// flow testable without contacting the real GitHub API. When goos or goarch are
// empty they fall back to the host platform.
func runInstallWithClient(ctx context.Context, client provider.Provider, repository, assetOverride, tag, goos, goarch string, out io.Writer) error {
	return runInstallWithOptions(ctx, client, repository, installOptions{
		assetOverride: assetOverride,
		tag:           tag,
		goos:          goos,
		goarch:        goarch,
		input:         os.Stdin,
		interactive:   stdinIsTerminal(),
	}, out)
}

// runInstallWithMinAge performs an install and optionally restricts release
// selection to releases that have been published for at least minAgeDays.
// A zero value keeps the default latest-release behavior for callers that do
// not use the command flag.
func runInstallWithMinAge(ctx context.Context, client provider.Provider, repository, assetOverride, tag, goos, goarch string, minAgeDays int, out io.Writer) error {
	return runInstallWithOptions(ctx, client, repository, installOptions{
		assetOverride: assetOverride,
		tag:           tag,
		goos:          goos,
		goarch:        goarch,
		minAgeDays:    minAgeDays,
		input:         os.Stdin,
		interactive:   stdinIsTerminal(),
	}, out)
}

type installOptions struct {
	assetOverride      string
	assetOverrideSet   bool
	assetMatch         string
	assetMatchSet      bool
	assetMatchRegex    string
	assetMatchRegexSet bool
	checksumPolicy     string
	checksumPolicySet  bool
	recipePath         string
	recipePathSet      bool
	installRecipe      *recipe.Recipe
	registryName       string
	registryNameSet    bool
	registryAutoName   string
	registryAutoSource registry.Source
	offline            bool
	noRegistry         bool
	assetPath          string
	assetVerified      bool
	binName            string
	targetName         string
	tag                string
	releaseFilter      string
	releaseFilterSet   bool
	release            *github.Release
	goos               string
	goarch             string
	minAgeDays         int
	input              io.Reader
	interactive        bool
	prompter           *wizard.Session
	force              bool
	exactTag           bool
}

func runInstallWithOptions(ctx context.Context, client provider.Provider, repository string, options installOptions, out io.Writer) error {
	if !repoPattern.MatchString(repository) {
		return fmt.Errorf("invalid repository %q: expected org/repo", repository)
	}
	if options.goos == "" {
		options.goos = runtime.GOOS
	}
	if options.goarch == "" {
		options.goarch = runtime.GOARCH
	}
	if options.offline && options.registryName == "" {
		return fmt.Errorf("--offline requires a named registry snapshot")
	}
	if options.registryName != "" && options.recipePathSet {
		return fmt.Errorf("--recipe and --registry are mutually exclusive")
	}
	checksumPolicy, err := resolveInstallChecksumPolicy(options)
	if err != nil {
		return err
	}
	if options.registryName != "" && options.installRecipe == nil {
		options.installRecipe, err = loadRegistryInstallRecipe(options.registryName, repository)
		if err != nil {
			return err
		}
	}
	if options.installRecipe == nil && options.registryName == "" && !options.recipePathSet && options.recipePath == "" && !options.noRegistry {
		options.installRecipe, options.registryAutoName, options.registryAutoSource, _ = resolveAutomaticRegistryRecipe(repository)
	}
	installRecipe, err := prepareInstallRecipe(options, repository)
	if err != nil {
		return err
	}
	options.installRecipe = installRecipe
	explicitSelector, err := assetSelectorFromInstallOptions(options)
	if err != nil {
		return err
	}
	if err := validateReleaseSelection(options.tag, options.minAgeDays, false); err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("GitHub client is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	if installRecipe != nil && options.registryAutoName != "" {
		if _, err := fmt.Fprintf(out, "Using recipe for %s from registry %s (%s)\n", repository, options.registryAutoName, options.registryAutoSource.String()); err != nil {
			return err
		}
	}
	if options.input == nil {
		options.input = os.Stdin
	}
	if options.interactive && options.prompter == nil {
		options.prompter = wizard.New(options.input, out)
	}
	if options.assetVerified && options.assetPath == "" {
		return fmt.Errorf("verified release asset path is missing")
	}
	if options.offline && options.release == nil {
		options.release, err = resolveOfflineRelease(repository, options)
		if err != nil {
			return err
		}
	}

	org, repo, _ := strings.Cut(repository, "/")

	st, err := state.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	previous, previouslyInstalled := st.Get(repository)
	if !options.releaseFilterSet {
		options.releaseFilter = previous.ReleaseFilter
	}
	if err := validateReleaseFilter(options.releaseFilter); err != nil {
		return err
	}

	rel, ageFiltered, err := resolveInstallRelease(ctx, client, org, repo, options)
	if err != nil {
		return releaseResolutionError(repository, options, err)
	}
	if rel == nil || rel.TagName == "" {
		return releaseResolutionError(repository, options, fmt.Errorf("provider returned no release tag"))
	}

	if previouslyInstalled && previous.Version == rel.TagName {
		if !options.force {
			if options.releaseFilterSet && previous.ReleaseFilter != options.releaseFilter {
				previous.ReleaseFilter = options.releaseFilter
				st.Set(repository, previous)
				if err := persistState(st); err != nil {
					return fmt.Errorf("save release filter: %w", err)
				}
			}
			_, err = fmt.Fprintf(out, "%s %s is already installed\n", repo, rel.TagName)
			return err
		}
	}
	if previouslyInstalled && previous.Pinned && options.tag == "" && !options.force {
		if options.releaseFilterSet && previous.ReleaseFilter != options.releaseFilter {
			previous.ReleaseFilter = options.releaseFilter
			st.Set(repository, previous)
			if err := persistState(st); err != nil {
				return fmt.Errorf("save release filter: %w", err)
			}
		}
		_, err = fmt.Fprintf(out, "%s %s is pinned; run unpin %s or use --tag to install explicitly\n", repo, previous.Version, repository)
		return err
	}
	applicationConfig, err := appcfg.Load(repository)
	if err != nil {
		return fmt.Errorf("load app config: %w", err)
	}
	asset, selectedAssetName, assetWasPrompted, err := resolveInstallAssetWithSelectors(
		rel,
		options.goos,
		options.goarch,
		options.assetOverride,
		explicitSelector,
		applicationConfig.Asset,
		applicationConfig.AssetSelector,
		installRecipe,
		options.interactive,
		options.prompter,
	)
	if err != nil {
		return err
	}

	providerName := providerNameFor(client)
	var recipeChecksumAsset *github.Asset
	if installRecipe != nil && installRecipe.Checksum != nil {
		checksumAsset, checksumErr := resolveRecipeChecksumAsset(rel, *installRecipe.Checksum)
		if checksumErr != nil {
			return checksumErr
		}
		recipeChecksumAsset = &checksumAsset
	}
	var cache *artifactcache.Cache
	var cacheErr error
	if installRecipe == nil || installRecipe.Checksum == nil {
		if checksumPolicy == config.ChecksumPolicyIfPresent {
			cache, cacheErr = artifactcache.New()
			if cacheErr != nil {
				if _, err := fmt.Fprintf(out, "Warning: artifact cache unavailable: %v\n", cacheErr); err != nil {
					return err
				}
			}
		}
	}
	cacheKey := artifactcache.Key{
		Provider: providerName, Repository: repository, Tag: rel.TagName,
		PlatformOS: options.goos, PlatformArch: options.goarch, PlatformLibc: install.LibcIdentity(options.goos),
		AssetID: asset.ID, AssetName: asset.Name,
		ArchiveBinary: cacheArchiveBinaryHint(options, applicationConfig),
		TargetName:    cacheTargetNameHint(options, applicationConfig),
	}
	var cacheLock *artifactcache.Lock
	if cache != nil && recipeChecksumAsset == nil {
		cacheLock, cacheErr = cache.Acquire(ctx, cacheKey)
		if cacheErr != nil {
			if _, err := fmt.Fprintf(out, "Warning: artifact cache lock unavailable: %v\n", cacheErr); err != nil {
				return err
			}
			cache = nil
		}
	}
	if cacheLock != nil {
		defer func() { _ = cacheLock.Release() }()
	}

	var binary []byte
	var archiveBinary string
	var binaryWasPrompted bool
	cacheHit := false
	if cache != nil && recipeChecksumAsset == nil {
		cachedEntry, cachedBinary, hit, lookupErr := cache.Lookup(cacheKey, asset.Digest)
		if lookupErr != nil {
			if _, err := fmt.Fprintf(out, "Warning: artifact cache lookup failed: %v\n", lookupErr); err != nil {
				return err
			}
		} else if hit {
			archiveBinary = cachedEntry.ArchiveBinary
			binary = cachedBinary
			cacheHit = true
		}
	}

	var temporaryPath string
	if !cacheHit {
		if options.offline && (options.assetPath == "" || !options.assetVerified) {
			return fmt.Errorf("offline install requires a verified artifact cache hit or staged asset")
		}
		temporaryPath = options.assetPath
		removeTemporary := false
		if temporaryPath == "" {
			temporary, err := os.CreateTemp("", "zenget-asset-*")
			if err != nil {
				return fmt.Errorf("create temporary asset file: %w", err)
			}
			temporaryPath = temporary.Name()
			removeTemporary = true
			defer func() {
				if removeTemporary {
					_ = os.Remove(temporaryPath)
				}
			}()
			if err := temporary.Close(); err != nil {
				return fmt.Errorf("close temporary asset file %q: %w", temporaryPath, err)
			}

			if err := client.DownloadAsset(ctx, asset, temporaryPath); err != nil {
				return fmt.Errorf("download release asset %q: %w", asset.Name, err)
			}
		} else if options.assetVerified {
			if _, err := os.Stat(temporaryPath); err != nil {
				return fmt.Errorf("check staged release asset %q: %w", temporaryPath, err)
			}
		}

		if recipeChecksumAsset != nil {
			if err := verify.VerifyWithChecksumAsset(ctx, client, out, asset, *recipeChecksumAsset, verify.ChecksumFormat(installRecipe.Checksum.Format), temporaryPath); err != nil {
				return err
			}
		} else if !options.assetVerified {
			if err := verify.VerifyWithPolicy(ctx, client, out, asset, rel, temporaryPath, repository, checksumPolicy); err != nil {
				return err
			}
		}

		format := install.DetectFormat(asset.Name)
		archiveBinary, binaryWasPrompted, err = resolveArchiveBinaryWithRecipe(
			temporaryPath,
			format,
			repo,
			options.binName,
			applicationConfig.ArchiveBinary,
			recipeArchiveBinary(installRecipe),
			options.interactive,
			options.prompter,
		)
		if err != nil {
			return fmt.Errorf("extract release asset %q: %w", asset.Name, err)
		}

		binary, err = install.ExtractBinaryByName(temporaryPath, format, archiveBinary)
		if err != nil {
			return fmt.Errorf("extract release asset %q: %w", asset.Name, err)
		}
	}

	targetName, _, err := resolveTargetNameWithRecipe(
		repo,
		options.targetName,
		applicationConfig.TargetName,
		recipeTargetName(installRecipe),
		assetWasPrompted || binaryWasPrompted || (applicationConfig.TargetName != "" && !validTargetName(applicationConfig.TargetName)),
		options.interactive,
		options.prompter,
	)
	if err != nil {
		return err
	}

	if cache != nil && !cacheHit {
		archiveSHA256, hashErr := verify.SHA256File(temporaryPath)
		if hashErr != nil {
			if _, err := fmt.Fprintf(out, "Warning: cannot index artifact cache: compute archive SHA-256: %v\n", hashErr); err != nil {
				return err
			}
		} else {
			cacheKey.ArchiveBinary = archiveBinary
			cacheKey.TargetName = targetName
			if _, publishErr := cache.PublishLocked(cacheKey, archiveSHA256, binary); publishErr != nil {
				if _, err := fmt.Fprintf(out, "Warning: cannot publish artifact cache: %v\n", publishErr); err != nil {
					return err
				}
			}
		}
	}

	target, err := install.TargetPathFor(repo, targetName)
	if err != nil {
		return err
	}
	_, targetErr := os.Lstat(target)
	foreignWrapper := targetErr == nil && !previouslyInstalled
	if targetErr != nil && !os.IsNotExist(targetErr) {
		return fmt.Errorf("check install target %q: %w", target, targetErr)
	}

	// A pre-existing wrapper must belong to zenget to be overwritten safely.
	if previouslyInstalled && targetErr == nil {
		data, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("read existing wrapper %q: %w", target, err)
		}
		if !wrapper.IsMarkerLine(string(data)) {
			return fmt.Errorf("refusing to overwrite foreign file %q", target)
		}
	}

	var transaction *install.VersionedInstallTransaction
	var wrapperPath, realPath string
	var previousArtifact *state.Artifact
	var retiredArtifacts []string
	if previouslyInstalled {
		previousArtifact, err = validatedActiveArtifact(previous)
		if err != nil {
			return err
		}
		retiredArtifacts, err = validatedRetiredArtifacts(previous, "")
		if err != nil {
			return err
		}
	}
	if previouslyInstalled {
		transaction, err = install.InstallUpgradeVersioned(
			binary,
			providerName,
			repository,
			rel.TagName,
			targetName,
			retiredArtifacts...,
		)
		if err != nil {
			return err
		}
		wrapperPath, realPath = transaction.WrapperPath(), transaction.RealPath()
	} else {
		wrapperPath, realPath, err = install.InstallVersioned(
			binary,
			providerName,
			repository,
			rel.TagName,
			foreignWrapper,
			targetName,
		)
		if err != nil {
			return err
		}
	}

	activeArtifact := &state.Artifact{
		Version:  rel.TagName,
		RealPath: realPath,
		XXH3_64:  hashx.Bytes(binary),
	}
	st.Set(repository, state.App{
		Provider:         providerName,
		Version:          rel.TagName,
		InstalledAt:      time.Now().UTC(),
		DownloadURL:      asset.BrowserDownloadURL,
		InstallPath:      wrapperPath,
		RealPath:         realPath,
		XXH3_64:          activeArtifact.XXH3_64,
		Pinned:           previouslyInstalled && previous.Pinned,
		PlatformOS:       options.goos,
		PlatformArch:     options.goarch,
		ReleaseFilter:    options.releaseFilter,
		ActiveArtifact:   activeArtifact,
		PreviousArtifact: previousArtifact,
	})
	if err := persistState(st); err != nil {
		if transaction != nil {
			if rollbackErr := transaction.Rollback(); rollbackErr != nil {
				return fmt.Errorf("save state: %w (rollback failed: %v)", err, rollbackErr)
			}
		} else if rollbackErr := rollbackFreshInstall(wrapperPath, realPath); rollbackErr != nil {
			return fmt.Errorf("save state: %w (rollback failed: %v)", err, rollbackErr)
		}
		return fmt.Errorf("save state: %w", err)
	}
	if transaction != nil {
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("commit upgrade: %w", err)
		}
		for _, warning := range transaction.CleanupWarnings() {
			if _, err := fmt.Fprintf(out, "Warning: %v\n", warning); err != nil {
				return err
			}
		}
	}

	switch {
	case explicitSelector != nil:
		applicationConfig.Asset = ""
		selectorCopy := *explicitSelector
		applicationConfig.AssetSelector = &selectorCopy
	case options.assetOverride != "" || options.assetOverrideSet || assetWasPrompted:
		applicationConfig.Asset = selectedAssetName
		applicationConfig.AssetSelector = nil
	case applicationConfig.AssetSelector != nil:
		applicationConfig.Asset = ""
	default:
		applicationConfig.Asset = selectedAssetName
	}
	applicationConfig.ArchiveBinary = archiveBinary
	applicationConfig.TargetName = targetName
	if err := applicationConfig.Save(repository); err != nil {
		return fmt.Errorf("save app config: %w", err)
	}

	if previouslyInstalled {
		_, err = fmt.Fprintf(out, "Upgraded %s %s -> %s%s\n", repo, previous.Version, rel.TagName, releaseAgeNotice(ageFiltered, options.minAgeDays))
		if err != nil {
			return err
		}
		// If the install directory changed, remove the wrapper left behind in
		// the previous location. Only files carrying the zenget marker are
		// removed; foreign files are left untouched.
		if previous.InstallPath != "" && previous.InstallPath != wrapperPath {
			if err := removeZengetWrapper(previous.InstallPath); err != nil {
				return err
			}
		}
		return nil
	}
	_, err = fmt.Fprintf(out, "Installed %s %s to %s%s\n", repo, rel.TagName, target, releaseAgeNotice(ageFiltered, options.minAgeDays))
	return err
}

func providerNameFor(client provider.Provider) string {
	if named, ok := client.(interface{ ProviderName() string }); ok {
		if name := named.ProviderName(); name != "" {
			return name
		}
	}
	return string(github.ForgeGitHub)
}

func resolveOfflineRelease(repository string, options installOptions) (*github.Release, error) {
	cache, err := artifactcache.New()
	if err != nil {
		return nil, fmt.Errorf("offline install requires the artifact cache: %w", err)
	}
	inspections, err := cache.List(artifactcache.Protection{})
	if err != nil {
		return nil, fmt.Errorf("offline install cannot inspect the artifact cache: %w", err)
	}
	platformLibc := install.LibcIdentity(options.goos)
	var candidates []artifactcache.Entry
	for _, inspection := range inspections {
		entry := inspection.Entry
		if inspection.Integrity != artifactcache.IntegrityValid || entry.Provider != string(github.ForgeGitHub) || entry.Repository != repository ||
			entry.PlatformOS != options.goos || entry.PlatformArch != options.goarch || entry.PlatformLibc != platformLibc {
			continue
		}
		if options.tag != "" && entry.Tag != options.tag {
			continue
		}
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("offline install requires a verified artifact cache entry for %s; no network request was made", repository)
	}
	tag := options.tag
	if tag == "" {
		if st, stateErr := state.Load(); stateErr == nil {
			if app, ok := st.Get(repository); ok {
				tag = app.Version
			}
		} else {
			return nil, fmt.Errorf("offline install cannot load state: %w", stateErr)
		}
	}
	if tag == "" {
		seenTags := make(map[string]struct{})
		for _, entry := range candidates {
			seenTags[entry.Tag] = struct{}{}
		}
		if len(seenTags) != 1 {
			return nil, fmt.Errorf("offline install requires --tag or one unambiguous cached release for %s; no network request was made", repository)
		}
		for cachedTag := range seenTags {
			tag = cachedTag
		}
	}
	assetsByKey := make(map[string]github.Asset)
	for _, entry := range candidates {
		if entry.Tag != tag {
			continue
		}
		key := fmt.Sprintf("%d\x00%s\x00%s", entry.AssetID, entry.AssetName, entry.ArchiveSHA256)
		assetsByKey[key] = github.Asset{
			ID: entry.AssetID, Name: entry.AssetName, Digest: "sha256:" + entry.ArchiveSHA256,
		}
	}
	if len(assetsByKey) == 0 {
		return nil, fmt.Errorf("offline install has no verified cached asset for %s release %s; no network request was made", repository, tag)
	}
	assets := make([]github.Asset, 0, len(assetsByKey))
	for _, asset := range assetsByKey {
		assets = append(assets, asset)
	}
	sort.Slice(assets, func(left, right int) bool {
		if assets[left].Name != assets[right].Name {
			return assets[left].Name < assets[right].Name
		}
		if assets[left].ID != assets[right].ID {
			return assets[left].ID < assets[right].ID
		}
		return assets[left].Digest < assets[right].Digest
	})
	return &github.Release{TagName: tag, Assets: assets}, nil
}

func cacheArchiveBinaryHint(options installOptions, applicationConfig *appcfg.AppConfig) string {
	if options.binName != "" {
		return options.binName
	}
	if applicationConfig == nil {
		return ""
	}
	return applicationConfig.ArchiveBinary
}

func cacheTargetNameHint(options installOptions, applicationConfig *appcfg.AppConfig) string {
	if options.targetName != "" {
		return options.targetName
	}
	if applicationConfig == nil {
		return ""
	}
	if validTargetName(applicationConfig.TargetName) {
		return applicationConfig.TargetName
	}
	return ""
}

func recipeArchiveBinary(installRecipe *recipe.Recipe) string {
	if installRecipe == nil {
		return ""
	}
	return installRecipe.ArchiveBinary
}

func recipeTargetName(installRecipe *recipe.Recipe) string {
	if installRecipe == nil {
		return ""
	}
	return installRecipe.TargetName
}

func resolveRecipeChecksumAsset(rel *github.Release, checksum recipe.Checksum) (github.Asset, error) {
	if rel == nil {
		return github.Asset{}, fmt.Errorf("recipe checksum cannot be resolved without a release")
	}
	if err := checksum.Validate(); err != nil {
		return github.Asset{}, fmt.Errorf("recipe checksum: %w", err)
	}
	var matches []github.Asset
	for _, asset := range rel.Assets {
		if checksum.Matches(asset.Name) {
			matches = append(matches, asset)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	names := make([]string, len(matches))
	for index, asset := range matches {
		names[index] = asset.Name
	}
	sort.Strings(names)
	if len(matches) == 0 {
		return github.Asset{}, fmt.Errorf("recipe checksum selector %s %q matched no assets in release %q", checksum.Type, checksum.Pattern, rel.TagName)
	}
	return github.Asset{}, fmt.Errorf("recipe checksum selector %s %q matched multiple release assets: %s", checksum.Type, checksum.Pattern, strings.Join(names, ", "))
}

func prepareInstallRecipe(options installOptions, repository string) (*recipe.Recipe, error) {
	var loaded *recipe.Recipe
	if options.installRecipe != nil {
		copy := *options.installRecipe
		loaded = &copy
	} else {
		var err error
		loaded, err = loadInstallRecipe(options.recipePath, options.recipePathSet || options.recipePath != "")
		if err != nil {
			return nil, err
		}
	}
	if loaded == nil {
		return nil, nil
	}
	normalized, err := loaded.Normalize()
	if err != nil {
		return nil, fmt.Errorf("validate recipe: %w", err)
	}
	if normalized.Repository != repository {
		return nil, fmt.Errorf("recipe repository %q does not match command repository %q", normalized.Repository, repository)
	}
	for _, supported := range normalized.SupportedSystems {
		if supported == normalizedSystem(options.goos, options.goarch) {
			return &normalized, nil
		}
	}
	if len(normalized.SupportedSystems) > 0 {
		return nil, fmt.Errorf("recipe %q does not support target %s/%s", normalized.Repository, options.goos, options.goarch)
	}
	return &normalized, nil
}

func normalizedSystem(goos, goarch string) string {
	return strings.ToLower(strings.TrimSpace(goos)) + "/" + strings.ToLower(strings.TrimSpace(goarch))
}

func assetSelectorFromInstallOptions(options installOptions) (*appcfg.AssetSelector, error) {
	assetProvided := options.assetOverrideSet || options.assetOverride != ""
	substringProvided := options.assetMatchSet || options.assetMatch != ""
	regexProvided := options.assetMatchRegexSet || options.assetMatchRegex != ""
	provided := 0
	if assetProvided {
		provided++
	}
	if substringProvided {
		provided++
	}
	if regexProvided {
		provided++
	}
	if provided > 1 {
		return nil, fmt.Errorf("--asset, --asset-match, and --asset-match-regex are mutually exclusive")
	}
	if substringProvided {
		if options.assetMatch == "" {
			return nil, fmt.Errorf("--asset-match must not be empty")
		}
		selector := appcfg.AssetSelector{Version: 1, Type: appcfg.AssetSelectorSubstring, Pattern: options.assetMatch}
		return &selector, nil
	}
	if regexProvided {
		if options.assetMatchRegex == "" {
			return nil, fmt.Errorf("--asset-match-regex must not be empty")
		}
		selector := appcfg.AssetSelector{Version: 1, Type: appcfg.AssetSelectorRegex, Pattern: options.assetMatchRegex}
		if err := selector.Validate(); err != nil {
			return nil, err
		}
		return &selector, nil
	}
	return nil, nil
}

func resolveInstallAsset(rel *github.Release, goos, goarch, explicit, saved string, interactive bool, prompter *wizard.Session) (github.Asset, string, bool, error) {
	return resolveInstallAssetWithSelectors(rel, goos, goarch, explicit, nil, saved, nil, nil, interactive, prompter)
}

func resolveInstallAssetWithSelectors(rel *github.Release, goos, goarch, explicit string, explicitSelector *appcfg.AssetSelector, saved string, savedSelector *appcfg.AssetSelector, installRecipe *recipe.Recipe, interactive bool, prompter *wizard.Session) (github.Asset, string, bool, error) {
	if explicit != "" {
		asset, err := install.SelectAsset(rel, goos, goarch, explicit)
		if err != nil {
			return github.Asset{}, "", false, err
		}
		return asset, asset.Name, false, nil
	}
	if explicitSelector != nil {
		asset, err := install.SelectAssetWithSelector(rel, goos, goarch, explicitSelector)
		if err != nil {
			return github.Asset{}, "", false, err
		}
		return asset, asset.Name, false, nil
	}

	stale := false
	savedSelectorStale := false
	if savedSelector != nil {
		asset, err := install.SelectAssetWithSelector(rel, goos, goarch, savedSelector)
		if err == nil {
			return asset, asset.Name, false, nil
		}
		stale = true
		savedSelectorStale = true
	}
	if saved != "" && savedSelector == nil {
		asset, err := install.SelectAsset(rel, goos, goarch, saved)
		if err == nil {
			return asset, asset.Name, false, nil
		}
		stale = true
	}
	if !stale && installRecipe != nil && installRecipe.AssetSelector != nil {
		asset, err := install.SelectAssetWithSelector(rel, goos, goarch, installRecipe.AssetSelector)
		if err != nil {
			return github.Asset{}, "", false, fmt.Errorf("recipe asset_selector: %w", err)
		}
		return asset, asset.Name, false, nil
	}

	asset, err := install.SelectAsset(rel, goos, goarch, "")
	if err == nil && !stale {
		return asset, asset.Name, false, nil
	}

	if !stale && !isAmbiguousAssetError(err) {
		return github.Asset{}, "", false, err
	}
	if !interactive || prompter == nil {
		if stale {
			if savedSelectorStale {
				return github.Asset{}, "", false, fmt.Errorf("saved asset selector %s %q is no longer available; non-interactive mode requires --asset-match or --asset-match-regex", savedSelector.Type, savedSelector.Pattern)
			}
			return github.Asset{}, "", false, fmt.Errorf("saved asset choice %q is no longer available; non-interactive mode requires --asset", saved)
		}
		return github.Asset{}, "", false, fmt.Errorf("%w; non-interactive mode requires --asset", err)
	}

	candidates := install.FilterAssets(rel.Assets, goos, goarch)
	if len(candidates) == 0 {
		if err != nil {
			return github.Asset{}, "", false, err
		}
		return github.Asset{}, "", false, fmt.Errorf("release %q has no asset choices", rel.TagName)
	}
	names := make([]string, len(candidates))
	for i, candidate := range candidates {
		names[i] = candidate.Name
	}
	choice, chooseErr := prompter.Choose("Select a release asset:", names)
	if chooseErr != nil {
		return github.Asset{}, "", false, chooseErr
	}
	asset, err = install.SelectAsset(rel, goos, goarch, choice)
	if err != nil {
		return github.Asset{}, "", false, err
	}
	return asset, asset.Name, true, nil
}

func resolveArchiveBinaryWithRecipe(assetPath string, format install.Format, defaultName, explicit, saved, recipeBinary string, interactive bool, prompter *wizard.Session) (string, bool, error) {
	executables, err := install.ListExecutables(assetPath, format)
	if err != nil {
		return "", false, err
	}
	if len(executables) == 0 {
		if recipeBinary != "" {
			return "", false, fmt.Errorf("recipe archive_binary %q requires an archive with executable files", recipeBinary)
		}
		return "", false, nil
	}

	if explicit != "" {
		if !containsExecutable(executables, explicit) {
			return "", false, fmt.Errorf("no executable matching %q; use --bin-name with one of: %s", explicit, strings.Join(executables, ", "))
		}
		return explicit, false, nil
	}
	if saved != "" && containsExecutable(executables, saved) {
		return saved, false, nil
	}
	stale := saved != ""
	if !stale && recipeBinary != "" {
		if !containsRecipeExecutable(executables, recipeBinary) {
			return "", false, fmt.Errorf("recipe archive_binary %q is not available; executable candidates: %s", recipeBinary, strings.Join(executables, ", "))
		}
		return recipeBinary, false, nil
	}
	if len(executables) == 1 && !stale {
		return executables[0], false, nil
	}
	if !interactive || prompter == nil {
		if stale {
			return "", false, fmt.Errorf("saved archive binary choice %q is no longer available; non-interactive mode requires --bin-name", saved)
		}
		return "", false, fmt.Errorf("archive contains multiple executable files; non-interactive mode requires --bin-name")
	}

	choice, err := prompter.Choose(fmt.Sprintf("Select an executable from %s:", defaultName), executables)
	if err != nil {
		return "", false, err
	}
	return choice, true, nil
}

func containsExecutable(executables []string, wanted string) bool {
	for _, executable := range executables {
		if executable == wanted || executable == pathBase(wanted) || pathBase(executable) == pathBase(wanted) {
			return true
		}
	}
	return false
}

func containsRecipeExecutable(executables []string, wanted string) bool {
	if !strings.Contains(wanted, "/") {
		return containsExecutable(executables, wanted)
	}
	wanted = path.Clean(wanted)
	for _, executable := range executables {
		if path.Clean(executable) == wanted {
			return true
		}
	}
	return false
}

func pathBase(name string) string {
	if index := strings.LastIndexAny(name, "/\\"); index >= 0 {
		return name[index+1:]
	}
	return name
}

func resolveTargetNameWithRecipe(repo, explicit, saved, recipeTarget string, promptNeeded, interactive bool, prompter *wizard.Session) (string, bool, error) {
	if explicit != "" {
		return explicit, false, nil
	}
	if saved != "" && validTargetName(saved) {
		return saved, false, nil
	}
	if saved != "" && !validTargetName(saved) && !interactive {
		return "", false, fmt.Errorf("saved target name %q is invalid; non-interactive mode requires --name", saved)
	}
	if saved == "" && recipeTarget != "" {
		return recipeTarget, false, nil
	}
	if !promptNeeded {
		return repo, false, nil
	}
	if !interactive || prompter == nil {
		return "", false, fmt.Errorf("target name requires interactive input; non-interactive mode requires --name")
	}
	name, err := prompter.Ask("Target binary name", repo)
	if err != nil {
		return "", false, err
	}
	if !validTargetName(name) {
		return "", false, fmt.Errorf("invalid target name %q", name)
	}
	return name, true, nil
}

func validTargetName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\") && !strings.ContainsRune(name, '\x00')
}

func isAmbiguousAssetError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "ambiguous assets matching") ||
		strings.Contains(message, "multiple downloadable assets and none match")
}

func releaseAgeNotice(ageFiltered bool, minAgeDays int) string {
	if !ageFiltered {
		return ""
	}
	return fmt.Sprintf(" (latest release too young for --min-age-days=%d)", minAgeDays)
}

// removeZengetWrapper deletes path only when it exists and contains the
// zenget wrapper marker. Missing files and foreign files are ignored.
func removeZengetWrapper(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read old wrapper %q: %w", path, err)
	}
	if !wrapper.IsMarkerLine(string(data)) {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove old wrapper %q: %w", path, err)
	}
	return nil
}

func validatedActiveArtifact(app state.App) (*state.Artifact, error) {
	artifact := app.Active()
	if artifact == nil || artifact.RealPath == "" {
		return nil, nil
	}
	if artifact.XXH3_64 == "" {
		return nil, fmt.Errorf("cannot retain active artifact %q: hash is missing", artifact.RealPath)
	}
	managed, err := wrapper.IsManagedArtifactPath(artifact.RealPath)
	if err != nil {
		return nil, fmt.Errorf("validate active artifact %q: %w", artifact.RealPath, err)
	}
	if !managed {
		return nil, fmt.Errorf("refusing to use active artifact outside zenget data directory: %q", artifact.RealPath)
	}
	info, err := os.Lstat(artifact.RealPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("check active artifact %q: %w", artifact.RealPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("active artifact %q is not a regular file", artifact.RealPath)
	}
	actual, err := hashx.File(artifact.RealPath)
	if err != nil {
		return nil, fmt.Errorf("hash active artifact %q: %w", artifact.RealPath, err)
	}
	if actual != artifact.XXH3_64 {
		// A locally modified active binary must not be treated as a managed
		// previous artifact. The upgrade itself remains transactional and may
		// replace it, but the modified file is never scheduled for cleanup.
		return nil, nil
	}
	return artifact, nil
}

func validatedRetiredArtifacts(app state.App, newPath string) ([]string, error) {
	artifact := app.PreviousArtifact
	if artifact == nil || artifact.RealPath == "" || artifact.RealPath == newPath {
		return nil, nil
	}
	if artifact.XXH3_64 == "" {
		return nil, fmt.Errorf("cannot retire previous artifact %q: hash is missing", artifact.RealPath)
	}
	managed, err := wrapper.IsManagedArtifactPath(artifact.RealPath)
	if err != nil {
		return nil, fmt.Errorf("validate previous artifact %q: %w", artifact.RealPath, err)
	}
	if !managed {
		return nil, fmt.Errorf("refusing to retire artifact outside zenget data directory: %q", artifact.RealPath)
	}
	info, err := os.Lstat(artifact.RealPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("check previous artifact %q: %w", artifact.RealPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("previous artifact %q is not a regular file", artifact.RealPath)
	}
	actual, err := hashx.File(artifact.RealPath)
	if err != nil {
		return nil, fmt.Errorf("hash previous artifact %q: %w", artifact.RealPath, err)
	}
	if actual != artifact.XXH3_64 {
		// Keep modified historical files untouched. They are no longer safe to
		// classify as disposable managed artifacts.
		return nil, nil
	}
	return []string{artifact.RealPath}, nil
}

func rollbackFreshInstall(wrapperPath, realPath string) error {
	var firstErr error
	if wrapperPath != "" {
		if err := removeZengetWrapper(wrapperPath); err != nil {
			firstErr = err
		}
	}
	if realPath != "" {
		if err := os.Remove(realPath); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("remove new artifact %q: %w", realPath, err)
		}
	}
	return firstErr
}

// resolveReleaseWithFilter returns the release to install. An empty filter
// delegates to the established resolver so existing callers keep the exact
// same provider calls and selection behavior. A non-empty filter applies to
// latest-release candidates and to the contains fallback, while an exact tag
// lookup remains authoritative.
func resolveReleaseWithFilter(ctx context.Context, client provider.Provider, org, repo, tag, releaseFilter string) (*github.Release, error) {
	if err := validateReleaseFilter(releaseFilter); err != nil {
		return nil, err
	}
	if releaseFilter == "" {
		return resolveReleaseWithoutFilter(ctx, client, org, repo, tag)
	}
	if tag == "" {
		return resolveLatestReleaseWithFilter(ctx, client, org, repo, releaseFilter)
	}

	rel, err := client.ReleaseByTag(ctx, org, repo, tag)
	if err == nil {
		return rel, nil
	}
	if !isNotFound(err) {
		return nil, err
	}

	releases, err := listReleaseCandidates(ctx, client, org, repo)
	if err != nil {
		return nil, err
	}
	filtered, err := filterReleasesByTag(releases, releaseFilter)
	if err != nil {
		return nil, err
	}

	var best *github.Release
	for i := range filtered {
		if !strings.Contains(filtered[i].TagName, tag) {
			continue
		}
		if best == nil || filtered[i].PublishedAt.After(best.PublishedAt) {
			best = &filtered[i]
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no release tag matches %q after applying release filter %q", tag, releaseFilter)
	}
	return best, nil
}

func resolveReleaseWithoutFilter(ctx context.Context, client provider.Provider, org, repo, tag string) (*github.Release, error) {
	if tag == "" {
		return client.LatestRelease(ctx, org, repo)
	}

	rel, err := client.ReleaseByTag(ctx, org, repo, tag)
	if err == nil {
		return rel, nil
	}
	if !isNotFound(err) {
		return nil, err
	}

	releases, err := client.ListReleases(ctx, org, repo)
	if err != nil {
		return nil, err
	}

	var best *github.Release
	for i := range releases {
		if !strings.Contains(releases[i].TagName, tag) {
			continue
		}
		if best == nil || releases[i].PublishedAt.After(best.PublishedAt) {
			best = &releases[i]
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no release tag matches %q", tag)
	}
	return best, nil
}

func resolveLatestReleaseWithFilter(ctx context.Context, client provider.Provider, org, repo, releaseFilter string) (*github.Release, error) {
	releases, err := listReleaseCandidates(ctx, client, org, repo)
	if err != nil {
		return nil, err
	}
	filtered, err := filterReleasesByTag(releases, releaseFilter)
	if err != nil {
		return nil, err
	}

	var latest *github.Release
	for i := range filtered {
		release := &filtered[i]
		if release.TagName == "" || release.Draft || release.Prerelease {
			continue
		}
		if latest == nil || release.PublishedAt.After(latest.PublishedAt) {
			latest = release
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("no published release matches release filter %q", releaseFilter)
	}
	return latest, nil
}

func validateReleaseFilter(releaseFilter string) error {
	if releaseFilter == "" {
		return nil
	}
	if _, err := path.Match(releaseFilter, ""); err != nil {
		return fmt.Errorf("invalid release filter %q: %w", releaseFilter, err)
	}
	return nil
}

type allReleaseLister interface {
	ListReleasesAll(ctx context.Context, org, repo string) ([]github.Release, error)
}

func listReleaseCandidates(ctx context.Context, client provider.Provider, org, repo string) ([]github.Release, error) {
	if paged, ok := client.(allReleaseLister); ok {
		return paged.ListReleasesAll(ctx, org, repo)
	}
	return client.ListReleases(ctx, org, repo)
}

func filterReleasesByTag(releases []github.Release, releaseFilter string) ([]github.Release, error) {
	if err := validateReleaseFilter(releaseFilter); err != nil {
		return nil, err
	}
	if releaseFilter == "" {
		return releases, nil
	}

	filtered := make([]github.Release, 0, len(releases))
	for _, release := range releases {
		matches, err := path.Match(releaseFilter, release.TagName)
		if err != nil {
			return nil, fmt.Errorf("match release filter %q: %w", releaseFilter, err)
		}
		if matches {
			filtered = append(filtered, release)
		}
	}
	return filtered, nil
}

func validateReleaseSelection(tag string, minAgeDays int, minAgeFlagChanged bool) error {
	if minAgeDays < 0 || (minAgeFlagChanged && minAgeDays <= 0) {
		return fmt.Errorf("--min-age-days must be a positive integer")
	}
	if minAgeDays > 0 && tag != "" {
		return fmt.Errorf("--tag cannot be combined with --min-age-days")
	}
	return nil
}

// resolveInstallRelease applies the install release-selection policy shared by
// install and read-only inspection. An injected release is used by tests and
// manifest workflows; otherwise minimum age, exact tag, and normal latest/tag
// resolution are selected in that order.
func resolveInstallRelease(ctx context.Context, client provider.Provider, org, repo string, options installOptions) (*github.Release, bool, error) {
	if options.release != nil {
		return options.release, false, nil
	}
	if options.minAgeDays > 0 {
		return resolveReleaseWithMinAgeFilter(ctx, client, org, repo, options.minAgeDays, options.releaseFilter)
	}
	if options.exactTag {
		release, err := client.ReleaseByTag(ctx, org, repo, options.tag)
		return release, false, err
	}
	release, err := resolveReleaseWithFilter(ctx, client, org, repo, options.tag, options.releaseFilter)
	return release, false, err
}

func releaseResolutionError(repository string, options installOptions, err error) error {
	if options.tag == "" && options.minAgeDays == 0 {
		return fmt.Errorf("fetch latest release for %s: %w", repository, err)
	}
	return fmt.Errorf("fetch release for %s: %w", repository, err)
}

func resolveReleaseWithMinAgeFilter(ctx context.Context, client provider.Provider, org, repo string, minAgeDays int, releaseFilter string) (*github.Release, bool, error) {
	var (
		releases []github.Release
		err      error
	)
	if releaseFilter == "" {
		releases, err = client.ListReleases(ctx, org, repo)
	} else {
		releases, err = listReleaseCandidates(ctx, client, org, repo)
	}
	if err != nil {
		return nil, false, err
	}
	if releaseFilter != "" {
		releases, err = filterReleasesByTag(releases, releaseFilter)
		if err != nil {
			return nil, false, err
		}
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -minAgeDays)
	var latest, selected *github.Release
	for i := range releases {
		release := &releases[i]
		if release.Draft || release.Prerelease || release.PublishedAt.IsZero() {
			continue
		}
		if latest == nil || release.PublishedAt.After(latest.PublishedAt) {
			latest = release
		}
		if release.PublishedAt.After(cutoff) {
			continue
		}
		if selected == nil || release.PublishedAt.After(selected.PublishedAt) {
			selected = release
		}
	}

	if selected == nil {
		if releaseFilter != "" {
			return nil, false, fmt.Errorf("no release matching release filter %q is at least %d days old", releaseFilter, minAgeDays)
		}
		return nil, false, fmt.Errorf("no release is at least %d days old", minAgeDays)
	}
	return selected, selected != latest, nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "404")
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

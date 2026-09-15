package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/github"
	"zenget/internal/install"
	manifestfile "zenget/internal/manifest"
	"zenget/internal/provider"
	"zenget/internal/state"
)

var (
	addReplace       bool
	addClientFactory = func(cmd *cobra.Command) (provider.Provider, error) { return newCommandProvider(cmd) }
)

var addCmd = &cobra.Command{
	Use:   "add <org/repo>",
	Short: "Add an exact release choice to a portable manifest",
	Args:  cobra.ExactArgs(1),
	RunE:  runAdd,
}

func init() {
	addCmd.Flags().String("tag", "", "Exact release tag to record; defaults to the latest release")
	addCmd.Flags().String("manifest", "", "Manifest file to update; defaults to project discovery")
	addCmd.Flags().BoolVar(&addReplace, "replace", false, "Replace an existing repository entry")
	addCmd.Flags().String("asset", "", "Select a release asset by exact name or unique substring")
	addCmd.Flags().String("bin-name", "", "Executable path inside an archive to record")
	addCmd.Flags().String("name", "", "Stable wrapper/target name to record")
	addCmd.Flags().String("system", "", "Target platform as os/arch (e.g. linux/amd64, darwin/arm64)")
	addCmd.Flags().String("api-base-url", "", "Release API base URL")
	addCmd.Flags().String("forge", "", "Release forge: github, gitlab, or forgejo")
	rootCmd.AddCommand(addCmd)
}

func runAdd(cmd *cobra.Command, args []string) error {
	repository := firstArg(args)
	if !repoPattern.MatchString(repository) {
		return fmt.Errorf("invalid repository %q: expected org/repo", repository)
	}
	manifestPath, err := getOptionalStringFlag(cmd, "manifest")
	if err != nil {
		return fmt.Errorf("read --manifest flag: %w", err)
	}
	resolution, err := resolveProjectManifest(manifestPath)
	if err != nil {
		return err
	}
	manifestPath = resolution.Path
	file := resolution.Manifest
	existingIndex := -1
	for index, entry := range file.Apps {
		if entry.Repository == repository {
			existingIndex = index
			break
		}
	}
	if existingIndex >= 0 && !addReplace {
		return fmt.Errorf("manifest already contains %s; use --replace to replace it", repository)
	}

	requestedTag, err := getOptionalStringFlag(cmd, "tag")
	if err != nil {
		return fmt.Errorf("read --tag flag: %w", err)
	}
	assetOverride, err := getOptionalStringFlag(cmd, "asset")
	if err != nil {
		return fmt.Errorf("read --asset flag: %w", err)
	}
	binName, err := getOptionalStringFlag(cmd, "bin-name")
	if err != nil {
		return fmt.Errorf("read --bin-name flag: %w", err)
	}
	targetName, err := getOptionalStringFlag(cmd, "name")
	if err != nil {
		return fmt.Errorf("read --name flag: %w", err)
	}
	system, err := getOptionalStringFlag(cmd, "system")
	if err != nil {
		return fmt.Errorf("read --system flag: %w", err)
	}
	goos, goarch, err := parseSystem(system)
	if err != nil {
		return err
	}
	platformOS, platformArch := "", ""
	if system != "" {
		platformOS, platformArch = goos, goarch
	}
	if err := validateAddChoices(repository, requestedTag, assetOverride, binName, targetName, platformOS, platformArch); err != nil {
		return err
	}

	client, err := addClientFactory(cmd)
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("release provider is nil")
	}
	if providerNameFor(client) != manifestProviderGitHub {
		return fmt.Errorf("manifest v1 supports provider %q; configured provider is %q", manifestProviderGitHub, providerNameFor(client))
	}
	release, err := resolveAddRelease(cmd.Context(), client, repository, requestedTag)
	if err != nil {
		return err
	}
	asset, err := install.SelectAsset(release, goos, goarch, assetOverride)
	if err != nil {
		return fmt.Errorf("select asset for %s: %w", repository, err)
	}
	entry := state.ManifestApp{
		Repository:    repository,
		Provider:      manifestProviderGitHub,
		Tag:           release.TagName,
		Asset:         asset.Name,
		PlatformOS:    platformOS,
		PlatformArch:  platformArch,
		ArchiveBinary: binName,
		TargetName:    targetName,
	}
	if err := manifestfile.ValidateEntry(entry); err != nil {
		return fmt.Errorf("validate manifest entry: %w", err)
	}

	file.SchemaURL = manifestfile.SchemaURL
	if existingIndex >= 0 {
		file.Apps[existingIndex] = entry
	} else {
		file.Apps = append(file.Apps, entry)
	}
	data, err := manifestfile.Marshal(manifestfile.Canonicalize(file))
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := manifestfile.WriteFile(manifestPath, data, true); err != nil {
		return err
	}
	writer := cmd.OutOrStdout()
	if writer == nil {
		writer = io.Discard
	}
	action := "Added"
	if existingIndex >= 0 {
		action = "Replaced"
	}
	_, err = fmt.Fprintf(writer, "%s %s %s in %s\n", action, repository, entry.Tag, manifestPath)
	return err
}

func validateAddChoices(repository, tag, asset, binName, targetName, goos, goarch string) error {
	if tag == "" {
		tag = "v0.0.0"
	}
	entry := state.ManifestApp{
		Repository:    repository,
		Provider:      manifestProviderGitHub,
		Tag:           tag,
		Asset:         asset,
		PlatformOS:    goos,
		PlatformArch:  goarch,
		ArchiveBinary: binName,
		TargetName:    targetName,
	}
	if err := manifestfile.ValidateEntry(entry); err != nil {
		return fmt.Errorf("invalid add choices: %w", err)
	}
	return nil
}

func resolveAddRelease(ctx context.Context, client provider.Provider, repository, requestedTag string) (*github.Release, error) {
	org, repo, _ := strings.Cut(repository, "/")
	if requestedTag == "" {
		release, err := client.LatestRelease(ctx, org, repo)
		if err != nil {
			return nil, fmt.Errorf("resolve latest release for %s: %w", repository, err)
		}
		if release == nil || release.TagName == "" {
			return nil, fmt.Errorf("resolve latest release for %s: provider returned no exact tag", repository)
		}
		return release, nil
	}
	release, err := client.ReleaseByTag(ctx, org, repo, requestedTag)
	if err != nil {
		return nil, fmt.Errorf("resolve release %s %s: %w", repository, requestedTag, err)
	}
	if release == nil || release.TagName != requestedTag {
		return nil, fmt.Errorf("resolve release %s: provider returned tag %q for requested tag %q", repository, releaseTag(release), requestedTag)
	}
	return release, nil
}

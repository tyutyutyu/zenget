package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"zenget/internal/recipe"
	"zenget/internal/recipepolicy"
	"zenget/internal/registry"
)

var (
	registryListJSON    bool
	registrySyncAll     bool
	registrySyncOffline bool
)

var registryCmd = &cobra.Command{
	Use:     "registry",
	Aliases: []string{"reg"},
	Short:   "Manage commit-pinned remote recipe registries",
	Long:    "Manage named, commit-pinned GitHub recipe registries. A source must use github:owner/repository@<40-hex-commit>:<relative-root>; add and remove change only local configuration, while sync fetches a policy-authorized immutable snapshot.",
}

var registryAddCmd = &cobra.Command{
	Use:   "add <name> <source>",
	Short: "Add one commit-pinned GitHub recipe registry",
	Args:  cobra.ExactArgs(2),
	RunE:  runRegistryAdd,
}

var registryRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove one named recipe registry",
	Args:    cobra.ExactArgs(1),
	RunE:    runRegistryRemove,
}

var registryListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "l"},
	Short:   "List configured recipe registries",
	Args:    cobra.NoArgs,
	RunE:    runRegistryList,
}

var registrySyncCmd = &cobra.Command{
	Use:   "sync [<name>|--all]",
	Short: "Synchronize one registry snapshot from GitHub",
	Long:  "Fetch registry.json and every indexed recipe at the exact commit. The current recipe-source policy is checked before requests; --offline validates the existing hash-checked snapshot without network access.",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runRegistrySync,
}

func init() {
	registryListCmd.Flags().BoolVar(&registryListJSON, "json", false, "print registries as JSON")
	registrySyncCmd.Flags().BoolVar(&registrySyncAll, "all", false, "synchronize every configured registry")
	registrySyncCmd.Flags().BoolVar(&registrySyncOffline, "offline", false, "validate the existing snapshot without network access")
	registrySyncCmd.Flags().String("api-base-url", "", "GitHub API base URL")
	registrySyncCmd.Flags().String("forge", "github", "registry API forge; only github is supported")
	registryCmd.AddCommand(registryAddCmd, registryRemoveCmd, registryListCmd, registrySyncCmd)
	rootCmd.AddCommand(registryCmd)
}

func runRegistryAdd(cmd *cobra.Command, args []string) error {
	source, err := registry.ParseSource(args[1])
	if err != nil {
		return fmt.Errorf("parse registry source: %w", err)
	}
	config, err := registry.LoadConfig()
	if err != nil {
		return fmt.Errorf("load registry config: %w", err)
	}
	changed, err := config.Add(args[0], source.String())
	if err != nil {
		return err
	}
	if changed {
		if err := registry.SaveConfig(config); err != nil {
			return fmt.Errorf("save registry config: %w", err)
		}
		_, err = fmt.Fprintf(outputWriter(cmd), "Added registry %s: %s\n", args[0], source.String())
		return err
	}
	_, err = fmt.Fprintf(outputWriter(cmd), "Registry %s already configured: %s\n", args[0], source.String())
	return err
}

func runRegistryRemove(cmd *cobra.Command, args []string) error {
	config, err := registry.LoadConfig()
	if err != nil {
		return fmt.Errorf("load registry config: %w", err)
	}
	changed, err := config.Remove(args[0])
	if err != nil {
		return err
	}
	if changed {
		if err := registry.SaveConfig(config); err != nil {
			return fmt.Errorf("save registry config: %w", err)
		}
		_, err = fmt.Fprintf(outputWriter(cmd), "Removed registry %s\n", args[0])
		return err
	}
	_, err = fmt.Fprintf(outputWriter(cmd), "Registry %s was not configured\n", args[0])
	return err
}

func runRegistryList(cmd *cobra.Command, _ []string) error {
	config, err := registry.LoadConfig()
	if err != nil {
		return fmt.Errorf("load registry config: %w", err)
	}
	jsonOutput, err := registryJSONFlag(cmd, registryListJSON)
	if err != nil {
		return fmt.Errorf("read --json flag: %w", err)
	}
	if jsonOutput {
		return writeJSON(outputWriter(cmd), config)
	}
	return printRegistries(outputWriter(cmd), config)
}

func runRegistrySync(cmd *cobra.Command, args []string) error {
	if registrySyncAll && len(args) > 0 {
		return errorsForRegistrySync("--all cannot be combined with a registry name")
	}
	if !registrySyncAll && len(args) == 0 {
		return errorsForRegistrySync("specify a registry name or --all")
	}
	config, err := registry.LoadConfig()
	if err != nil {
		return fmt.Errorf("load registry config: %w", err)
	}
	selected, err := selectRegistries(config, args, registrySyncAll)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		_, err := fmt.Fprintln(outputWriter(cmd), "No registries configured")
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	cache, err := registry.NewCache()
	if err != nil {
		return fmt.Errorf("open registry cache: %w", err)
	}
	if registrySyncOffline {
		for _, configured := range selected {
			source, parseErr := registry.ParseSource(configured.Source)
			if parseErr != nil {
				return fmt.Errorf("registry %s: %w", configured.Name, parseErr)
			}
			snapshot, loadErr := cache.Load(source)
			if loadErr != nil {
				return fmt.Errorf("validate offline registry %s: %w", configured.Name, loadErr)
			}
			if _, err := fmt.Fprintf(outputWriter(cmd), "Validated registry %s (%d recipes) offline\n", configured.Name, len(snapshot.Index.Entries)); err != nil {
				return err
			}
		}
		return nil
	}

	client, err := newCommandProvider(cmd)
	if err != nil {
		return err
	}
	contents, ok := client.(registryContentsClient)
	if !ok {
		return fmt.Errorf("registry sync requires a GitHub contents client")
	}
	for _, configured := range selected {
		if err := syncOneRegistry(ctx, contents, configured, cache); err != nil {
			return fmt.Errorf("sync registry %s: %w", configured.Name, err)
		}
		if _, err := fmt.Fprintf(outputWriter(cmd), "Synchronized registry %s\n", configured.Name); err != nil {
			return err
		}
	}
	return nil
}

type registryContentsClient interface {
	ContentsAtCommit(context.Context, string, string, string, string) ([]byte, error)
}

func syncOneRegistry(ctx context.Context, client registryContentsClient, configured registry.Registry, cache *registry.Cache) error {
	if client == nil {
		return fmt.Errorf("GitHub contents client is nil")
	}
	if cache == nil {
		return fmt.Errorf("registry cache is nil")
	}
	source, err := registry.ParseSource(configured.Source)
	if err != nil {
		return err
	}
	if err := checkRegistryPolicy(source, registry.IndexFileName); err != nil {
		return err
	}
	indexPath, err := source.IndexPath()
	if err != nil {
		return err
	}
	indexData, err := client.ContentsAtCommit(ctx, source.Owner, source.Repository, source.Commit, indexPath)
	if err != nil {
		return fmt.Errorf("fetch registry index: %w", err)
	}
	index, err := registry.DecodeIndex(indexData)
	if err != nil {
		return fmt.Errorf("validate registry index: %w", err)
	}
	// Preflight every recipe policy before the first recipe request. An index
	// that contains one denied entry therefore cannot cause partial recipe
	// downloads.
	for _, entry := range index.Entries {
		if err := checkRegistryPolicy(source, entry.Path); err != nil {
			return err
		}
	}
	recipes := make(map[string][]byte, len(index.Entries))
	for _, entry := range index.Entries {
		remotePath, err := source.Path(entry.Path)
		if err != nil {
			return err
		}
		data, err := client.ContentsAtCommit(ctx, source.Owner, source.Repository, source.Commit, remotePath)
		if err != nil {
			return fmt.Errorf("fetch recipe %q: %w", entry.Repository, err)
		}
		if got := registry.SHA256(data); got != entry.SHA256 {
			return fmt.Errorf("recipe %q SHA-256 %s does not match index digest %s", entry.Repository, got, entry.SHA256)
		}
		parsed, err := recipe.Decode(data, "registry recipe")
		if err != nil {
			return fmt.Errorf("validate recipe %q: %w", entry.Repository, err)
		}
		if parsed.Repository != entry.Repository {
			return fmt.Errorf("recipe repository %q does not match index repository %q", parsed.Repository, entry.Repository)
		}
		recipes[entry.Path] = append([]byte(nil), data...)
	}
	if err := cache.Save(source, index, recipes); err != nil {
		return fmt.Errorf("publish registry snapshot: %w", err)
	}
	return nil
}

func loadRegistryInstallRecipe(name, repository string) (*recipe.Recipe, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("--registry must name a configured registry")
	}
	config, err := registry.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("load registry config: %w", err)
	}
	configured, found, err := config.Find(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("unknown registry %q", name)
	}
	source, err := registry.ParseSource(configured.Source)
	if err != nil {
		return nil, fmt.Errorf("registry %s source: %w", name, err)
	}
	cache, err := registry.NewCache()
	if err != nil {
		return nil, fmt.Errorf("open registry cache: %w", err)
	}
	entry, data, err := cache.Lookup(source, repository)
	if err != nil {
		return nil, fmt.Errorf("load registry %s snapshot: %w", name, err)
	}
	loaded, err := recipe.Decode(data, "registry recipe")
	if err != nil {
		return nil, fmt.Errorf("validate registry recipe %q: %w", entry.Repository, err)
	}
	if loaded.Repository != repository {
		return nil, fmt.Errorf("registry recipe repository %q does not match command repository %q", loaded.Repository, repository)
	}
	return &loaded, nil
}

// resolveAutomaticRegistryRecipe consults every configured registry snapshot in
// configuration order and returns the first recipe recorded for repository. It
// performs no network I/O: only the local snapshot cache is read, and every
// read is gated by the remote recipe-source policy, so a policy denial blocks
// cache hits the same way a missing snapshot does. Unsynced snapshots, denied
// sources, corrupt snapshots, and repositories absent from an index are
// recorded as skip reasons and never fail resolution; callers silently fall
// back to heuristic install behavior. A nil recipe with no reasons means no
// registry is configured at all.
func resolveAutomaticRegistryRecipe(repository string) (*recipe.Recipe, string, registry.Source, []string) {
	config, err := registry.LoadConfig()
	if err != nil {
		return nil, "", registry.Source{}, []string{fmt.Sprintf("registry config unavailable: %v", err)}
	}
	if len(config.Registries) == 0 {
		return nil, "", registry.Source{}, nil
	}
	cache, err := registry.NewCache()
	if err != nil {
		return nil, "", registry.Source{}, []string{fmt.Sprintf("registry cache unavailable: %v", err)}
	}
	var reasons []string
	for _, configured := range config.Registries {
		source, err := registry.ParseSource(configured.Source)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("registry %s: %v", configured.Name, err))
			continue
		}
		_, data, err := cache.Lookup(source, repository)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("registry %s: %v", configured.Name, err))
			continue
		}
		decoded, err := recipe.Decode(data, "registry recipe")
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("registry %s: %v", configured.Name, err))
			continue
		}
		if decoded.Repository != repository {
			reasons = append(reasons, fmt.Sprintf("registry %s: recipe repository %q does not match %q", configured.Name, decoded.Repository, repository))
			continue
		}
		return &decoded, configured.Name, source, reasons
	}
	return nil, "", registry.Source{}, reasons
}

func checkRegistryPolicy(source registry.Source, relativePath string) error {
	identity, err := source.PolicyIdentity(relativePath)
	if err != nil {
		return err
	}
	decision := recipepolicy.CheckSource(identity)
	if !decision.Allowed {
		return fmt.Errorf("registry source policy denied %s: %s", source.String(), decision.Reason)
	}
	return nil
}

func selectRegistries(config registry.Config, args []string, all bool) ([]registry.Registry, error) {
	if all {
		return append([]registry.Registry(nil), config.Registries...), nil
	}
	entry, found, err := config.Find(args[0])
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("unknown registry %q", args[0])
	}
	return []registry.Registry{entry}, nil
}

func registryJSONFlag(cmd *cobra.Command, fallback bool) (bool, error) {
	if cmd == nil || cmd.Flags().Lookup("json") == nil {
		return fallback, nil
	}
	return cmd.Flags().GetBool("json")
}

func printRegistries(out io.Writer, config registry.Config) error {
	if _, err := fmt.Fprintf(out, "Registries (schema_version=%d)\n", config.SchemaVersion); err != nil {
		return err
	}
	if len(config.Registries) == 0 {
		_, err := fmt.Fprintln(out, "Registries: none")
		return err
	}
	for _, configured := range config.Registries {
		if _, err := fmt.Fprintf(out, "- %s: %s\n", configured.Name, configured.Source); err != nil {
			return err
		}
	}
	return nil
}

func errorsForRegistrySync(message string) error {
	return fmt.Errorf("invalid registry sync arguments: %s", strings.TrimSpace(message))
}

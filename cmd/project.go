package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"zenget/internal/install"
	manifestfile "zenget/internal/manifest"
	"zenget/internal/projectmanifest"
	"zenget/internal/shim"
	"zenget/internal/state"
	"zenget/internal/trust"
	"zenget/internal/wrapper"
)

var projectTrustListJSON bool

var projectCmd = &cobra.Command{
	Use:     "project",
	Aliases: []string{"proj"},
	Short:   "Trust and activate project manifests",
	Long: `Project manifests are inert until their exact local path is trusted.

Trust a manifest, then activate its managed target shims. Activation performs
all validation before changing any shim and never downloads release artifacts.
Enable lazy installation separately when a trusted project target should fetch
its exact recorded release on first execution.`,
}

var projectTrustCmd = &cobra.Command{
	Use:   "trust",
	Short: "Trust the selected local project manifest",
	Long: `Trust the selected local manifest after checking its file and directory
ownership and permissions. Trust is recorded for the canonical manifest path
and does not install or execute any artifact.`,
	Args: cobra.NoArgs,
	RunE: runProjectTrust,
}

var projectTrustListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "l"},
	Short:   "List trusted project manifests",
	Long:    "List the canonical local project manifest paths currently trusted by zenget.",
	Args:    cobra.NoArgs,
	RunE:    runProjectTrustList,
}

var projectUntrustCmd = &cobra.Command{
	Use:   "untrust",
	Short: "Remove trust from the selected project manifest",
	Long:  "Remove trust from a project manifest without deleting its files or changing installed artifacts.",
	Args:  cobra.NoArgs,
	RunE:  runProjectUntrust,
}

var projectActivateCmd = &cobra.Command{
	Use:   "activate",
	Short: "Create project shims without downloading artifacts",
	Long: `Create managed shims for every entry in a trusted project manifest.

Activation is offline and performs a complete preflight first. It does not
download, install, or update the global application registry.`,
	Args: cobra.NoArgs,
	RunE: runProjectActivate,
}

func init() {
	for _, command := range []*cobra.Command{projectTrustCmd, projectUntrustCmd, projectActivateCmd} {
		command.Flags().String("manifest", "", "Manifest path override; defaults to project discovery")
	}
	projectTrustListCmd.Flags().BoolVar(&projectTrustListJSON, "json", false, "print trusted projects as JSON")
	projectTrustCmd.AddCommand(projectTrustListCmd)
	projectCmd.AddCommand(projectTrustCmd, projectUntrustCmd, projectActivateCmd)
	rootCmd.AddCommand(projectCmd)
}

func runProjectTrust(cmd *cobra.Command, _ []string) error {
	resolution, err := resolveProjectCommandManifest(cmd)
	if err != nil {
		return err
	}
	if err := manifestfile.Validate(resolution.Manifest); err != nil {
		return fmt.Errorf("validate project manifest: %w", err)
	}

	store, err := trust.Load()
	if err != nil {
		return fmt.Errorf("load project trust store: %w", err)
	}
	record, changed, err := store.Add(resolution.Path)
	if err != nil {
		return fmt.Errorf("trust project manifest: %w", err)
	}
	if changed {
		if err := trust.Save(store); err != nil {
			return fmt.Errorf("save project trust store: %w", err)
		}
	}
	if changed {
		_, err = fmt.Fprintf(outputWriter(cmd), "Trusted project manifest: %s\nProject root: %s\n", record.ManifestPath, record.ProjectRoot)
	} else {
		_, err = fmt.Fprintf(outputWriter(cmd), "Project manifest already trusted: %s\n", record.ManifestPath)
	}
	return err
}

func runProjectTrustList(cmd *cobra.Command, _ []string) error {
	store, err := trust.Load()
	if err != nil {
		return fmt.Errorf("load project trust store: %w", err)
	}
	jsonOutput, err := projectTrustJSONFlag(cmd)
	if err != nil {
		return fmt.Errorf("read --json flag: %w", err)
	}
	if jsonOutput {
		return writeJSON(outputWriter(cmd), store)
	}
	return printTrustedProjects(outputWriter(cmd), store)
}

func runProjectUntrust(cmd *cobra.Command, _ []string) error {
	manifestPath, err := projectManifestPathForUntrust(cmd)
	if err != nil {
		return err
	}
	store, err := trust.Load()
	if err != nil {
		return fmt.Errorf("load project trust store: %w", err)
	}
	removed, err := store.Remove(manifestPath)
	if err != nil {
		return fmt.Errorf("resolve project trust path: %w", err)
	}
	if removed {
		if err := trust.Save(store); err != nil {
			return fmt.Errorf("save project trust store: %w", err)
		}
		_, err = fmt.Fprintf(outputWriter(cmd), "Untrusted project manifest: %s\n", manifestPath)
		return err
	}
	_, err = fmt.Fprintf(outputWriter(cmd), "Project manifest was not trusted: %s\n", manifestPath)
	return err
}

func runProjectActivate(cmd *cobra.Command, _ []string) error {
	resolution, err := resolveProjectCommandManifest(cmd)
	if err != nil {
		return err
	}
	if _, err := requireTrustedProject(resolution); err != nil {
		return err
	}
	targets, err := preflightProjectActivation(resolution.Manifest)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		_, err := fmt.Fprintf(outputWriter(cmd), "Activated project manifest: %s (0 target shims)\n", resolution.Path)
		return err
	}

	runtimePath, err := shim.EnsureCurrent()
	if err != nil {
		return fmt.Errorf("prepare project shim runtime: %w", err)
	}
	transactions := make([]*install.WrapperUpdateTransaction, 0, len(targets))
	for _, target := range targets {
		transaction, updateErr := install.PrepareShimWrapperUpdate(target.Path, target.App.Repository, runtimePath, target.Target)
		if updateErr != nil {
			return rollbackProjectActivation(transactions, updateErr)
		}
		transactions = append(transactions, transaction)
	}
	for _, transaction := range transactions {
		if err := transaction.Commit(); err != nil {
			return rollbackProjectActivation(transactions, fmt.Errorf("commit project shim %q: %w", transaction.WrapperPath(), err))
		}
	}
	_, err = fmt.Fprintf(outputWriter(cmd), "Activated project manifest: %s (%d target shims)\n", resolution.Path, len(targets))
	return err
}

type projectActivationTarget struct {
	App    state.ManifestApp
	Target string
	Path   string
}

func preflightProjectActivation(file state.Manifest) ([]projectActivationTarget, error) {
	if err := manifestfile.Validate(file); err != nil {
		return nil, fmt.Errorf("validate project manifest: %w", err)
	}
	targets := make([]projectActivationTarget, 0, len(file.Apps))
	seen := make(map[string]string, len(file.Apps))
	for _, app := range file.Apps {
		target := effectiveManifestTarget(app)
		if previous, ok := seen[target]; ok {
			return nil, fmt.Errorf("target %q is declared by both %s and %s", target, previous, app.Repository)
		}
		seen[target] = app.Repository
		path, err := install.TargetPathFor(app.Repository, target)
		if err != nil {
			return nil, fmt.Errorf("resolve target %q for %s: %w", target, app.Repository, err)
		}
		if err := preflightProjectTarget(path); err != nil {
			return nil, err
		}
		targets = append(targets, projectActivationTarget{App: app, Target: target, Path: path})
	}
	return targets, nil
}

func preflightProjectTarget(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect project target %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to overwrite foreign target %q: existing path is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read project target %q: %w", path, err)
	}
	if !wrapper.IsMarkerLine(string(data)) {
		return fmt.Errorf("refusing to overwrite foreign target %q", path)
	}
	return nil
}

func rollbackProjectActivation(transactions []*install.WrapperUpdateTransaction, operationErr error) error {
	var rollbackErr error
	for index := len(transactions) - 1; index >= 0; index-- {
		if err := transactions[index].Rollback(); err != nil && rollbackErr == nil {
			rollbackErr = err
		}
	}
	if rollbackErr != nil {
		return fmt.Errorf("%w (rollback failed: %v)", operationErr, rollbackErr)
	}
	return operationErr
}

func resolveProjectCommandManifest(cmd *cobra.Command) (projectmanifest.Resolution, error) {
	manifestPath, err := getOptionalStringFlag(cmd, "manifest")
	if err != nil {
		return projectmanifest.Resolution{}, fmt.Errorf("read --manifest flag: %w", err)
	}
	return resolveProjectManifest(manifestPath)
}

func projectManifestPathForUntrust(cmd *cobra.Command) (string, error) {
	manifestPath, err := getOptionalStringFlag(cmd, "manifest")
	if err != nil {
		return "", fmt.Errorf("read --manifest flag: %w", err)
	}
	if manifestPath != "" {
		if !isLocalManifestPath(manifestPath) {
			return "", errors.New("manifest path must be a local file")
		}
		record, err := trust.NewRecord(manifestPath)
		if err != nil {
			return "", err
		}
		return record.ManifestPath, nil
	}
	resolution, err := resolveProjectManifest("")
	if err != nil {
		return "", err
	}
	return resolution.Path, nil
}

func requireTrustedProject(resolution projectmanifest.Resolution) (trust.Record, error) {
	store, err := trust.Load()
	if err != nil {
		return trust.Record{}, fmt.Errorf("load project trust store: %w", err)
	}
	record, ok, err := store.Find(resolution.Path)
	if err != nil {
		return trust.Record{}, fmt.Errorf("resolve project trust: %w", err)
	}
	if !ok {
		return trust.Record{}, fmt.Errorf("project manifest %q is not trusted; run zenget project trust --manifest %s", resolution.Path, resolution.Path)
	}
	if err := trust.CheckPathSafety(record); err != nil {
		return trust.Record{}, fmt.Errorf("project manifest trust is no longer safe: %w", err)
	}
	return record, nil
}

func projectTrustJSONFlag(cmd *cobra.Command) (bool, error) {
	if cmd == nil || cmd.Flags().Lookup("json") == nil {
		return projectTrustListJSON, nil
	}
	return cmd.Flags().GetBool("json")
}

func printTrustedProjects(out io.Writer, store trust.Store) error {
	if len(store.Projects) == 0 {
		_, err := fmt.Fprintln(out, "Trusted project manifests: none")
		return err
	}
	if _, err := fmt.Fprintf(out, "Trusted project manifests: %d\n", len(store.Projects)); err != nil {
		return err
	}
	for _, record := range store.Projects {
		if _, err := fmt.Fprintf(out, "- %s (root: %s)\n", record.ManifestPath, record.ProjectRoot); err != nil {
			return err
		}
	}
	return nil
}

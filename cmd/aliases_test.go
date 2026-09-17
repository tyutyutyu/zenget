package cmd

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"zenget/internal/state"
)

func TestCommandAliasesResolveToExistingCommands(t *testing.T) {
	tests := []struct {
		name    string
		full    []string
		aliases [][]string
		want    *cobra.Command
	}{
		{name: "install", full: []string{"install"}, aliases: [][]string{{"i"}}, want: installCmd},
		{name: "uninstall", full: []string{"uninstall"}, aliases: [][]string{{"rm"}, {"un"}}, want: uninstallCmd},
		{name: "upgrade", full: []string{"upgrade"}, aliases: [][]string{{"up"}}, want: upgradeCmd},
		{name: "list", full: []string{"list"}, aliases: [][]string{{"ls"}, {"l"}}, want: listCmd},
		{name: "config", full: []string{"config"}, aliases: [][]string{{"cfg"}}, want: configCmd},
		{name: "registry", full: []string{"registry"}, aliases: [][]string{{"reg"}}, want: registryCmd},
		{name: "project", full: []string{"project"}, aliases: [][]string{{"proj"}}, want: projectCmd},
		{name: "config list", full: []string{"config", "list"}, aliases: [][]string{{"config", "ls"}, {"config", "l"}, {"cfg", "list"}, {"cfg", "ls"}, {"cfg", "l"}}, want: configListCmd},
		{name: "registry list", full: []string{"registry", "list"}, aliases: [][]string{{"registry", "ls"}, {"registry", "l"}, {"reg", "list"}, {"reg", "ls"}, {"reg", "l"}}, want: registryListCmd},
		{name: "cache list", full: []string{"cache", "list"}, aliases: [][]string{{"cache", "ls"}, {"cache", "l"}}, want: cacheListCmd},
		{name: "policy recipe list", full: []string{"policy", "recipe", "list"}, aliases: [][]string{{"policy", "recipe", "ls"}, {"policy", "recipe", "l"}}, want: policyRecipeListCmd},
		{name: "project trust list", full: []string{"project", "trust", "list"}, aliases: [][]string{{"project", "trust", "ls"}, {"project", "trust", "l"}, {"proj", "trust", "list"}, {"proj", "trust", "ls"}, {"proj", "trust", "l"}}, want: projectTrustListCmd},
		{name: "registry remove", full: []string{"registry", "remove"}, aliases: [][]string{{"registry", "rm"}, {"reg", "remove"}, {"reg", "rm"}}, want: registryRemoveCmd},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, aliasPath := range test.aliases {
				fullArgs := append(append([]string{}, test.full...), "argument", "--preserved")
				aliasArgs := append(append([]string{}, aliasPath...), "argument", "--preserved")

				fullCommand, fullRemaining, err := rootCmd.Find(fullArgs)
				if err != nil {
					t.Fatalf("find full command: %v", err)
				}
				aliasCommand, aliasRemaining, err := rootCmd.Find(aliasArgs)
				if err != nil {
					t.Fatalf("find alias %v: %v", aliasPath, err)
				}
				if fullCommand != test.want {
					t.Fatalf("full path resolved to %q, want %q", fullCommand.CommandPath(), test.want.CommandPath())
				}
				if aliasCommand != test.want {
					t.Fatalf("alias path %v resolved to %q, want %q", aliasPath, aliasCommand.CommandPath(), test.want.CommandPath())
				}
				if !reflect.DeepEqual(aliasRemaining, fullRemaining) {
					t.Errorf("alias path %v remaining args = %v, full path remaining args = %v", aliasPath, aliasRemaining, fullRemaining)
				}
			}
		})
	}
}

func TestCommandAliasesHaveNoSiblingCollisions(t *testing.T) {
	var visit func(*cobra.Command)
	visit = func(parent *cobra.Command) {
		seen := make(map[string]string)
		for _, child := range parent.Commands() {
			register := func(name, kind string) {
				if previous, ok := seen[name]; ok {
					t.Errorf("%s has duplicate command token %q (%s and %s)", parent.CommandPath(), name, previous, kind)
					return
				}
				seen[name] = kind
			}

			register(child.Name(), "command "+child.CommandPath())
			for _, alias := range child.Aliases {
				if alias == "" || alias != strings.ToLower(alias) {
					t.Errorf("%s has non-lowercase or empty alias %q", child.CommandPath(), alias)
				}
				register(alias, "alias "+child.CommandPath())
			}
		}
		for _, child := range parent.Commands() {
			visit(child)
		}
	}

	visit(rootCmd)
}

func TestListAliasesProduceIdenticalJSON(t *testing.T) {
	saveFixtureState(t, map[string]state.App{
		"acme/widget": {Version: "v1.0.0"},
	})

	var want string
	for _, command := range []string{"list", "ls", "l"} {
		stdout, stderr, err := executeRootCommand(t, command, "--json")
		if err != nil {
			t.Fatalf("%s --json: %v", command, err)
		}
		if stderr != "" {
			t.Errorf("%s --json stderr = %q", command, stderr)
		}
		var decoded map[string]state.App
		if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
			t.Fatalf("%s --json output is not JSON: %v", command, err)
		}
		if command == "list" {
			want = stdout
			continue
		}
		if stdout != want {
			t.Errorf("%s --json output = %q, list --json output = %q", command, stdout, want)
		}
	}
}

func TestListUpdatesShorthandRemainsAvailable(t *testing.T) {
	flag := listCmd.Flags().ShorthandLookup("u")
	if flag == nil || flag.Name != "updates" {
		t.Fatalf("list -u resolves to %v, want the updates flag", flag)
	}
}

func TestInstallAliasPreservesValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing repository"},
		{name: "invalid repository", args: []string{"not-a-repository"}},
		{name: "unknown flag", args: []string{"acme/widget", "--not-a-real-flag"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fullArgs := append([]string{"install"}, test.args...)
			aliasArgs := append([]string{"i"}, test.args...)
			fullOut, _, fullErr := executeRootCommand(t, fullArgs...)
			aliasOut, _, aliasErr := executeRootCommand(t, aliasArgs...)
			if fullErr == nil || aliasErr == nil {
				t.Fatalf("install error = %v, i error = %v; both should fail", fullErr, aliasErr)
			}
			if fullErr.Error() != aliasErr.Error() {
				t.Errorf("i error = %q, install error = %q", aliasErr, fullErr)
			}
			if commandExitCode(aliasErr) != commandExitCode(fullErr) {
				t.Errorf("i exit code = %d, install exit code = %d", commandExitCode(aliasErr), commandExitCode(fullErr))
			}
			if !bytes.Equal([]byte(aliasOut), []byte(fullOut)) {
				t.Errorf("i stdout = %q, install stdout = %q", aliasOut, fullOut)
			}
		})
	}
}

func TestUnsupportedShortNamesRemainUnknown(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"inst", "u", "c", "I"} {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := executeRootCommand(t, name)
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded", name)
			}
			if commandExitCode(err) == 0 {
				t.Fatalf("%s returned a success exit code", name)
			}
			if stdout != "" {
				t.Errorf("%s wrote operation output %q", name, stdout)
			}
		})
	}
}

func TestAliasHelpUsesNativeCobraAliases(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "install alias", args: []string{"i", "--help"}, want: []string{"Aliases:", "install", "--asset"}},
		{name: "help install alias", args: []string{"help", "i"}, want: []string{"Aliases:", "install", "--asset"}},
		{name: "registry list alias", args: []string{"reg", "ls", "--help"}, want: []string{"Aliases:", "list", "List configured recipe registries"}},
		{name: "project trust list alias", args: []string{"proj", "trust", "ls", "--help"}, want: []string{"Aliases:", "list", "canonical local project manifest paths"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, err := executeRootCommand(t, test.args...)
			if err != nil {
				t.Fatalf("help: %v", err)
			}
			output := stdout + stderr
			for _, want := range test.want {
				if !strings.Contains(output, want) {
					t.Errorf("help output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestInstallAliasCompletionIncludesAssetFlag(t *testing.T) {
	stdout, stderr, err := executeRootCommand(t, "__complete", "i", "--ass")
	if err != nil {
		t.Fatalf("completion: %v; stderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "--asset") {
		t.Fatalf("completion output missing --asset:\n%s", stdout)
	}
}

func executeRootCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	flags := snapshotCommandFlags(rootCmd)
	oldOut := rootCmd.OutOrStdout()
	oldErr := rootCmd.ErrOrStderr()
	oldIn := rootCmd.InOrStdin()
	rootCmd.SetArgs(args)
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetIn(strings.NewReader(""))
	defer func() {
		rootCmd.SetArgs([]string{})
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetIn(oldIn)
		restoreCommandFlags(flags)
	}()

	err := rootCmd.Execute()
	return stdout.String(), stderr.String(), err
}

type flagSnapshot struct {
	flag    *pflag.Flag
	value   string
	changed bool
}

func snapshotCommandFlags(root *cobra.Command) []flagSnapshot {
	var snapshots []flagSnapshot
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		command.Flags().VisitAll(func(flag *pflag.Flag) {
			snapshots = append(snapshots, flagSnapshot{flag: flag, value: flag.Value.String(), changed: flag.Changed})
		})
		command.PersistentFlags().VisitAll(func(flag *pflag.Flag) {
			snapshots = append(snapshots, flagSnapshot{flag: flag, value: flag.Value.String(), changed: flag.Changed})
		})
		for _, child := range command.Commands() {
			visit(child)
		}
	}
	visit(root)
	return snapshots
}

func restoreCommandFlags(snapshots []flagSnapshot) {
	for _, snapshot := range snapshots {
		_ = snapshot.flag.Value.Set(snapshot.value)
		snapshot.flag.Changed = snapshot.changed
	}
}

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zenget/internal/recipepolicy"
)

func TestPolicyRecipeCommandsManageExactRulesIdempotently(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	allowCommand := &cobra.Command{}
	allowCommand.Flags().String("path-prefix", "recipes", "")
	var output bytes.Buffer
	allowCommand.SetOut(&output)
	if err := runPolicyRecipeAllow(allowCommand, []string{"github:Acme/widget"}); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if !strings.Contains(output.String(), "Allowed recipe source: github:Acme/widget/recipes") {
		t.Fatalf("allow output = %q", output.String())
	}
	output.Reset()
	if err := runPolicyRecipeAllow(allowCommand, []string{"github:Acme/widget"}); err != nil {
		t.Fatalf("second allow: %v", err)
	}
	if !strings.Contains(output.String(), "already allowed") {
		t.Fatalf("second allow output = %q", output.String())
	}

	listCommand := &cobra.Command{}
	listCommand.Flags().Bool("json", true, "")
	listCommand.SetOut(&output)
	output.Reset()
	if err := runPolicyRecipeList(listCommand, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	var policy recipepolicy.Policy
	if err := json.Unmarshal(output.Bytes(), &policy); err != nil {
		t.Fatalf("list JSON = %q: %v", output.String(), err)
	}
	if len(policy.Allow) != 1 || policy.Allow[0].PathPrefix != "recipes" {
		t.Fatalf("listed policy = %#v", policy)
	}

	denyCommand := &cobra.Command{}
	denyCommand.Flags().String("path-prefix", "recipes", "")
	denyCommand.SetOut(&output)
	output.Reset()
	if err := runPolicyRecipeDeny(denyCommand, []string{"github:Acme/widget"}); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if !strings.Contains(output.String(), "Denied recipe source") {
		t.Fatalf("deny output = %q", output.String())
	}
	output.Reset()
	if err := runPolicyRecipeDeny(denyCommand, []string{"github:Acme/widget"}); err != nil {
		t.Fatalf("second deny: %v", err)
	}
	if !strings.Contains(output.String(), "already denied") {
		t.Fatalf("second deny output = %q", output.String())
	}
}

func TestPolicyRecipeListHumanAndCheckJSONAreStableAndSideEffectFree(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	policy := recipepolicy.Policy{
		SchemaVersion: recipepolicy.SchemaVersion,
		Allow:         []recipepolicy.Rule{{Host: recipepolicy.GitHubHost, Owner: "acme", Repository: "widget", PathPrefix: "recipes"}},
	}
	if err := recipepolicy.Save(policy); err != nil {
		t.Fatal(err)
	}
	policyPath, err := recipepolicy.Path()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}

	listCommand := &cobra.Command{}
	listCommand.Flags().Bool("json", false, "")
	var human bytes.Buffer
	listCommand.SetOut(&human)
	if err := runPolicyRecipeList(listCommand, nil); err != nil {
		t.Fatalf("human list: %v", err)
	}
	for _, want := range []string{"Recipe source policy", "Allow rules: 1", "github:acme/widget/recipes"} {
		if !strings.Contains(human.String(), want) {
			t.Fatalf("human list missing %q: %s", want, human.String())
		}
	}

	checkCommand := &cobra.Command{}
	checkCommand.Flags().Bool("json", true, "")
	var output bytes.Buffer
	checkCommand.SetOut(&output)
	if err := runPolicyRecipeCheck(checkCommand, []string{"github:acme/widget/recipes/v1/recipe.json"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(output.Bytes(), &check); err != nil {
		t.Fatalf("check JSON = %q: %v", output.String(), err)
	}
	if check["decision"] != "allow" || check["allowed"] != true {
		t.Fatalf("check result = %#v", check)
	}
	if check["source"] != "github:acme/widget/recipes/v1/recipe.json" {
		t.Fatalf("check source = %#v", check["source"])
	}
	after, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("check modified policy file")
	}
}

func TestPolicyRecipeCheckHumanOutputIncludesDecisionRuleAndReason(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := recipepolicy.Save(recipepolicy.Policy{
		SchemaVersion: recipepolicy.SchemaVersion,
		Allow:         []recipepolicy.Rule{{Host: recipepolicy.GitHubHost, Owner: "acme", Repository: "widget"}},
	}); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.Flags().Bool("json", false, "")
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runPolicyRecipeCheck(command, []string{"github:acme/widget"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, want := range []string{"Decision: allow", "Rule: github:acme/widget", "Reason: source matches"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("human check missing %q: %s", want, output.String())
		}
	}
}

func TestPolicyRecipeCheckDeniesInvalidAndUnsupportedSourcesWithoutLeakingSecrets(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, source := range []string{
		"github:acme/widget?token=secret-token",
		"gitlab.com:acme/widget",
		"github:acme/widget/../private",
	} {
		t.Run(source, func(t *testing.T) {
			command := &cobra.Command{}
			command.Flags().Bool("json", true, "")
			var output bytes.Buffer
			command.SetOut(&output)
			if err := runPolicyRecipeCheck(command, []string{source}); err != nil {
				t.Fatalf("check: %v", err)
			}
			if strings.Contains(output.String(), "secret-token") || !strings.Contains(output.String(), `"decision": "deny"`) {
				t.Fatalf("check output = %q", output.String())
			}
		})
	}
}

func TestPolicyRecipeCommandsRejectUnsafeManagementInputs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	command := &cobra.Command{}
	command.Flags().String("path-prefix", "", "")
	for _, source := range []string{
		"gitlab.com:acme/widget",
		"github:acme/widget/recipes",
		"https://github.com/acme/widget",
	} {
		if err := runPolicyRecipeAllow(command, []string{source}); err == nil {
			t.Fatalf("allow(%q) error = nil", source)
		}
	}

	policyPath, err := recipepolicy.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(policyPath)); !os.IsNotExist(err) {
		t.Fatalf("invalid allow input created policy directory: %v", err)
	}
}

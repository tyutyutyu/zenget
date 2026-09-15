package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"zenget/internal/recipepolicy"
)

var (
	policyRecipeAllowPathPrefix string
	policyRecipeDenyPathPrefix  string
	policyRecipeListJSON        bool
	policyRecipeCheckJSON       bool
)

var policyCmd = &cobra.Command{
	Use:   "policy",
	Short: "Manage zenget security policies",
}

var policyRecipeCmd = &cobra.Command{
	Use:   "recipe",
	Short: "Manage remote recipe-source trust",
}

var policyRecipeAllowCmd = &cobra.Command{
	Use:   "allow <github:owner/repository>",
	Short: "Allow one exact remote recipe source",
	Args:  cobra.ExactArgs(1),
	RunE:  runPolicyRecipeAllow,
}

var policyRecipeDenyCmd = &cobra.Command{
	Use:   "deny <github:owner/repository>",
	Short: "Remove one exact remote recipe allow rule",
	Args:  cobra.ExactArgs(1),
	RunE:  runPolicyRecipeDeny,
}

var policyRecipeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List allowed remote recipe sources",
	Args:  cobra.NoArgs,
	RunE:  runPolicyRecipeList,
}

var policyRecipeCheckCmd = &cobra.Command{
	Use:   "check <source>",
	Short: "Check a remote recipe source without side effects",
	Args:  cobra.ExactArgs(1),
	RunE:  runPolicyRecipeCheck,
}

func init() {
	policyRecipeAllowCmd.Flags().StringVar(&policyRecipeAllowPathPrefix, "path-prefix", "", "allow this clean repository-relative path and its descendants")
	policyRecipeDenyCmd.Flags().StringVar(&policyRecipeDenyPathPrefix, "path-prefix", "", "remove the rule for this clean repository-relative path")
	policyRecipeListCmd.Flags().BoolVar(&policyRecipeListJSON, "json", false, "print the policy as JSON")
	policyRecipeCheckCmd.Flags().BoolVar(&policyRecipeCheckJSON, "json", false, "print the decision as JSON")
	policyRecipeCmd.AddCommand(policyRecipeAllowCmd, policyRecipeDenyCmd, policyRecipeListCmd, policyRecipeCheckCmd)
	policyCmd.AddCommand(policyRecipeCmd)
	rootCmd.AddCommand(policyCmd)
}

func runPolicyRecipeAllow(cmd *cobra.Command, args []string) error {
	source, err := parsePolicyRepository(args[0])
	if err != nil {
		return err
	}
	pathPrefix, err := policyPathPrefix(cmd, policyRecipeAllowPathPrefix)
	if err != nil {
		return fmt.Errorf("read --path-prefix flag: %w", err)
	}
	source.Path = pathPrefix
	rule, err := recipepolicy.NewRule(source)
	if err != nil {
		return err
	}

	policy, err := recipepolicy.Load()
	if err != nil {
		return fmt.Errorf("load recipe source policy: %w", err)
	}
	changed, err := policy.Add(rule)
	if err != nil {
		return err
	}
	if changed {
		if err := recipepolicy.Save(policy); err != nil {
			return fmt.Errorf("save recipe source policy: %w", err)
		}
		_, err = fmt.Fprintf(outputWriter(cmd), "Allowed recipe source: %s\n", rule.String())
		return err
	}
	_, err = fmt.Fprintf(outputWriter(cmd), "Recipe source already allowed: %s\n", rule.String())
	return err
}

func runPolicyRecipeDeny(cmd *cobra.Command, args []string) error {
	source, err := parsePolicyRepository(args[0])
	if err != nil {
		return err
	}
	pathPrefix, err := policyPathPrefix(cmd, policyRecipeDenyPathPrefix)
	if err != nil {
		return fmt.Errorf("read --path-prefix flag: %w", err)
	}
	source.Path = pathPrefix
	rule, err := recipepolicy.NewRule(source)
	if err != nil {
		return err
	}

	policy, err := recipepolicy.Load()
	if err != nil {
		return fmt.Errorf("load recipe source policy: %w", err)
	}
	changed, err := policy.Remove(rule)
	if err != nil {
		return err
	}
	if changed {
		if err := recipepolicy.Save(policy); err != nil {
			return fmt.Errorf("save recipe source policy: %w", err)
		}
		_, err = fmt.Fprintf(outputWriter(cmd), "Denied recipe source: %s\n", rule.String())
		return err
	}
	_, err = fmt.Fprintf(outputWriter(cmd), "Recipe source already denied: %s\n", rule.String())
	return err
}

func runPolicyRecipeList(cmd *cobra.Command, _ []string) error {
	policy, err := recipepolicy.Load()
	if err != nil {
		return fmt.Errorf("load recipe source policy: %w", err)
	}
	jsonOutput, err := policyJSONFlag(cmd, policyRecipeListJSON)
	if err != nil {
		return fmt.Errorf("read --json flag: %w", err)
	}
	if jsonOutput {
		return writeJSON(outputWriter(cmd), policy)
	}
	return printRecipePolicy(outputWriter(cmd), policy)
}

func runPolicyRecipeCheck(cmd *cobra.Command, args []string) error {
	jsonOutput, err := policyJSONFlag(cmd, policyRecipeCheckJSON)
	if err != nil {
		return fmt.Errorf("read --json flag: %w", err)
	}

	source, parseErr := recipepolicy.ParseSource(args[0])
	result := recipepolicy.CheckResult{Reason: "invalid recipe source"}
	sourceText := "<invalid-source>"
	if parseErr == nil {
		sourceText = source.String()
		result = recipepolicy.CheckSource(source)
	} else {
		result.Reason = parseErr.Error()
	}
	output := recipePolicyCheckOutput{
		Source:   sourceText,
		Decision: decisionName(result.Allowed),
		Allowed:  result.Allowed,
		Rule:     result.Rule,
		Reason:   result.Reason,
	}
	if jsonOutput {
		return writeJSON(outputWriter(cmd), output)
	}
	return printRecipePolicyCheck(outputWriter(cmd), output)
}

type recipePolicyCheckOutput struct {
	Source   string             `json:"source"`
	Decision string             `json:"decision"`
	Allowed  bool               `json:"allowed"`
	Rule     *recipepolicy.Rule `json:"rule"`
	Reason   string             `json:"reason"`
}

func parsePolicyRepository(value string) (recipepolicy.SourceIdentity, error) {
	source, err := recipepolicy.ParseSource(value)
	if err != nil {
		return recipepolicy.SourceIdentity{}, fmt.Errorf("parse recipe source: %w", err)
	}
	if source.Path != "" {
		return recipepolicy.SourceIdentity{}, fmt.Errorf("recipe source must be only github:owner/repository; use --path-prefix for a path")
	}
	if source.Host != recipepolicy.GitHubHost {
		return recipepolicy.SourceIdentity{}, fmt.Errorf("recipe source host is not supported; only github.com is allowed")
	}
	return source, nil
}

func policyPathPrefix(cmd *cobra.Command, fallback string) (string, error) {
	if cmd == nil || cmd.Flags().Lookup("path-prefix") == nil {
		return fallback, nil
	}
	return cmd.Flags().GetString("path-prefix")
}

func policyJSONFlag(cmd *cobra.Command, fallback bool) (bool, error) {
	if cmd == nil || cmd.Flags().Lookup("json") == nil {
		return fallback, nil
	}
	return cmd.Flags().GetBool("json")
}

func outputWriter(cmd *cobra.Command) io.Writer {
	if cmd == nil {
		return io.Discard
	}
	writer := cmd.OutOrStdout()
	if writer == nil {
		return io.Discard
	}
	return writer
}

func printRecipePolicy(out io.Writer, policy recipepolicy.Policy) error {
	if _, err := fmt.Fprintf(out, "Recipe source policy (schema_version=%d)\n", policy.SchemaVersion); err != nil {
		return err
	}
	if len(policy.Allow) == 0 {
		_, err := fmt.Fprintln(out, "Allow rules: none (remote recipes are denied)")
		return err
	}
	if _, err := fmt.Fprintf(out, "Allow rules: %d\n", len(policy.Allow)); err != nil {
		return err
	}
	for _, rule := range policy.Allow {
		if _, err := fmt.Fprintf(out, "- %s\n", rule.String()); err != nil {
			return err
		}
	}
	return nil
}

func printRecipePolicyCheck(out io.Writer, result recipePolicyCheckOutput) error {
	if _, err := fmt.Fprintf(out, "Decision: %s\nSource: %s\n", result.Decision, result.Source); err != nil {
		return err
	}
	if result.Rule == nil {
		if _, err := fmt.Fprintln(out, "Rule: none"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(out, "Rule: %s\n", result.Rule.String()); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "Reason: %s\n", result.Reason)
	return err
}

func decisionName(allowed bool) string {
	if allowed {
		return "allow"
	}
	return "deny"
}

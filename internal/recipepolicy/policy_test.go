package recipepolicy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"zenget/internal/limits"
)

func TestParseSourceCanonicalizesProviderAndPath(t *testing.T) {
	source, err := ParseSource("GITHUB:Acme/Widget/recipes//v1/")
	if err != nil {
		t.Fatalf("ParseSource() error = %v", err)
	}
	if source.Host != GitHubHost || source.Owner != "Acme" || source.Repository != "Widget" || source.Path != "recipes/v1" {
		t.Fatalf("source = %#v, want canonical GitHub identity", source)
	}
	if got, want := source.String(), "github:Acme/Widget/recipes/v1"; got != want {
		t.Fatalf("source.String() = %q, want %q", got, want)
	}
}

func TestParseSourceRejectsAmbiguousOrPatternInputs(t *testing.T) {
	invalid := []string{
		"",
		"github:owner",
		"github:/repo/tool",
		"https://github.com/owner/repo",
		"github:owner/repo?token=secret",
		"github:owner/repo#fragment",
		"github:owner/repo/%2e%2e/secret",
		"github:owner/repo/../secret",
		"github:owner/repo/./recipe.json",
		"github:owner/repo\\recipe.json",
		"github:owner/repo/*.json",
		"github:owner/repo/[a-z].json",
		"github:owner/repo/élite",
	}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseSource(input); err == nil {
				t.Fatal("ParseSource() error = nil")
			} else if strings.Contains(err.Error(), "secret") {
				t.Fatalf("ParseSource() leaked sensitive input: %v", err)
			}
		})
	}
}

func TestRuleAndPolicyRejectUnsupportedHostsAndPatterns(t *testing.T) {
	for _, rule := range []Rule{
		{Host: "gitlab.com", Owner: "acme", Repository: "widget"},
		{Host: "github.com", Owner: "acme/*", Repository: "widget"},
		{Host: "github.com", Owner: "acme", Repository: "widget", PathPrefix: "recipes/../private"},
	} {
		if _, err := rule.Normalize(); err == nil {
			t.Fatalf("Rule.Normalize(%#v) error = nil", rule)
		}
	}
}

func TestPolicyCheckUsesExactRepositoryAndPathBoundaries(t *testing.T) {
	policy := Policy{
		SchemaVersion: SchemaVersion,
		Allow:         []Rule{{Host: GitHubHost, Owner: "acme", Repository: "widget", PathPrefix: "recipes"}},
	}
	tests := []struct {
		name    string
		source  SourceIdentity
		allowed bool
		reason  string
	}{
		{name: "prefix root", source: SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "widget", Path: "recipes"}, allowed: true},
		{name: "prefix descendant", source: SourceIdentity{Host: "github", Owner: "acme", Repository: "widget", Path: "recipes/v1/recipe.json"}, allowed: true},
		{name: "prefix boundary", source: SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "widget", Path: "recipes-old/recipe.json"}, reason: "does not match"},
		{name: "repository case", source: SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "Widget", Path: "recipes"}, reason: "does not match"},
		{name: "owner case", source: SourceIdentity{Host: GitHubHost, Owner: "Acme", Repository: "widget", Path: "recipes"}, reason: "does not match"},
		{name: "unsupported host", source: SourceIdentity{Host: "gitlab.com", Owner: "acme", Repository: "widget", Path: "recipes"}, reason: "host is not allowed"},
		{name: "invalid path", source: SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "widget", Path: "recipes/../private"}, reason: "invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := policy.Check(test.source)
			if result.Allowed != test.allowed {
				t.Fatalf("Allowed = %t, want %t; result = %#v", result.Allowed, test.allowed, result)
			}
			if test.reason != "" && !strings.Contains(result.Reason, test.reason) {
				t.Fatalf("Reason = %q, want substring %q", result.Reason, test.reason)
			}
			if test.allowed && result.Rule == nil {
				t.Fatal("allowed result has nil Rule")
			}
		})
	}
}

func TestPolicyDefaultDenyAndInvalidPolicyDeny(t *testing.T) {
	source := SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "widget"}
	if result := Default().Check(source); result.Allowed {
		t.Fatal("Default().Check() allowed an empty policy")
	}
	if result := (Policy{SchemaVersion: 2}).Check(source); result.Allowed {
		t.Fatal("invalid policy was allowed")
	}
	if result := (Policy{SchemaVersion: SchemaVersion, Allow: []Rule{{Host: GitHubHost, Owner: "acme", Repository: "widget", PathPrefix: "*.json"}}}).Check(source); result.Allowed {
		t.Fatal("invalid rule was allowed")
	}
}

func TestPolicyNormalizeSortsAndAddRemoveAreIdempotent(t *testing.T) {
	policy := Policy{
		SchemaVersion: SchemaVersion,
		Allow: []Rule{
			{Host: GitHubHost, Owner: "zeta", Repository: "tool"},
			{Host: "github", Owner: "acme", Repository: "widget", PathPrefix: "recipes//v1"},
		},
	}
	normalized, err := policy.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got, want := normalized.Allow[0].String(), "github:acme/widget/recipes/v1"; got != want {
		t.Fatalf("first rule = %q, want %q", got, want)
	}
	newRule := Rule{Host: GitHubHost, Owner: "acme", Repository: "widget"}
	changed, err := normalized.Add(newRule)
	if err != nil || !changed {
		t.Fatalf("Add() = (%t, %v), want changed", changed, err)
	}
	changed, err = normalized.Add(newRule)
	if err != nil || changed {
		t.Fatalf("second Add() = (%t, %v), want unchanged", changed, err)
	}
	changed, err = normalized.Remove(newRule)
	if err != nil || !changed {
		t.Fatalf("Remove() = (%t, %v), want changed", changed, err)
	}
	changed, err = normalized.Remove(newRule)
	if err != nil || changed {
		t.Fatalf("second Remove() = (%t, %v), want unchanged", changed, err)
	}
}

func TestPolicyMarshalIsDeterministicAndStrictDecode(t *testing.T) {
	policy := Policy{
		SchemaVersion: SchemaVersion,
		Allow: []Rule{
			{Host: GitHubHost, Owner: "zeta", Repository: "tool"},
			{Host: GitHubHost, Owner: "acme", Repository: "widget", PathPrefix: "recipes"},
		},
	}
	first, err := Marshal(policy)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	second, err := Marshal(policy)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("Marshal() is not deterministic: %q vs %q; error = %v", first, second, err)
	}
	loaded, err := Decode(first, "policy.json")
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !bytes.Equal(first, mustMarshal(t, loaded)) {
		t.Fatal("Decode/Marshal changed canonical policy")
	}

	invalid := map[string]string{
		"missing schema":  `{"allow":[]}`,
		"missing allow":   `{"schema_version":1}`,
		"unknown field":   `{"schema_version":1,"allow":[],"secret":"value"}`,
		"duplicate key":   `{"schema_version":1,"allow":[],"allow":[]}`,
		"multiple values": `{"schema_version":1,"allow":[]} {}`,
		"wrong schema":    `{"schema_version":2,"allow":[]}`,
		"null allow":      `{"schema_version":1,"allow":null}`,
	}
	for name, data := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(data), "policy.json"); err == nil {
				t.Fatal("Decode() error = nil")
			}
		})
	}
}

func TestPolicyLoadMissingFileIsDefaultDenyAndSaveUsesSecureAtomicFile(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() missing file error = %v", err)
	}
	if result := loaded.Check(SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "widget"}); result.Allowed {
		t.Fatal("missing policy allowed a source")
	}

	policy := Policy{SchemaVersion: SchemaVersion, Allow: []Rule{{Host: GitHubHost, Owner: "acme", Repository: "widget"}}}
	if err := Save(policy); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	policyPath, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("saved policy is a symlink: %v", info.Mode())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("saved policy mode = %v, want regular 0600 file", info.Mode())
	}
	loaded, err = Load()
	if err != nil || !loaded.Check(SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "widget"}).Allowed {
		t.Fatalf("saved policy load/check = %#v, %v; want allowed", loaded, err)
	}
	if leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(policyPath), ".zenget-recipe-policy-*")); err != nil {
		t.Fatal(err)
	} else if len(leftovers) != 0 {
		t.Fatalf("temporary policy files remain: %v", leftovers)
	}
}

func TestPolicyLoadRejectsUnsafeFileAndDoesNotRepairIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode-bit rejection is covered on Unix; Windows ACL rejection has dedicated tests")
	}
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	policyPath, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(policyPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, []byte(`{"schema_version":1,"allow":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("Load() error = %v, want permissions error", err)
	}
	if err := Save(Default()); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("Save() error = %v, want permissions error", err)
	}
	after, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unsafe policy file was repaired or replaced")
	}

	if err := os.Remove(policyPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(configHome, "target.json")
	if err := os.WriteFile(target, []byte(`{"schema_version":1,"allow":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, policyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Load() symlink error = %v, want symlink error", err)
	}
	if err := Save(Default()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Save() symlink error = %v, want symlink error", err)
	}
}

func TestPolicyInputLimitAndDiagnosticsDoNotLeakURLQuery(t *testing.T) {
	data := bytes.Repeat([]byte("x"), int(limits.DefaultStructuredBytes)+1)
	_, err := Decode(data, "https://example.invalid/recipe-policy.json?token=secret-token")
	if !errors.Is(err, limits.ErrLimitExceeded) {
		t.Fatalf("Decode() error = %v, want limit error", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("Decode() leaked sensitive input: %v", err)
	}

	result := CheckSource(SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "widget"})
	if result.Allowed {
		t.Fatal("missing policy unexpectedly allowed source")
	}
}

func TestCheckSourceReloadsPolicyAfterModification(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	source := SourceIdentity{Host: GitHubHost, Owner: "acme", Repository: "widget"}
	allowed := Policy{SchemaVersion: SchemaVersion, Allow: []Rule{{Host: GitHubHost, Owner: "acme", Repository: "widget"}}}
	if err := Save(allowed); err != nil {
		t.Fatal(err)
	}
	if result := CheckSource(source); !result.Allowed {
		t.Fatalf("initial CheckSource() = %#v, want allow", result)
	}
	if err := Save(Default()); err != nil {
		t.Fatal(err)
	}
	if result := CheckSource(source); result.Allowed {
		t.Fatalf("CheckSource() after policy change = %#v, want deny", result)
	}
}

func mustMarshal(t *testing.T, policy Policy) []byte {
	t.Helper()
	data, err := Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

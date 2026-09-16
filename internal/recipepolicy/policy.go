// Package recipepolicy controls which remote recipe sources zenget may use.
//
// The policy is deliberately a small, data-only allowlist. A missing policy,
// an empty allowlist, and every policy error result in a default-deny decision.
package recipepolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"zenget/internal/fileowner"
	"zenget/internal/limits"
)

const (
	// SchemaVersion is the current recipe-source policy schema.
	SchemaVersion = 1
	// PolicyFileName is the basename of the persistent recipe-source policy.
	PolicyFileName = "recipe-policy.json"
	// GitHubHost is the only remote host supported by this policy version.
	GitHubHost = "github.com"
)

// SourceIdentity is the complete identity of a recipe source. Path is a
// repository-relative recipe path or prefix, not a filesystem path and not a
// URL. Owner and Repository retain GitHub's case-sensitive spelling.
type SourceIdentity struct {
	Host       string
	Owner      string
	Repository string
	Path       string
}

// Source is a short alias for SourceIdentity.
type Source = SourceIdentity

// Rule is one exact remote recipe-source allow rule. An empty PathPrefix
// allows every path in the named repository; a non-empty prefix allows that
// path and its descendants only.
type Rule struct {
	Host       string `json:"host"`
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	PathPrefix string `json:"path_prefix,omitempty"`
}

// AllowRule is an alternate name for Rule used by callers that prefer the
// policy terminology.
type AllowRule = Rule

// Policy is the versioned recipe-source allowlist. The Allow slice is always
// canonicalized into deterministic order before it is persisted.
type Policy struct {
	SchemaVersion int    `json:"schema_version"`
	Allow         []Rule `json:"allow"`
}

// CheckResult is the side-effect-free result of Policy.Check.
type CheckResult struct {
	Allowed bool   `json:"allowed"`
	Rule    *Rule  `json:"rule,omitempty"`
	Reason  string `json:"reason"`
}

// Decision is an alternate name for CheckResult.
type Decision = CheckResult

// Default returns the empty, schema-versioned default-deny policy.
func Default() Policy {
	return Policy{SchemaVersion: SchemaVersion, Allow: []Rule{}}
}

// DefaultPolicy is an alternate name for Default.
func DefaultPolicy() Policy {
	return Default()
}

// Path returns the persistent recipe-source policy path.
func Path() (string, error) {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "zenget", PolicyFileName), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}
	return filepath.Join(home, ".config", "zenget", PolicyFileName), nil
}

// ParseSource parses the CLI source notation
// "github:owner/repository[/relative/path]". The short github provider name
// and the canonical github.com host are both accepted and canonicalized to
// github.com. Other syntactically valid hosts are retained so Check can return
// an explicit deny decision for them; they cannot be used in a rule.
func ParseSource(value string) (SourceIdentity, error) {
	if value == "" || strings.ContainsAny(value, "?#%\\\x00") {
		return SourceIdentity{}, errorsForSource("source must be a clean provider identity")
	}

	host, remainder, ok := strings.Cut(value, ":")
	if !ok || !validHost(host) || remainder == "" || strings.HasPrefix(remainder, "/") {
		return SourceIdentity{}, errorsForSource("source must use provider:owner/repository syntax")
	}
	parts := strings.Split(remainder, "/")
	if len(parts) < 2 || !validRepositoryPart(parts[0]) || !validRepositoryPart(parts[1]) {
		return SourceIdentity{}, errorsForSource("source must identify one owner and repository")
	}

	pathValue := ""
	if len(parts) > 2 {
		pathValue = strings.Join(parts[2:], "/")
	}
	cleanPath, err := normalizeRelativePath(pathValue, true)
	if err != nil {
		return SourceIdentity{}, err
	}

	return SourceIdentity{
		Host:       canonicalHost(host),
		Owner:      parts[0],
		Repository: parts[1],
		Path:       cleanPath,
	}, nil
}

// ParseSourceIdentity is an alternate name for ParseSource.
func ParseSourceIdentity(value string) (SourceIdentity, error) {
	return ParseSource(value)
}

// NewSourceIdentity creates and validates a complete source identity.
func NewSourceIdentity(host, owner, repository, sourcePath string) (SourceIdentity, error) {
	source := SourceIdentity{Host: host, Owner: owner, Repository: repository, Path: sourcePath}
	return source.Normalize()
}

// Normalize validates and canonicalizes a source identity.
func (s SourceIdentity) Normalize() (SourceIdentity, error) {
	if !validHost(s.Host) {
		return SourceIdentity{}, errorsForSource("source host is invalid")
	}
	if !validRepositoryPart(s.Owner) || !validRepositoryPart(s.Repository) {
		return SourceIdentity{}, errorsForSource("source owner or repository is invalid")
	}
	cleanPath, err := normalizeRelativePath(s.Path, true)
	if err != nil {
		return SourceIdentity{}, err
	}
	s.Host = canonicalHost(s.Host)
	s.Path = cleanPath
	return s, nil
}

// String returns the canonical source notation without query or fragment
// data. It is safe for diagnostics because a SourceIdentity has already
// passed validation.
func (s SourceIdentity) String() string {
	normalized, err := s.Normalize()
	if err != nil {
		return "<invalid-source>"
	}
	host := normalized.Host
	if host == GitHubHost {
		host = "github"
	}
	value := host + ":" + normalized.Owner + "/" + normalized.Repository
	if normalized.Path != "" {
		value += "/" + normalized.Path
	}
	return value
}

// Normalize validates and canonicalizes one allow rule.
func (r Rule) Normalize() (Rule, error) {
	source, err := SourceIdentity{
		Host:       r.Host,
		Owner:      r.Owner,
		Repository: r.Repository,
		Path:       r.PathPrefix,
	}.Normalize()
	if err != nil {
		return Rule{}, fmt.Errorf("invalid allow rule: %w", err)
	}
	if source.Host != GitHubHost {
		return Rule{}, errors.New("invalid allow rule: only github.com is supported")
	}
	return Rule{
		Host:       source.Host,
		Owner:      source.Owner,
		Repository: source.Repository,
		PathPrefix: source.Path,
	}, nil
}

// NewRule creates an exact allow rule from a complete source identity. The
// source path becomes the rule's optional path prefix.
func NewRule(source SourceIdentity) (Rule, error) {
	rule := Rule{
		Host:       source.Host,
		Owner:      source.Owner,
		Repository: source.Repository,
		PathPrefix: source.Path,
	}
	return rule.Normalize()
}

// String returns a stable human-readable rule identity.
func (r Rule) String() string {
	normalized, err := r.Normalize()
	if err != nil {
		return "<invalid-rule>"
	}
	return SourceIdentity{
		Host:       normalized.Host,
		Owner:      normalized.Owner,
		Repository: normalized.Repository,
		Path:       normalized.PathPrefix,
	}.String()
}

// Source returns the source identity represented by a rule.
func (r Rule) Source() SourceIdentity {
	return SourceIdentity{
		Host:       r.Host,
		Owner:      r.Owner,
		Repository: r.Repository,
		Path:       r.PathPrefix,
	}
}

// Normalize validates a policy and returns its canonical sorted copy. A nil
// Allow slice is represented as an empty array in the returned policy.
func (p Policy) Normalize() (Policy, error) {
	if p.SchemaVersion != SchemaVersion {
		return Policy{}, fmt.Errorf("unsupported recipe policy schema_version %d; want %d", p.SchemaVersion, SchemaVersion)
	}

	normalized := Policy{SchemaVersion: SchemaVersion, Allow: make([]Rule, 0, len(p.Allow))}
	seen := make(map[string]struct{}, len(p.Allow))
	for _, rule := range p.Allow {
		canonical, err := rule.Normalize()
		if err != nil {
			return Policy{}, err
		}
		key := ruleKey(canonical)
		if _, exists := seen[key]; exists {
			return Policy{}, errors.New("recipe policy contains a duplicate allow rule")
		}
		seen[key] = struct{}{}
		normalized.Allow = append(normalized.Allow, canonical)
	}
	sort.Slice(normalized.Allow, func(left, right int) bool {
		return ruleKey(normalized.Allow[left]) < ruleKey(normalized.Allow[right])
	})
	return normalized, nil
}

// Validate checks the policy without accessing the filesystem or network.
func (p Policy) Validate() error {
	_, err := p.Normalize()
	return err
}

// Marshal returns deterministic, indented policy JSON with a trailing newline.
func Marshal(p Policy) ([]byte, error) {
	normalized, err := p.Normalize()
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal recipe policy: %w", err)
	}
	return append(data, '\n'), nil
}

// Decode strictly validates policy JSON supplied by a caller that has already
// read the local input.
func Decode(data []byte, input string) (Policy, error) {
	return decode(data, input)
}

// Add inserts one rule if it is not already present. It returns whether the
// policy changed.
func (p *Policy) Add(rule Rule) (bool, error) {
	if p == nil {
		return false, errors.New("recipe policy is nil")
	}
	normalized, err := p.Normalize()
	if err != nil {
		return false, err
	}
	canonical, err := rule.Normalize()
	if err != nil {
		return false, err
	}
	key := ruleKey(canonical)
	for _, existing := range normalized.Allow {
		if ruleKey(existing) == key {
			*p = normalized
			return false, nil
		}
	}
	normalized.Allow = append(normalized.Allow, canonical)
	normalized, err = normalized.Normalize()
	if err != nil {
		return false, err
	}
	*p = normalized
	return true, nil
}

// Remove deletes one exact rule. It returns whether the policy changed.
func (p *Policy) Remove(rule Rule) (bool, error) {
	if p == nil {
		return false, errors.New("recipe policy is nil")
	}
	normalized, err := p.Normalize()
	if err != nil {
		return false, err
	}
	canonical, err := rule.Normalize()
	if err != nil {
		return false, err
	}
	key := ruleKey(canonical)
	for index, existing := range normalized.Allow {
		if ruleKey(existing) != key {
			continue
		}
		normalized.Allow = append(normalized.Allow[:index], normalized.Allow[index+1:]...)
		*p = normalized
		return true, nil
	}
	*p = normalized
	return false, nil
}

// Check applies the exact allowlist to a complete source identity. It never
// accesses the network, filesystem, or a cache. Invalid policies and source
// identities are denied rather than being converted into an allow decision.
func (p Policy) Check(source SourceIdentity) CheckResult {
	canonicalSource, err := source.Normalize()
	if err != nil {
		return CheckResult{Reason: "source identity is invalid"}
	}
	canonicalPolicy, err := p.Normalize()
	if err != nil {
		return CheckResult{Reason: "recipe source policy is invalid"}
	}
	if canonicalSource.Host != GitHubHost {
		return CheckResult{Reason: "source host is not allowed"}
	}

	for index := range canonicalPolicy.Allow {
		rule := canonicalPolicy.Allow[index]
		if rule.Owner != canonicalSource.Owner || rule.Repository != canonicalSource.Repository {
			continue
		}
		if !pathMatches(rule.PathPrefix, canonicalSource.Path) {
			continue
		}
		matched := rule
		return CheckResult{
			Allowed: true,
			Rule:    &matched,
			Reason:  "source matches an allow rule",
		}
	}
	return CheckResult{Reason: "source does not match an allow rule"}
}

// CheckSource loads the current policy and checks one source. Policy read or
// validation failures are intentionally converted to a deny result so a
// caller cannot accidentally continue with an unavailable policy.
func CheckSource(source SourceIdentity) CheckResult {
	policy, err := Load()
	if err != nil {
		return CheckResult{Reason: "recipe source policy is unavailable"}
	}
	return policy.Check(source)
}

// Load reads and strictly validates the persistent policy. A missing file is
// the empty default-deny policy. Unsafe files and parse errors are returned to
// management commands and are converted to deny by CheckSource.
func Load() (Policy, error) {
	policyPath, err := Path()
	if err != nil {
		return Policy{}, err
	}

	linkInfo, err := os.Lstat(policyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return Policy{}, fmt.Errorf("inspect recipe policy %q: %w", policyPath, err)
	}
	if err := validatePolicyFile(policyPath, linkInfo); err != nil {
		return Policy{}, err
	}

	file, err := os.Open(policyPath)
	if err != nil {
		return Policy{}, fmt.Errorf("open recipe policy %q: %w", policyPath, err)
	}
	defer func() { _ = file.Close() }()
	data, err := limits.ReadAll(file, limits.DefaultStructuredBytes, "recipe policy", policyPath)
	if err != nil {
		return Policy{}, fmt.Errorf("read recipe policy %q: %w", policyPath, err)
	}
	return decode(data, policyPath)
}

// Save validates and atomically writes the persistent policy with mode 0600.
// Existing policy files must already be regular, current-user-owned, and no
// more permissive than 0600; an unsafe file is never silently repaired.
func Save(p Policy) error {
	normalized, err := p.Normalize()
	if err != nil {
		return err
	}
	policyPath, err := Path()
	if err != nil {
		return err
	}
	directory := filepath.Dir(policyPath)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create recipe policy directory %q: %w", directory, err)
	}
	if err := fileowner.SecurePath(directory, true); err != nil {
		return fmt.Errorf("secure recipe policy directory %q: %w", directory, err)
	}
	if info, statErr := os.Lstat(policyPath); statErr == nil {
		if err := validatePolicyFile(policyPath, info); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect recipe policy %q: %w", policyPath, statErr)
	}

	data, err := Marshal(normalized)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".zenget-recipe-policy-*")
	if err != nil {
		return fmt.Errorf("create temporary recipe policy in %q: %w", directory, err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set recipe policy permissions: %w", err)
	}
	if err := fileowner.SecurePath(temporaryPath, false); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary recipe policy: %w", err)
	}
	if n, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary recipe policy: %w", err)
	} else if n != len(data) {
		_ = temporary.Close()
		return fmt.Errorf("write temporary recipe policy: %w", io.ErrShortWrite)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary recipe policy: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary recipe policy: %w", err)
	}

	// Re-check the destination immediately before replacement so a symlink or
	// unsafe file created while the temporary file was written is rejected.
	if info, statErr := os.Lstat(policyPath); statErr == nil {
		if err := validatePolicyFile(policyPath, info); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect recipe policy %q: %w", policyPath, statErr)
	}
	if err := os.Rename(temporaryPath, policyPath); err != nil {
		return fmt.Errorf("replace recipe policy %q: %w", policyPath, err)
	}
	removeTemporary = false
	return nil
}

func decode(data []byte, input string) (Policy, error) {
	var checked json.RawMessage
	if err := limits.DecodeJSON(bytes.NewReader(data), &checked, limits.DefaultStructuredBytes, "recipe policy", input); err != nil {
		return Policy{}, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("decode recipe policy: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Policy{}, errors.New("decode recipe policy: multiple JSON values")
		}
		return Policy{}, fmt.Errorf("decode recipe policy: trailing data: %w", err)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return Policy{}, fmt.Errorf("decode recipe policy: %w", err)
	}
	if object == nil {
		return Policy{}, errors.New("recipe policy must be a JSON object")
	}
	if _, ok := object["schema_version"]; !ok {
		return Policy{}, errors.New("recipe policy is missing schema_version")
	}
	allowValue, ok := object["allow"]
	if !ok {
		return Policy{}, errors.New("recipe policy is missing allow")
	}
	if bytes.Equal(bytes.TrimSpace(allowValue), []byte("null")) {
		return Policy{}, errors.New("recipe policy allow must be an array")
	}

	normalized, err := policy.Normalize()
	if err != nil {
		return Policy{}, err
	}
	return normalized, nil
}

func validatePolicyFile(policyPath string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to use recipe policy through symlink %q", policyPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("recipe policy %q is not a regular file", policyPath)
	}
	if !fileowner.PrivateFileModeSafe(info) {
		return fmt.Errorf("recipe policy %q permissions must be no more permissive than 0600", policyPath)
	}
	if !fileowner.CurrentUserOwnsPath(policyPath, info) {
		return fmt.Errorf("recipe policy %q is not owned by the current user", policyPath)
	}
	return nil
}

func pathMatches(prefix, sourcePath string) bool {
	if prefix == "" {
		return true
	}
	return sourcePath == prefix || strings.HasPrefix(sourcePath, prefix+"/")
}

func ruleKey(rule Rule) string {
	return rule.Host + "\x00" + rule.Owner + "\x00" + rule.Repository + "\x00" + rule.PathPrefix
}

func canonicalHost(host string) string {
	host = strings.ToLower(host)
	if host == "github" {
		return GitHubHost
	}
	return host
}

func validHost(host string) bool {
	if host == "" || strings.TrimSpace(host) != host {
		return false
	}
	for _, char := range host {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '.' && char != '-' {
			return false
		}
	}
	return true
}

func validRepositoryPart(part string) bool {
	if part == "" || part == "." || part == ".." || strings.TrimSpace(part) != part {
		return false
	}
	for _, char := range part {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' && char != '-' && char != '.' {
			return false
		}
	}
	return true
}

func normalizeRelativePath(value string, emptyAllowed bool) (string, error) {
	if value == "" {
		if emptyAllowed {
			return "", nil
		}
		return "", errorsForSource("relative path must not be empty")
	}
	if strings.ContainsAny(value, "\\%?#:\x00") || strings.HasPrefix(value, "/") {
		return "", errorsForSource("path must be a clean relative path")
	}

	components := strings.Split(value, "/")
	cleaned := make([]string, 0, len(components))
	for _, component := range components {
		if component == "" {
			continue
		}
		if component == "." || component == ".." {
			return "", errorsForSource("path dot-segments are not allowed")
		}
		if !validPathComponent(component) {
			return "", errorsForSource("path contains an unsupported character")
		}
		cleaned = append(cleaned, component)
	}
	if len(cleaned) == 0 {
		if emptyAllowed {
			return "", nil
		}
		return "", errorsForSource("relative path must not be empty")
	}
	return strings.Join(cleaned, "/"), nil
}

func validPathComponent(component string) bool {
	for _, char := range component {
		if char < 0x20 || char == 0x7f || char > 0x7f {
			return false
		}
		if strings.ContainsRune("*?[](){}|^$+", char) {
			return false
		}
	}
	return true
}

func errorsForSource(message string) error {
	return fmt.Errorf("invalid recipe source: %s", message)
}

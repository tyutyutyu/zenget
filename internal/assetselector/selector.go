// Package assetselector defines the versioned, reusable asset-selection rule
// stored by zenget application configurations.
package assetselector

import (
	"fmt"
	"regexp"
	"strings"
)

// CurrentVersion is the version of the persisted selector representation.
const CurrentVersion = 1

// Type identifies how a selector pattern is matched against an asset name.
type Type string

const (
	// TypeSubstring performs a case-insensitive literal substring match.
	TypeSubstring Type = "substring"
	// TypeRegex performs a RE2 regular-expression match.
	TypeRegex Type = "regex"
)

// Selector is a versioned reusable rule for selecting release assets.
type Selector struct {
	Version int    `json:"version"`
	Type    Type   `json:"type"`
	Pattern string `json:"pattern"`
}

// NewSubstring returns a selector that matches an asset name literally,
// without regard to case.
func NewSubstring(pattern string) Selector {
	return Selector{Version: CurrentVersion, Type: TypeSubstring, Pattern: pattern}
}

// NewRegex returns a selector that matches an asset name with a RE2 pattern.
func NewRegex(pattern string) Selector {
	return Selector{Version: CurrentVersion, Type: TypeRegex, Pattern: pattern}
}

// Validate checks the persisted selector shape and compiles regex selectors.
// Go's regexp package implements RE2 syntax and therefore rejects unsupported
// constructs before any release or state operation is attempted.
func (s Selector) Validate() error {
	if s.Version != CurrentVersion {
		return fmt.Errorf("unsupported asset selector version %d; expected %d", s.Version, CurrentVersion)
	}
	if s.Pattern == "" {
		return fmt.Errorf("asset selector pattern must not be empty")
	}
	switch s.Type {
	case TypeSubstring:
		return nil
	case TypeRegex:
		if _, err := regexp.Compile(s.Pattern); err != nil {
			return fmt.Errorf("invalid asset selector regex %q: %w", s.Pattern, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported asset selector type %q; expected %q or %q", s.Type, TypeSubstring, TypeRegex)
	}
}

// Match reports whether name satisfies the selector. Invalid selectors never
// match; callers that accept external input should call Validate first to
// return a useful diagnostic error.
func (s Selector) Match(name string) bool {
	switch s.Type {
	case TypeSubstring:
		return strings.Contains(strings.ToLower(name), strings.ToLower(s.Pattern))
	case TypeRegex:
		compiled, err := regexp.Compile(s.Pattern)
		return err == nil && compiled.MatchString(name)
	default:
		return false
	}
}

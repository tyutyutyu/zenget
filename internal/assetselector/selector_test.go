package assetselector

import "testing"

func TestSelectorValidationAndMatching(t *testing.T) {
	tests := []struct {
		name     string
		selector Selector
		valid    bool
		matches  string
		noMatch  string
	}{
		{
			name:     "substring",
			selector: NewSubstring("linux-amd64"),
			valid:    true,
			matches:  "tool-LINUX-amd64-v2.tar.gz",
			noMatch:  "tool-darwin-arm64-v2.tar.gz",
		},
		{
			name:     "regex",
			selector: NewRegex(`^tool-linux-amd64-v[0-9]+\.tar\.gz$`),
			valid:    true,
			matches:  "tool-linux-amd64-v2.tar.gz",
			noMatch:  "tool-linux-amd64-v2.zip",
		},
		{
			name:     "empty pattern",
			selector: Selector{Version: CurrentVersion, Type: TypeSubstring},
		},
		{
			name:     "invalid regex",
			selector: NewRegex("["),
		},
		{
			name:     "unknown type",
			selector: Selector{Version: CurrentVersion, Type: "glob", Pattern: "tool"},
		},
		{
			name:     "unknown version",
			selector: Selector{Version: 2, Type: TypeSubstring, Pattern: "tool"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.selector.Validate()
			if (err == nil) != test.valid {
				t.Fatalf("Validate() error = %v, valid = %v", err, test.valid)
			}
			if !test.valid {
				return
			}
			if !test.selector.Match(test.matches) {
				t.Errorf("Match(%q) = false, want true", test.matches)
			}
			if test.selector.Match(test.noMatch) {
				t.Errorf("Match(%q) = true, want false", test.noMatch)
			}
		})
	}
}

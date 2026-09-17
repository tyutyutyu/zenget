package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestValidateColorMode(t *testing.T) {
	for _, mode := range []string{colorModeAuto, colorModeAlways, colorModeNever} {
		if err := validateColorMode(mode); err != nil {
			t.Errorf("validateColorMode(%q) = %v, want nil", mode, err)
		}
	}
	for _, mode := range []string{"", "AUTO", "yes", "auto "} {
		if err := validateColorMode(mode); err == nil || !strings.Contains(err.Error(), "invalid --color value") {
			t.Errorf("validateColorMode(%q) = %v, want invalid --color value error", mode, err)
		}
	}
}

func TestNoColorSet(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"empty means unset", "", false},
		{"zero means unset", "0", false},
		{"one disables color", "1", true},
		{"word disables color", "true", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", test.value)
			if got := noColorSet(); got != test.want {
				t.Errorf("noColorSet() with NO_COLOR=%q = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestNoColorSetUnset(t *testing.T) {
	old, hadOld := os.LookupEnv("NO_COLOR")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatalf("unset NO_COLOR: %v", err)
	}
	t.Cleanup(func() {
		if hadOld {
			_ = os.Setenv("NO_COLOR", old)
		}
	})
	if noColorSet() {
		t.Error("noColorSet() = true with NO_COLOR unset, want false")
	}
}

func TestColorEnabledModesOnBuffer(t *testing.T) {
	// A bytes.Buffer is never a terminal, so auto mode must not color.
	if colorEnabled(colorModeAuto, &bytes.Buffer{}) {
		t.Error("colorEnabled(auto, buffer) = true, want false")
	}
	if !colorEnabled(colorModeAlways, &bytes.Buffer{}) {
		t.Error("colorEnabled(always, buffer) = false, want true")
	}
	if colorEnabled(colorModeNever, &bytes.Buffer{}) {
		t.Error("colorEnabled(never, buffer) = true, want false")
	}
}

func TestColorEnabledNeverColorsRegularFile(t *testing.T) {
	// A regular file is not a character device, so auto mode must not color.
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer func() { _ = f.Close() }()
	if colorEnabled(colorModeAuto, f) {
		t.Error("colorEnabled(auto, regular file) = true, want false")
	}
	if !colorEnabled(colorModeAlways, f) {
		t.Error("colorEnabled(always, regular file) = false, want true")
	}
}

func TestColorEnabledRespectsNoColor(t *testing.T) {
	// NO_COLOR only affects auto mode; always overrides it.
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(colorModeAuto, &bytes.Buffer{}) {
		t.Error("colorEnabled(auto, buffer) with NO_COLOR=1 = true, want false")
	}
	if !colorEnabled(colorModeAlways, &bytes.Buffer{}) {
		t.Error("colorEnabled(always, buffer) with NO_COLOR=1 = false, want true")
	}
}

func TestRootRejectsInvalidColorBeforeOutput(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	oldArgs := rootCmd.Flags().Args()
	t.Cleanup(func() {
		rootCmd.SetArgs(oldArgs)
		colorFlag = colorModeAuto
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	rootCmd.SetArgs([]string{"list", "--color", "bogus"})

	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)

	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid --color value") {
		t.Fatalf("error = %v, want invalid --color value", err)
	}
	// Cobra prints its usage block on error; what must not appear is any
	// list output, which proves the command body never ran.
	if strings.Contains(stdout.String(), "No applications installed.") {
		t.Errorf("list body ran despite invalid --color: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid --color value") {
		t.Errorf("stderr missing validation error: %q", stderr.String())
	}
}

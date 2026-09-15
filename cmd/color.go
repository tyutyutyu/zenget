package cmd

import (
	"fmt"
	"io"
	"os"
)

// Color modes for the --color flag.
const (
	colorModeAuto   = "auto"
	colorModeAlways = "always"
	colorModeNever  = "never"
)

// colorFlag holds the value of the persistent --color flag. It is a package
// variable so tests can override it without going through flag parsing.
var colorFlag = colorModeAuto

func init() {
	rootCmd.PersistentFlags().StringVar(&colorFlag, "color", colorModeAuto,
		"colorize output: auto, always, or never")
}

// validateColorMode returns an error unless mode is one of the supported
// color modes.
func validateColorMode(mode string) error {
	switch mode {
	case colorModeAuto, colorModeAlways, colorModeNever:
		return nil
	default:
		return fmt.Errorf("invalid --color value %q: must be %s, %s, or %s",
			mode, colorModeAuto, colorModeAlways, colorModeNever)
	}
}

// colorEnabled reports whether output written to out should be colorized in
// the given mode. auto colors only when out is a terminal and the NO_COLOR
// environment variable is not set; always colors regardless of NO_COLOR;
// never disables color.
func colorEnabled(mode string, out io.Writer) bool {
	switch mode {
	case colorModeAlways:
		return true
	case colorModeNever:
		return false
	default:
		return isTerminal(out) && !noColorSet()
	}
}

// isTerminal reports whether out is a character device such as a terminal.
// Writers that are not *os.File (for example test buffers) are not terminals.
func isTerminal(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// noColorSet reports whether the NO_COLOR environment variable requests that
// colors be disabled. An unset, empty, or "0" value means the variable is not
// considered set (per https://no-color.org/).
func noColorSet() bool {
	value, ok := os.LookupEnv("NO_COLOR")
	return ok && value != "" && value != "0"
}

//go:build !linux && !darwin && !windows

package cmd

import "os"

// Unsupported terminal platforms conservatively use noninteractive input.
func fileIsTerminal(file *os.File) bool { return false }

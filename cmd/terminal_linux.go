package cmd

import (
	"golang.org/x/sys/unix"
	"os"
)

func fileIsTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

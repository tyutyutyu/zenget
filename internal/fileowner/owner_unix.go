//go:build !windows

// Package fileowner provides the small platform-specific ownership check used
// by local policy and trust files.
package fileowner

import (
	"os"
	"syscall"
)

// CurrentUserOwns reports whether info identifies a file owned by the current
// Unix user. The second-level metadata type is intentionally isolated here so
// Windows builds do not depend on syscall.Stat_t.
func CurrentUserOwns(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(os.Geteuid())
}

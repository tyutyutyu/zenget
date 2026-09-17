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
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(os.Geteuid())
}

// CurrentUserOwnsPath reports whether the path is owned by the current Unix
// user. The path argument is retained for parity with Windows, where the ACL
// must be queried by name.
func CurrentUserOwnsPath(_ string, info os.FileInfo) bool {
	return info != nil && CurrentUserOwns(info)
}

// PrivateFileModeSafe reports whether a private file has no group/world or
// special permission bits. Unix uses these mode bits as part of its trust
// boundary.
func PrivateFileModeSafe(info os.FileInfo) bool {
	return info != nil && info.Mode().Perm()&0077 == 0 && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

// PrivateDirectoryModeSafe reports whether a private directory has no
// group/world write or special permission bits.
func PrivateDirectoryModeSafe(info os.FileInfo) bool {
	return info != nil && info.Mode().Perm()&0022 == 0 && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

// SecurePath applies the Unix mode expected for a new sensitive path.
func SecurePath(path string, directory bool) error {
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	return os.Chmod(path, mode)
}

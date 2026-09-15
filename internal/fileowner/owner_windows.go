//go:build windows

// Package fileowner provides the small platform-specific ownership check used
// by local policy and trust files.
package fileowner

import "os"

// CurrentUserOwns fails closed on Windows. Windows ACLs do not expose a
// portable POSIX uid through os.FileInfo, so a caller must add an explicit ACL
// check before allowing a sensitive local file.
func CurrentUserOwns(os.FileInfo) bool {
	return false
}

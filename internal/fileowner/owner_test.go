//go:build !windows

package fileowner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentUserOwnsTemporaryFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "owner-check")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	info, err := os.Stat(file.Name())
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !CurrentUserOwns(info) {
		t.Fatal("CurrentUserOwns() = false, want true for a temporary file")
	}
}

func TestPrivateModesAndSecurePath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private-file")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if PrivateFileModeSafe(info) {
		t.Fatal("PrivateFileModeSafe() = true for a group-readable file")
	}
	if !CurrentUserOwnsPath(path, info) {
		t.Fatal("CurrentUserOwnsPath() = false for a file owned by the current user")
	}
	if err := SecurePath(path, false); err != nil {
		t.Fatalf("SecurePath(file) error = %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() after SecurePath(file) error = %v", err)
	}
	if !PrivateFileModeSafe(info) {
		t.Fatal("PrivateFileModeSafe() = false after SecurePath(file)")
	}

	directory := filepath.Join(root, "private-directory")
	if err := os.Mkdir(directory, 0o777); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatalf("Chmod(directory) error = %v", err)
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("Stat(directory) error = %v", err)
	}
	if PrivateDirectoryModeSafe(directoryInfo) {
		t.Fatal("PrivateDirectoryModeSafe() = true for a writable directory")
	}
	if err := SecurePath(directory, true); err != nil {
		t.Fatalf("SecurePath(directory) error = %v", err)
	}
	directoryInfo, err = os.Stat(directory)
	if err != nil {
		t.Fatalf("Stat(directory) after SecurePath error = %v", err)
	}
	if !PrivateDirectoryModeSafe(directoryInfo) {
		t.Fatal("PrivateDirectoryModeSafe() = false after SecurePath(directory)")
	}

	if CurrentUserOwnsPath(path, nil) || PrivateFileModeSafe(nil) || PrivateDirectoryModeSafe(nil) {
		t.Fatal("ownership and mode checks must fail closed for nil metadata")
	}
}

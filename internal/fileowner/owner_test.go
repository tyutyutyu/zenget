//go:build !windows

package fileowner

import (
	"os"
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

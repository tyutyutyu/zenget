//go:build linux

package projectmanifest

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDiscoverSkipsNamedPipeAndSocketCandidates(t *testing.T) {
	tests := []struct {
		name   string
		create func(t *testing.T, path string)
	}{
		{
			name: "named pipe",
			create: func(t *testing.T, path string) {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Skipf("named pipes unavailable: %v", err)
				}
			},
		},
		{
			name: "unix socket",
			create: func(t *testing.T, path string) {
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Skipf("unix sockets unavailable: %v", err)
				}
				t.Cleanup(func() {
					_ = listener.Close()
					_ = os.Remove(path)
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			child := filepath.Join(parent, "child")
			if err := os.MkdirAll(child, 0755); err != nil {
				t.Fatal(err)
			}
			parentManifest := filepath.Join(parent, DefaultFileName)
			writeTestManifest(t, parentManifest, "parent/repository")
			test.create(t, filepath.Join(child, DefaultFileName))

			resolution, err := Resolve(Options{WorkingDirectory: child})
			if err != nil {
				t.Fatalf("discovery with %s: %v", test.name, err)
			}
			if resolution.Path != parentManifest {
				t.Fatalf("discovery with %s = %q, want %q", test.name, resolution.Path, parentManifest)
			}
		})
	}
}

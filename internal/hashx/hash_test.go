package hashx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBytesEmpty(t *testing.T) {
	got := Bytes([]byte{})
	want := "2d06800538d394c2"
	if got != want {
		t.Fatalf("Bytes(empty) = %q, want %q", got, want)
	}
}

func TestFileMatchesBytes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hello.txt")
	content := []byte("hello")
	if err := os.WriteFile(p, content, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := File(p)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	want := Bytes(content)
	if got != want {
		t.Fatalf("File(%q) = %q, Bytes = %q", p, got, want)
	}
}

func TestFileMissing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "missing.bin")
	_, err := File(p)
	if err == nil {
		t.Fatal("File on missing path returned nil error")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("File on missing path returned %T %v, want os.IsNotExist", err, err)
	}
}

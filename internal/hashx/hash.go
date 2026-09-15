// Package hashx provides file hashing helpers for zenget.
package hashx

import (
	"encoding/hex"
	"io"
	"os"

	"github.com/zeebo/xxh3"
)

// File returns the XXH3-64 hash of the file at path as a lowercase hex string.
func File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := xxh3.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Bytes returns the XXH3-64 hash of b as a lowercase hex string.
func Bytes(b []byte) string {
	h := xxh3.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

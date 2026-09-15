// Package shim provides the managed runtime and dispatch support used by
// zenget-generated command wrappers.
package shim

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"zenget/internal/hashx"
	"zenget/internal/wrapper"
)

var (
	currentExecutable = os.Executable
	runtimeRename     = os.Rename
)

// RuntimePath returns the path used by generated wrappers for the managed
// dispatcher executable.
func RuntimePath() (string, error) {
	return wrapper.ShimRuntimePath()
}

// EnsureCurrent installs the currently running zenget executable as the
// managed dispatcher runtime. Both source and destination are required to be
// regular, non-symlink files. A replacement is staged and atomically renamed
// into place, so a failed update leaves an existing runtime untouched.
func EnsureCurrent() (string, error) {
	source, err := currentExecutable()
	if err != nil {
		return "", fmt.Errorf("resolve current zenget executable: %w", err)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolve current zenget executable path: %w", err)
	}
	sourceData, sourceHash, err := readRegular(source, "current zenget executable")
	if err != nil {
		return "", err
	}

	destination, err := RuntimePath()
	if err != nil {
		return "", err
	}
	if valid, err := runtimeMatches(destination, sourceHash); err != nil {
		return "", err
	} else if valid {
		return destination, nil
	}

	directory := filepath.Dir(destination)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return "", fmt.Errorf("create shim runtime directory %q: %w", directory, err)
	}
	if err := rejectRuntimeDestination(destination); err != nil {
		return "", err
	}

	temporary, err := os.CreateTemp(directory, ".runtime-*")
	if err != nil {
		return "", fmt.Errorf("create temporary shim runtime in %q: %w", directory, err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if n, err := temporary.Write(sourceData); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary shim runtime %q: %w", temporaryPath, err)
	} else if n != len(sourceData) {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary shim runtime %q: %w", temporaryPath, io.ErrShortWrite)
	}
	if err := temporary.Chmod(0755); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("set shim runtime permissions %q: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync temporary shim runtime %q: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary shim runtime %q: %w", temporaryPath, err)
	}
	if actual, err := hashx.File(temporaryPath); err != nil {
		return "", fmt.Errorf("verify temporary shim runtime %q: %w", temporaryPath, err)
	} else if actual != sourceHash {
		return "", fmt.Errorf("verify temporary shim runtime %q: hash changed during copy", temporaryPath)
	}
	if err := runtimeRename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("replace shim runtime %q: %w", destination, err)
	}
	removeTemporary = false
	return destination, nil
}

func runtimeMatches(path, expectedHash string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect shim runtime %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("refusing to replace shim runtime symlink %q", path)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("shim runtime %q is not a regular file", path)
	}
	actual, err := hashx.File(path)
	if err != nil {
		return false, fmt.Errorf("hash shim runtime %q: %w", path, err)
	}
	return actual == expectedHash, nil
}

func rejectRuntimeDestination(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect shim runtime %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to replace shim runtime symlink %q", path)
	}
	return nil
}

func readRegular(path, label string) ([]byte, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("inspect %s %q: %w", label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("refusing symlink %s %q", label, path)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("%s %q is not a regular file", label, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s %q: %w", label, path, err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("recheck %s %q: %w", label, path, err)
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || after.Size() != int64(len(data)) {
		return nil, "", fmt.Errorf("%s %q changed during verification", label, path)
	}
	return data, hashx.Bytes(data), nil
}

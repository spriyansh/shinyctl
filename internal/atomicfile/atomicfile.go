// Package atomicfile writes files that are never observed half-written.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces path with data atomically: into a sibling temp file
// first, then renamed into place. rename(2) is atomic on POSIX, so a
// crash or a concurrent reader sees either the old file or the new one,
// never a truncated half of the new one. The file is created 0600 — every
// caller here holds per-user state, and the temp file inherits
// os.CreateTemp's mode without a separate chmod step.
func Write(path string, data []byte) error {
	// Same directory as path, so the rename never crosses a filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

package atomicfile

import (
	"os"
	"path/filepath"
)

// Write persists data to path atomically using a synced temporary file and rename.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			cleanup()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

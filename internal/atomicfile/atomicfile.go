package atomicfile

import (
	"os"
	"path/filepath"
)

// Write persists data to path atomically using a synced temporary file, rename, and parent directory sync.
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

	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		cleanup()
		return err
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
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

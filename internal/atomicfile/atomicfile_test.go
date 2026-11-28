package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWriteCreatesAndReplacesWithPermissions(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sample.txt")

	if err := Write(target, []byte("first"), 0600); err != nil {
		t.Fatalf("first Write failed: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("perm = %o, want 0600", info.Mode().Perm())
	}

	if err := Write(target, []byte("second"), 0600); err != nil {
		t.Fatalf("second Write failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want second", string(got))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 file in dir, got %d", len(entries))
	}
}

func TestWriteEmptyData(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"nil slice", nil},
		{"empty slice", []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".txt")
			if err := Write(target, tc.data, 0600); err != nil {
				t.Fatalf("Write failed: %v", err)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("ReadFile failed: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("len(got) = %d, want 0", len(got))
			}
		})
	}
}

func TestWriteInvalidPaths(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"empty", "", "path is empty"},
		{"whitespace", "   ", "path is empty"},
		{"null byte", filepath.Join(dir, "bad\x00name"), "null byte"},
		{"trailing separator", dir + string(os.PathSeparator), "directory separator"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Write(tc.path, []byte("x"), 0600)
			if err == nil {
				t.Fatalf("Write(%q) succeeded, want error containing %q", tc.path, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestWriteDirectoryErrorsAndNoTempLeak(t *testing.T) {
	t.Run("missing parent dir", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "does-not-exist", "file.txt")
		if err := Write(target, []byte("x"), 0600); err == nil {
			t.Fatal("Write to missing parent dir succeeded, want error")
		}
	})

	t.Run("read-only parent dir", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("skipping read-only directory check under root")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0500); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(dir, 0700)
		})
		if err := Write(filepath.Join(dir, "file.txt"), []byte("x"), 0600); err == nil {
			t.Fatal("Write to read-only parent dir succeeded, want error")
		}
	})

	t.Run("target is existing directory leaves no temp files", func(t *testing.T) {
		parent := t.TempDir()
		sub := filepath.Join(parent, "existing-dir")
		if err := os.Mkdir(sub, 0700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := Write(sub, []byte("x"), 0600); err == nil {
			t.Fatal("Write over existing directory succeeded, want error")
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".tmp-") {
				t.Errorf("leaked temporary file: %s", entry.Name())
			}
		}
	})
}

func TestWriteReplacesSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	realTarget := filepath.Join(dir, "original.txt")
	if err := os.WriteFile(realTarget, []byte("original-content"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	linkPath := filepath.Join(dir, "link.txt")
	if err := os.Symlink("original.txt", linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if err := Write(linkPath, []byte("replacement-content"), 0600); err != nil {
		t.Fatalf("Write over symlink failed: %v", err)
	}

	fi, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("linkPath is still a symlink, want regular file")
	}

	gotLink, err := os.ReadFile(linkPath)
	if err != nil {
		t.Fatalf("ReadFile linkPath: %v", err)
	}
	if string(gotLink) != "replacement-content" {
		t.Errorf("linkPath content = %q, want replacement-content", string(gotLink))
	}

	gotTarget, err := os.ReadFile(realTarget)
	if err != nil {
		t.Fatalf("ReadFile realTarget: %v", err)
	}
	if string(gotTarget) != "original-content" {
		t.Errorf("original target modified: got %q, want original-content", string(gotTarget))
	}
}

func TestWriteConcurrent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "concurrent.txt")

	const workers = 16
	const iterations = 10
	validPayloads := make(map[string]bool, workers*iterations)
	for w := 0; w < workers; w++ {
		for i := 0; i < iterations; i++ {
			validPayloads[fmt.Sprintf("worker-%02d-iter-%02d", w, i)] = true
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, workers*iterations)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				payload := fmt.Sprintf("worker-%02d-iter-%02d", workerID, i)
				if err := Write(target, []byte(payload), 0600); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent Write failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !validPayloads[string(got)] {
		t.Errorf("final content = %q is not a complete payload", string(got))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Errorf("leaked temporary file after concurrent writes: %s", entry.Name())
		}
	}
}

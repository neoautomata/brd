package atomicfile

import (
	"os"
	"path/filepath"
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

package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/neoautomata/brd/internal/config"
	"github.com/neoautomata/brd/internal/imgconv"
)

type fakeSession struct {
	getErr     error
	waitErr    error
	winPNG     []byte
	elemPNG    []byte
	gotURL     string
	waitedID   string
	capturedID string
}

func (f *fakeSession) Get(url string) error {
	f.gotURL = url
	return f.getErr
}

func (f *fakeSession) WaitForElement(id string, _ time.Duration) error {
	f.waitedID = id
	return f.waitErr
}

func (f *fakeSession) WindowScreenshot() ([]byte, error) { return f.winPNG, nil }

func (f *fakeSession) ElementScreenshot(id string) ([]byte, error) {
	f.capturedID = id
	return f.elemPNG, nil
}

func (f *fakeSession) Close() error { return nil }

type fakeFactory struct {
	session *fakeSession
	err     error
	closed  bool
}

func (f *fakeFactory) NewSession() (BrowserSession, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.session, nil
}

func (f *fakeFactory) Close() error {
	f.closed = true
	return nil
}

type fakeConverter struct {
	writeSha bool
	err      error
}

func (fc fakeConverter) Convert(_ context.Context, inputPNG, outputImg string) error {
	if fc.err != nil {
		return fc.err
	}
	data, err := os.ReadFile(inputPNG)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outputImg, data, 0600); err != nil {
		return err
	}
	if fc.writeSha {
		h := fmt.Sprintf("%x", sha256.Sum256(data))
		return os.WriteFile(outputImg+".sha", []byte(h), 0600)
	}
	return nil
}

func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(`{"data_dir":"`+dir+`","timezone":"UTC","retention_days":7}`), "")
	if err != nil {
		t.Fatalf("Parse config: %v", err)
	}
	for _, d := range []string{cfg.ContentsDir(), cfg.ScreenshotsDir(), cfg.ImagesDir()} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	return cfg
}

func TestRegenerateWritesFilesAndFallbackHash(t *testing.T) {
	cfg := newTestConfig(t)
	sess := &fakeSession{
		winPNG:  []byte("window-png"),
		elemPNG: []byte("element-png"),
	}
	factory := &fakeFactory{session: sess}
	svc := NewService(cfg, factory, fakeConverter{writeSha: false})
	svc.settleDelay = 0

	if err := svc.Regenerate(context.Background(), "2026-06-15"); err != nil {
		t.Fatalf("Regenerate failed: %v", err)
	}

	if sess.gotURL != "http://127.0.0.1:1234/?date=2026-06-15" {
		t.Errorf("gotURL = %q", sess.gotURL)
	}
	if sess.waitedID != "screenshot_content" || sess.capturedID != "screenshot_content" {
		t.Errorf("unexpected element IDs: waited=%q captured=%q", sess.waitedID, sess.capturedID)
	}

	gotHash, err := os.ReadFile(cfg.HashPath("2026-06-15"))
	if err != nil {
		t.Fatalf("read hash file: %v", err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte("element-png")))
	if string(gotHash) != wantHash {
		t.Errorf("hash = %q, want %q", string(gotHash), wantHash)
	}
}

func TestRegeneratePropagatesWaitErrorAndInvalidDate(t *testing.T) {
	cfg := newTestConfig(t)
	sess := &fakeSession{waitErr: errors.New("element timeout")}
	svc := NewService(cfg, &fakeFactory{session: sess}, fakeConverter{writeSha: true})
	svc.settleDelay = 0

	if err := svc.Regenerate(context.Background(), "bad-date"); err == nil {
		t.Error("expected error for invalid date")
	}
	if err := svc.Regenerate(context.Background(), "2026-06-15"); err == nil {
		t.Error("expected WaitForElement error to propagate")
	}
}

func TestCleanupOldFiles(t *testing.T) {
	cfg := newTestConfig(t)
	svc := NewService(cfg, &fakeFactory{}, fakeConverter{})

	oldFile := filepath.Join(cfg.ContentsDir(), "2026-06-01.html")
	newFile := filepath.Join(cfg.ContentsDir(), "2026-06-14.html")
	otherFile := filepath.Join(cfg.ContentsDir(), "readme.txt")

	for _, p := range []string{oldFile, newFile, otherFile} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	svc.CleanupOldFiles(now)

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("expected old file to be deleted")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("expected recent file to remain: %v", err)
	}
	if _, err := os.Stat(otherFile); err != nil {
		t.Errorf("expected non-dated file to remain: %v", err)
	}
}

func TestBuildViewBaseURL(t *testing.T) {
	tests := []struct {
		addr string
		want string
	}{
		{":1234", "http://127.0.0.1:1234"},
		{"0.0.0.0:1234", "http://127.0.0.1:1234"},
		{"[::]:1234", "http://127.0.0.1:1234"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080"},
	}
	for _, tc := range tests {
		if got := buildViewBaseURL(tc.addr); got != tc.want {
			t.Errorf("buildViewBaseURL(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func TestRegenerateWithGoConverterEndToEnd(t *testing.T) {
	cfg := newTestConfig(t)

	// Encode a real 540x960 PNG with a white top-right corner pixel.
	img := image.NewGray(image.Rect(0, 0, imgconv.DefaultWidth, imgconv.DefaultHeight))
	img.SetGray(imgconv.DefaultWidth-1, 0, color.Gray{Y: 0xF0})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	sess := &fakeSession{
		winPNG:  buf.Bytes(),
		elemPNG: buf.Bytes(),
	}
	svc := NewService(cfg, &fakeFactory{session: sess}, nil)
	svc.settleDelay = 0

	if err := svc.Regenerate(context.Background(), "2026-06-15"); err != nil {
		t.Fatalf("Regenerate with GoConverter failed: %v", err)
	}

	raw, err := os.ReadFile(cfg.ImagePath("2026-06-15"))
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if len(raw) != 259200 {
		t.Fatalf("len(raw) = %d, want 259200", len(raw))
	}
	if raw[0] != 0x0F {
		t.Errorf("raw[0] = 0x%02x, want 0x0f", raw[0])
	}

	shaBytes, err := os.ReadFile(cfg.HashPath("2026-06-15"))
	if err != nil {
		t.Fatalf("read hash: %v", err)
	}
	wantSha := fmt.Sprintf("%x", sha256.Sum256(raw))
	if string(shaBytes) != wantSha {
		t.Errorf("sha = %q, want %q", string(shaBytes), wantSha)
	}
}

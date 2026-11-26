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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	blockOnGet chan struct{}
	getStarted chan struct{}
}

func (f *fakeSession) Get(ctx context.Context, url string) error {
	f.gotURL = url
	if f.getStarted != nil {
		close(f.getStarted)
	}
	if f.blockOnGet != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.blockOnGet:
		}
	}
	return f.getErr
}

func (f *fakeSession) WaitForElement(ctx context.Context, id string, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.waitedID = id
	return f.waitErr
}

func (f *fakeSession) WindowScreenshot(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.winPNG, nil
}

func (f *fakeSession) ElementScreenshot(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.capturedID = id
	return f.elemPNG, nil
}

func (f *fakeSession) Close() error { return nil }

type fakeFactory struct {
	session *fakeSession
	err     error
	closed  bool
}

func (f *fakeFactory) NewSession(ctx context.Context) (BrowserSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	defer svc.Close()
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
	defer svc.Close()
	svc.settleDelay = 0

	if err := svc.Regenerate(context.Background(), "bad-date"); err == nil {
		t.Error("expected error for invalid date")
	}
	if err := svc.Regenerate(context.Background(), "2026-06-15"); err == nil {
		t.Error("expected WaitForElement error to propagate")
	}
}

func TestRegenerateRespectsContextCancellationAndClose(t *testing.T) {
	cfg := newTestConfig(t)
	sess := &fakeSession{
		blockOnGet: make(chan struct{}),
		getStarted: make(chan struct{}),
	}
	factory := &fakeFactory{session: sess}
	svc := NewService(cfg, factory, fakeConverter{writeSha: true})
	svc.settleDelay = 0

	// Cancelled caller context fails immediately before touching the factory.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Regenerate(cancelledCtx, "2026-06-15"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Regenerate err = %v, want context.Canceled", err)
	}

	// Service.Close cancels an active capture and rejects subsequent Regenerate calls.
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Regenerate(context.Background(), "2026-06-15")
	}()

	<-sess.getStarted
	if err := svc.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !factory.closed {
		t.Error("expected factory.Close() to be called")
	}

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Errorf("in-flight Regenerate err after Close = %v, want context.Canceled", err)
	}
	if err := svc.Regenerate(context.Background(), "2026-06-15"); !errors.Is(err, ErrServiceClosed) {
		t.Errorf("post-Close Regenerate err = %v, want ErrServiceClosed", err)
	}
}

func TestCleanupOldFiles(t *testing.T) {
	cfg := newTestConfig(t)
	svc := NewService(cfg, &fakeFactory{}, fakeConverter{})
	defer svc.Close()

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
	defer svc.Close()
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

func TestChromedpFactoryRealCapture(t *testing.T) {
	browserBin := ""
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			browserBin = p
			break
		}
	}
	if browserBin == "" {
		t.Skip("no chromium/chrome binary available in PATH")
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><body style="margin:0;padding:0;">`+
			`<div id="screenshot_content" style="width:540px;height:960px;background:#ffffff;color:#000000;">`+
			`<h1>brd chromedp test</h1></div></body></html>`)
	}))
	defer ts.Close()

	cfg := newTestConfig(t)
	cfg.ChromePath = browserBin
	svc := NewService(cfg, nil, nil)
	defer svc.Close()
	svc.viewBaseURL = ts.URL
	svc.settleDelay = 0

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := svc.Regenerate(ctx, "2026-06-15"); err != nil {
		t.Fatalf("real chromedp Regenerate failed: %v", err)
	}

	elemBytes, err := os.ReadFile(cfg.ScreenshotPath("2026-06-15"))
	if err != nil {
		t.Fatalf("read element screenshot: %v", err)
	}
	elemImg, err := png.Decode(bytes.NewReader(elemBytes))
	if err != nil {
		t.Fatalf("decode element png: %v", err)
	}
	if b := elemImg.Bounds(); b.Dx() != 540 || b.Dy() != 960 {
		t.Errorf("element screenshot dimensions = %dx%d, want 540x960", b.Dx(), b.Dy())
	}

	raw, err := os.ReadFile(cfg.ImagePath("2026-06-15"))
	if err != nil {
		t.Fatalf("read 4bpp image: %v", err)
	}
	if len(raw) != 259200 {
		t.Errorf("len(raw) = %d, want 259200", len(raw))
	}
}

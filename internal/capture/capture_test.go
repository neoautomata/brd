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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neoautomata/brd/internal/config"
	"github.com/neoautomata/brd/internal/imgconv"
)

type fakeSession struct {
	getErr     error
	waitErr    error
	winErr     error
	elemErr    error
	winPNG     []byte
	elemPNG    []byte
	gotURL     string
	waitedID   string
	capturedID string
	blockOnGet chan struct{}
	getStarted chan struct{}
	onStart    func()
	onClose    func()
	getDelay   time.Duration
}

func (f *fakeSession) Get(ctx context.Context, url string) error {
	if f.onStart != nil {
		f.onStart()
	}
	f.gotURL = url
	if f.getStarted != nil {
		close(f.getStarted)
	}
	if f.getDelay > 0 {
		timer := time.NewTimer(f.getDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
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
	if f.winErr != nil {
		return nil, f.winErr
	}
	return f.winPNG, nil
}

func (f *fakeSession) ElementScreenshot(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.capturedID = id
	if f.elemErr != nil {
		return nil, f.elemErr
	}
	return f.elemPNG, nil
}

func (f *fakeSession) Close() error {
	if f.onClose != nil {
		f.onClose()
	}
	return nil
}

type fakeReadySession struct {
	*fakeSession
	readyErr    error
	readyCalled bool
}

func (f *fakeReadySession) WaitReady(ctx context.Context, _ time.Duration) error {
	f.readyCalled = true
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.readyErr
}

type fakeFactory struct {
	session    BrowserSession
	newSession func(ctx context.Context) (BrowserSession, error)
	err        error
	closed     bool
}

func (f *fakeFactory) NewSession(ctx context.Context) (BrowserSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.newSession != nil {
		return f.newSession(ctx)
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

func TestRegenerateWritesAndUpdatesHashAcrossRepeatedRuns(t *testing.T) {
	cfg := newTestConfig(t)
	sess := &fakeSession{
		winPNG:  []byte("window-png-1"),
		elemPNG: []byte("element-png-1"),
	}
	factory := &fakeFactory{session: sess}
	svc := NewService(cfg, factory, fakeConverter{writeSha: false})
	defer svc.Close()
	svc.settleDelay = 0

	if err := svc.Regenerate(context.Background(), "2026-06-15"); err != nil {
		t.Fatalf("first Regenerate failed: %v", err)
	}

	if sess.gotURL != "http://127.0.0.1:1234/?date=2026-06-15" {
		t.Errorf("gotURL = %q", sess.gotURL)
	}
	if sess.waitedID != "screenshot_content" || sess.capturedID != "screenshot_content" {
		t.Errorf("unexpected element IDs: waited=%q captured=%q", sess.waitedID, sess.capturedID)
	}

	gotHash1, err := os.ReadFile(cfg.HashPath("2026-06-15"))
	if err != nil {
		t.Fatalf("read first hash file: %v", err)
	}
	wantHash1 := fmt.Sprintf("%x", sha256.Sum256([]byte("element-png-1")))
	if string(gotHash1) != wantHash1 {
		t.Errorf("first hash = %q, want %q", string(gotHash1), wantHash1)
	}

	// Second Regenerate for same date with new image bytes must update .sha even when converter skips writing .sha.
	sess.elemPNG = []byte("element-png-2-updated")
	if err := svc.Regenerate(context.Background(), "2026-06-15"); err != nil {
		t.Fatalf("second Regenerate failed: %v", err)
	}
	gotHash2, err := os.ReadFile(cfg.HashPath("2026-06-15"))
	if err != nil {
		t.Fatalf("read second hash file: %v", err)
	}
	wantHash2 := fmt.Sprintf("%x", sha256.Sum256([]byte("element-png-2-updated")))
	if string(gotHash2) != wantHash2 {
		t.Errorf("updated hash = %q, want %q", string(gotHash2), wantHash2)
	}
}

func TestRegenerateErrorPropagation(t *testing.T) {
	today := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		factory BrowserFactory
		conv    Converter
		dateStr string
	}{
		{
			name:    "invalid date",
			factory: &fakeFactory{session: &fakeSession{}},
			conv:    fakeConverter{writeSha: true},
			dateStr: "bad-date",
		},
		{
			name:    "NewSession error",
			factory: &fakeFactory{err: errors.New("browser start failed")},
			conv:    fakeConverter{writeSha: true},
			dateStr: "2026-06-15",
		},
		{
			name:    "Get error",
			factory: &fakeFactory{session: &fakeSession{getErr: errors.New("nav failed")}},
			conv:    fakeConverter{writeSha: true},
			dateStr: "2026-06-15",
		},
		{
			name:    "WaitForElement error",
			factory: &fakeFactory{session: &fakeSession{waitErr: errors.New("element timeout")}},
			conv:    fakeConverter{writeSha: true},
			dateStr: "2026-06-15",
		},
		{
			name:    "WindowScreenshot error",
			factory: &fakeFactory{session: &fakeSession{winErr: errors.New("window capture failed")}},
			conv:    fakeConverter{writeSha: true},
			dateStr: "2026-06-15",
		},
		{
			name:    "ElementScreenshot error",
			factory: &fakeFactory{session: &fakeSession{winPNG: []byte("w"), elemErr: errors.New("element capture failed")}},
			conv:    fakeConverter{writeSha: true},
			dateStr: "2026-06-15",
		},
		{
			name:    "Convert error",
			factory: &fakeFactory{session: &fakeSession{winPNG: []byte("w"), elemPNG: []byte("e")}},
			conv:    fakeConverter{err: errors.New("convert failed")},
			dateStr: "2026-06-15",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig(t)
			svc := NewService(cfg, tc.factory, tc.conv)
			defer svc.Close()
			svc.settleDelay = 0
			svc.nowFn = func() time.Time { return today }

			if err := svc.Regenerate(context.Background(), tc.dateStr); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestRegenerateWeatherWidgetSettleAndCancel(t *testing.T) {
	t.Run("no weather widget skips settle wait completely", func(t *testing.T) {
		cfg := newTestConfig(t)
		cfg.WeatherWidgetHTML = ""
		sess := &fakeReadySession{
			fakeSession: &fakeSession{winPNG: []byte("w"), elemPNG: []byte("e")},
		}
		svc := NewService(cfg, &fakeFactory{session: sess}, fakeConverter{writeSha: true})
		defer svc.Close()
		svc.settleDelay = 5 * time.Second

		start := time.Now()
		if err := svc.Regenerate(context.Background(), "2026-06-15"); err != nil {
			t.Fatalf("Regenerate failed: %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("expected immediate return without weather widget, took %v", elapsed)
		}
		if sess.readyCalled {
			t.Error("expected WaitReady not to be called when WeatherWidgetHTML is empty")
		}
	})

	t.Run("readiness waiter timeout logs warning and completes capture", func(t *testing.T) {
		cfg := newTestConfig(t)
		cfg.WeatherWidgetHTML = `<div class="weather">Sunny</div>`
		sess := &fakeReadySession{
			fakeSession: &fakeSession{winPNG: []byte("w"), elemPNG: []byte("e")},
			readyErr:    context.DeadlineExceeded,
		}
		svc := NewService(cfg, &fakeFactory{session: sess}, fakeConverter{writeSha: true})
		defer svc.Close()
		svc.settleDelay = 50 * time.Millisecond

		if err := svc.Regenerate(context.Background(), "2026-06-15"); err != nil {
			t.Fatalf("Regenerate should continue after widget readiness timeout, got %v", err)
		}
		if !sess.readyCalled {
			t.Error("expected WaitReady to be called when WeatherWidgetHTML is configured")
		}
	})

	t.Run("context cancellation during settle delay aborts promptly", func(t *testing.T) {
		cfg := newTestConfig(t)
		cfg.WeatherWidgetHTML = `<div class="weather">Sunny</div>`
		sess := &fakeSession{winPNG: []byte("w"), elemPNG: []byte("e")}
		svc := NewService(cfg, &fakeFactory{session: sess}, fakeConverter{writeSha: true})
		defer svc.Close()
		svc.settleDelay = 5 * time.Second

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		err := svc.Regenerate(ctx, "2026-06-15")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestRegenerateSerializesConcurrentCalls(t *testing.T) {
	cfg := newTestConfig(t)
	var active atomic.Int32
	var maxActive atomic.Int32

	factory := &fakeFactory{
		newSession: func(_ context.Context) (BrowserSession, error) {
			return &fakeSession{
				winPNG:   []byte("win"),
				elemPNG:  []byte("elem"),
				getDelay: 10 * time.Millisecond,
				onStart: func() {
					cur := active.Add(1)
					for {
						prev := maxActive.Load()
						if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
							break
						}
					}
				},
				onClose: func() {
					active.Add(-1)
				},
			}, nil
		},
	}

	svc := NewService(cfg, factory, fakeConverter{writeSha: true})
	defer svc.Close()
	svc.settleDelay = 0

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Regenerate(context.Background(), "2026-06-15"); err != nil {
				t.Errorf("concurrent Regenerate failed: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := maxActive.Load(); got != 1 {
		t.Errorf("max concurrent sessions = %d, want 1", got)
	}
}

func TestNewServiceGuardsConfigDefaultsLocationAndPropagatesNoSandbox(t *testing.T) {
	t.Run("nil config panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic when cfg is nil")
			}
		}()
		NewService(nil, nil, nil)
	})

	t.Run("defaults nil Location and propagates ChromeNoSandbox", func(t *testing.T) {
		cfg := &config.Config{
			DataDir:         t.TempDir(),
			LocalAddr:       "127.0.0.1:1234",
			ChromePath:      "/custom/bin/chromium",
			ChromeNoSandbox: true,
		}
		svc := NewService(cfg, nil, nil)
		defer svc.Close()

		if svc.cfg.Location != time.UTC {
			t.Errorf("svc.cfg.Location = %v, want UTC", svc.cfg.Location)
		}
		if cfg.Location != nil {
			t.Errorf("caller cfg.Location was mutated to %v", cfg.Location)
		}
		cf, ok := svc.browsers.(*ChromedpFactory)
		if !ok {
			t.Fatalf("svc.browsers type = %T, want *ChromedpFactory", svc.browsers)
		}
		if cf.ExecPath != "/custom/bin/chromium" {
			t.Errorf("cf.ExecPath = %q, want /custom/bin/chromium", cf.ExecPath)
		}
		if !cf.NoSandbox {
			t.Error("cf.NoSandbox = false, want true")
		}
		if _, hasFlag := cf.allocatorFlags()["no-sandbox"]; !hasFlag {
			t.Error("expected no-sandbox allocator flag when ChromeNoSandbox is true")
		}
	})
}

func TestRegenerateQueuedCallerAbortsOnContextDoneAndCloseIsIdempotent(t *testing.T) {
	cfg := newTestConfig(t)
	var sessionCalls atomic.Int32
	var closeCalls atomic.Int32

	firstSess := &fakeSession{
		winPNG:     []byte("win"),
		elemPNG:    []byte("elem"),
		blockOnGet: make(chan struct{}),
		getStarted: make(chan struct{}),
	}
	factory := &countingCloseFactory{
		fakeFactory: fakeFactory{
			newSession: func(_ context.Context) (BrowserSession, error) {
				sessionCalls.Add(1)
				return firstSess, nil
			},
		},
		onClose: func() {
			closeCalls.Add(1)
		},
	}

	svc := NewService(cfg, factory, fakeConverter{writeSha: true})
	svc.settleDelay = 0

	firstErrCh := make(chan error, 1)
	go func() {
		firstErrCh <- svc.Regenerate(context.Background(), "2026-06-15")
	}()
	<-firstSess.getStarted

	// Queued Regenerate aborts promptly when its context expires while slot is held.
	queuedCtx, cancelQueued := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelQueued()
	start := time.Now()
	queuedErr := svc.Regenerate(queuedCtx, "2026-06-16")
	elapsed := time.Since(start)
	if !errors.Is(queuedErr, context.DeadlineExceeded) {
		t.Fatalf("queued Regenerate err = %v, want context.DeadlineExceeded", queuedErr)
	}
	if elapsed > time.Second {
		t.Errorf("queued Regenerate took %v to abort, want prompt cancellation", elapsed)
	}
	if got := sessionCalls.Load(); got != 1 {
		t.Errorf("NewSession calls = %d, want 1", got)
	}

	// Release first capture and confirm it finishes cleanly.
	close(firstSess.blockOnGet)
	if err := <-firstErrCh; err != nil {
		t.Fatalf("first Regenerate failed: %v", err)
	}

	// Repeated Close calls remain idempotent and do not deadlock.
	if err := svc.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Errorf("factory.Close calls = %d, want 1", got)
	}
}

type countingCloseFactory struct {
	fakeFactory
	onClose func()
}

func (f *countingCloseFactory) Close() error {
	if f.onClose != nil {
		f.onClose()
	}
	return f.fakeFactory.Close()
}

func TestWidgetReadyScriptJSContainsHydrationChecks(t *testing.T) {
	for _, token := range []string{".weather-widget", `[id^="ww_"]`, ".ww-box", ".ww_cond", ".ww_line", ".ww_hour", "script"} {
		if !bytes.Contains([]byte(widgetReadyScriptJS), []byte(token)) {
			t.Errorf("widgetReadyScriptJS missing expected selector or token %q", token)
		}
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

func TestCleanupOldFilesRetentionBoundaryAndSymlinks(t *testing.T) {
	cfg := newTestConfig(t)
	svc := NewService(cfg, &fakeFactory{}, fakeConverter{})
	defer svc.Close()

	// now = 2026-06-15 12:00 UTC, RetentionDays = 7 -> cutoff = 2026-06-08 00:00 UTC.
	// 2026-06-07 is strictly before cutoff (deleted); 2026-06-08 equals cutoff (kept).
	expiredNote := filepath.Join(cfg.ContentsDir(), "2026-06-07.html")
	boundaryNote := filepath.Join(cfg.ContentsDir(), "2026-06-08.html")
	recentNote := filepath.Join(cfg.ContentsDir(), "2026-06-14.html")
	otherFile := filepath.Join(cfg.ContentsDir(), "readme.txt")

	expiredWin := filepath.Join(cfg.ScreenshotsDir(), "2026-06-07_window.png")
	boundaryWin := filepath.Join(cfg.ScreenshotsDir(), "2026-06-08_window.png")
	expiredSha := filepath.Join(cfg.ImagesDir(), "2026-06-07.sha")
	boundarySha := filepath.Join(cfg.ImagesDir(), "2026-06-08.sha")

	staleTmp := filepath.Join(cfg.ImagesDir(), ".tmp-stale-123")
	freshTmp := filepath.Join(cfg.ImagesDir(), ".tmp-fresh-456")

	for _, p := range []string{
		expiredNote, boundaryNote, recentNote, otherFile,
		expiredWin, boundaryWin, expiredSha, boundarySha,
		staleTmp, freshTmp,
	} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	// Symlink with expired date name must NOT be unlinked.
	symlinkPath := filepath.Join(cfg.ContentsDir(), "2026-06-01.html")
	if err := os.Symlink(otherFile, symlinkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-2 * time.Hour)
	freshTime := now.Add(-5 * time.Minute)
	if err := os.Chtimes(staleTmp, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes staleTmp: %v", err)
	}
	if err := os.Chtimes(freshTmp, freshTime, freshTime); err != nil {
		t.Fatalf("chtimes freshTmp: %v", err)
	}
	svc.CleanupOldFiles(now)

	for _, shouldDelete := range []string{expiredNote, expiredWin, expiredSha, staleTmp} {
		if _, err := os.Stat(shouldDelete); !os.IsNotExist(err) {
			t.Errorf("expected %s to be deleted", shouldDelete)
		}
	}
	for _, shouldKeep := range []string{boundaryNote, recentNote, otherFile, boundaryWin, boundarySha, freshTmp} {
		if _, err := os.Stat(shouldKeep); err != nil {
			t.Errorf("expected %s to remain: %v", shouldKeep, err)
		}
	}
	if _, err := os.Lstat(symlinkPath); err != nil {
		t.Errorf("expected symlink %s to remain untouched: %v", symlinkPath, err)
	}

	// Non-ENOENT ReadDir error (path is a regular file, not a directory) is logged without panicking.
	if err := os.RemoveAll(cfg.ScreenshotsDir()); err != nil {
		t.Fatalf("remove screenshots dir: %v", err)
	}
	if err := os.WriteFile(cfg.ScreenshotsDir(), []byte("not-a-dir"), 0600); err != nil {
		t.Fatalf("write file at screenshots dir path: %v", err)
	}
	svc.CleanupOldFiles(now)
}

func TestChromedpFactoryNoSandboxOptIn(t *testing.T) {
	defaultFactory := &ChromedpFactory{}
	if defaultFactory.NoSandbox {
		t.Error("expected NoSandbox default to be false")
	}
	if _, ok := defaultFactory.allocatorFlags()["no-sandbox"]; ok {
		t.Error("expected no-sandbox flag to be omitted by default")
	}

	optInFactory := &ChromedpFactory{NoSandbox: true}
	if _, ok := optInFactory.allocatorFlags()["no-sandbox"]; !ok {
		t.Error("expected no-sandbox flag when NoSandbox is true")
	}
}

func TestRegenerateWritesWindowScreenshotOnlyForToday(t *testing.T) {
	cfg := newTestConfig(t)
	sess := &fakeSession{
		winPNG:  []byte("window-png"),
		elemPNG: []byte("element-png"),
	}
	svc := NewService(cfg, &fakeFactory{session: sess}, fakeConverter{writeSha: true})
	defer svc.Close()
	svc.settleDelay = 0
	svc.nowFn = func() time.Time { return time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC) }

	if err := svc.Regenerate(context.Background(), "2026-06-15"); err != nil {
		t.Fatalf("Regenerate today: %v", err)
	}
	if _, err := os.Stat(cfg.WindowScreenshotPath("2026-06-15")); err != nil {
		t.Errorf("expected today window screenshot to exist: %v", err)
	}

	if err := svc.Regenerate(context.Background(), "2026-06-16"); err != nil {
		t.Fatalf("Regenerate tomorrow: %v", err)
	}
	if _, err := os.Stat(cfg.WindowScreenshotPath("2026-06-16")); !os.IsNotExist(err) {
		t.Errorf("expected tomorrow window screenshot not to be written")
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
		switch r.URL.Path {
		case "/ww-hydrated":
			_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><body><div id="screenshot_content">`+
				`<div class="weather-widget"><div id="ww_abc123"><a href="https://weatherwidget.org/">Weather</a></div>`+
				`<script>setTimeout(() => {`+
				`const ww = document.getElementById('ww_abc123');`+
				`ww.innerHTML = '<div class="ww-box"><span class="ww_cond">Sunny 22C</span></div>';`+
				`}, 250);</script></div></div></body></html>`)
		case "/ww-unhydrated":
			_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><body><div id="screenshot_content">`+
				`<div class="weather-widget"><div id="ww_stuck"><a href="https://weatherwidget.org/">Fallback</a></div></div>`+
				`</div></body></html>`)
		case "/static-widget":
			_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><body><div id="screenshot_content">`+
				`<div class="weather-widget"><span>20C</span></div></div></body></html>`)
		case "/script-hydrated":
			_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><body><div id="screenshot_content">`+
				`<div class="weather-widget"><div id="custom-weather"></div>`+
				`<script>setTimeout(() => {`+
				`document.getElementById('custom-weather').textContent = '19C Cloudy';`+
				`}, 200);</script></div></div></body></html>`)
		default:
			_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><body style="margin:0;padding:0;">`+
				`<div id="screenshot_content" style="width:540px;height:960px;background:#ffffff;color:#000000;">`+
				`<h1>brd chromedp test %s</h1></div></body></html>`, r.URL.Query().Get("date"))
		}
	}))
	defer ts.Close()

	cfg := newTestConfig(t)
	cfg.ChromePath = browserBin
	cfg.WeatherWidgetHTML = `<span>20C</span>`
	factory := &ChromedpFactory{ExecPath: browserBin, NoSandbox: true}
	svc := NewService(cfg, factory, nil)
	defer svc.Close()
	svc.viewBaseURL = ts.URL
	svc.captureTimeout = 3 * time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := svc.Regenerate(ctx, "2026-06-15"); err != nil {
		t.Fatalf("first real chromedp Regenerate failed: %v", err)
	}
	factory.mu.Lock()
	firstBrowserCtx := factory.browserCtx
	factory.mu.Unlock()
	if firstBrowserCtx == nil {
		t.Fatal("expected persistent browserCtx after first Regenerate")
	}

	// Capture a second date to verify persistent browser reuse across tabs.
	if err := svc.Regenerate(ctx, "2026-06-16"); err != nil {
		t.Fatalf("second real chromedp Regenerate failed: %v", err)
	}
	factory.mu.Lock()
	secondBrowserCtx := factory.browserCtx
	factory.mu.Unlock()
	if secondBrowserCtx != firstBrowserCtx {
		t.Error("expected second Regenerate to reuse the same persistent browserCtx")
	}

	for _, dateStr := range []string{"2026-06-15", "2026-06-16"} {
		elemBytes, err := os.ReadFile(cfg.ScreenshotPath(dateStr))
		if err != nil {
			t.Fatalf("read element screenshot %s: %v", dateStr, err)
		}
		elemImg, err := png.Decode(bytes.NewReader(elemBytes))
		if err != nil {
			t.Fatalf("decode element png %s: %v", dateStr, err)
		}
		if b := elemImg.Bounds(); b.Dx() != 540 || b.Dy() != 960 {
			t.Errorf("element screenshot %s dimensions = %dx%d, want 540x960", dateStr, b.Dx(), b.Dy())
		}

		raw, err := os.ReadFile(cfg.ImagePath(dateStr))
		if err != nil {
			t.Fatalf("read 4bpp image %s: %v", dateStr, err)
		}
		if len(raw) != 259200 {
			t.Errorf("len(raw) for %s = %d, want 259200", dateStr, len(raw))
		}
	}

	// Verify widgetReadyScriptJS behavior on the active persistent Chromium process.
	widgetCtx, widgetCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer widgetCancel()

	t.Run("widget readiness waits for ww container hydration", func(t *testing.T) {
		sess, err := factory.NewSession(widgetCtx)
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer sess.Close()
		if err := sess.Get(widgetCtx, ts.URL+"/ww-hydrated"); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := sess.WaitForElement(widgetCtx, "screenshot_content", waitElementTimeout); err != nil {
			t.Fatalf("WaitForElement: %v", err)
		}
		waiter := sess.(readinessWaiter)
		start := time.Now()
		if err := waiter.WaitReady(widgetCtx, 2*time.Second); err != nil {
			t.Fatalf("WaitReady failed: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
			t.Errorf("WaitReady returned in %v before async ww hydration", elapsed)
		}
	})

	t.Run("widget readiness falls back gracefully on unhydrated ww container", func(t *testing.T) {
		sess, err := factory.NewSession(widgetCtx)
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer sess.Close()
		if err := sess.Get(widgetCtx, ts.URL+"/ww-unhydrated"); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := sess.WaitForElement(widgetCtx, "screenshot_content", waitElementTimeout); err != nil {
			t.Fatalf("WaitForElement: %v", err)
		}
		waiter := sess.(readinessWaiter)
		if err := waiter.WaitReady(widgetCtx, 200*time.Millisecond); err == nil {
			t.Fatal("expected WaitReady timeout for unhydrated ww container")
		}

		waitCfg := newTestConfig(t)
		waitCfg.WeatherWidgetHTML = `<div id="ww_stuck"><a href="https://weatherwidget.org/">Fallback</a></div>`
		waitSvc := NewService(waitCfg, factory, fakeConverter{writeSha: true})
		waitSvc.settleDelay = 150 * time.Millisecond
		if err := waitSvc.waitForWeatherWidget(widgetCtx, sess); err != nil {
			t.Fatalf("waitForWeatherWidget should fall back gracefully on timeout, got %v", err)
		}
	})

	t.Run("widget readiness returns immediately for static weather widget", func(t *testing.T) {
		sess, err := factory.NewSession(widgetCtx)
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer sess.Close()
		if err := sess.Get(widgetCtx, ts.URL+"/static-widget"); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := sess.WaitForElement(widgetCtx, "screenshot_content", waitElementTimeout); err != nil {
			t.Fatalf("WaitForElement: %v", err)
		}
		waiter := sess.(readinessWaiter)
		if err := waiter.WaitReady(widgetCtx, 2*time.Second); err != nil {
			t.Fatalf("WaitReady failed for static widget: %v", err)
		}
	})

	t.Run("widget readiness waits for script-driven DOM population", func(t *testing.T) {
		sess, err := factory.NewSession(widgetCtx)
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		defer sess.Close()
		if err := sess.Get(widgetCtx, ts.URL+"/script-hydrated"); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := sess.WaitForElement(widgetCtx, "screenshot_content", waitElementTimeout); err != nil {
			t.Fatalf("WaitForElement: %v", err)
		}
		waiter := sess.(readinessWaiter)
		start := time.Now()
		if err := waiter.WaitReady(widgetCtx, 2*time.Second); err != nil {
			t.Fatalf("WaitReady failed for script widget: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
			t.Errorf("WaitReady returned in %v before script hydration", elapsed)
		}
	})

	if err := svc.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
	if _, err := factory.NewSession(context.Background()); !errors.Is(err, ErrServiceClosed) {
		t.Errorf("post-Close NewSession err = %v, want ErrServiceClosed", err)
	}
}

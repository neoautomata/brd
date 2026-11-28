package capture

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/neoautomata/brd/internal/atomicfile"
	"github.com/neoautomata/brd/internal/config"
	"github.com/neoautomata/brd/internal/imgconv"
)

const (
	captureTimeout      = 60 * time.Second
	navigateTimeout     = 30 * time.Second
	waitElementTimeout  = 15 * time.Second
	screenshotTimeout   = 15 * time.Second
	closeGraceTimeout   = 5 * time.Second
	widgetSettleDelay   = 2 * time.Second
	widgetPollInterval  = 50 * time.Millisecond
	staleTempMaxAge     = 1 * time.Hour
	widgetReadyScriptJS = `(() => {
		if (document.readyState !== 'complete') return false;
		const root = document.getElementById('screenshot_content');
		if (!root) return false;
		const isRenderedEl = (el) => {
			const tag = el.tagName;
			if (tag === 'A' || tag === 'SCRIPT' || tag === 'STYLE' || tag === 'LINK' || tag === 'NOSCRIPT') {
				return false;
			}
			return (el.textContent && el.textContent.trim().length > 0) || el.querySelector('img, svg, canvas, iframe') !== null;
		};
		const widget = root.querySelector('.weather-widget');
		if (widget) {
			const wwNodes = widget.querySelectorAll('[id^="ww_"]');
			if (wwNodes.length > 0) {
				for (const ww of wwNodes) {
					const hasWWDom = ww.querySelector('.ww-box, .ww_cond, .ww_line, .ww_hour') !== null;
					const hasRenderedChild = Array.from(ww.children).some(isRenderedEl);
					if (!hasWWDom && !hasRenderedChild) return false;
				}
			} else if (widget.querySelector('script')) {
				if (!Array.from(widget.children).some(isRenderedEl)) return false;
			}
		}
		for (const img of root.querySelectorAll('img')) {
			if (!img.complete) return false;
		}
		for (const frame of root.querySelectorAll('iframe')) {
			try {
				if (frame.contentDocument && frame.contentDocument.readyState !== 'complete') return false;
			} catch (e) {}
		}
		return true;
	})()`
)

// ErrServiceClosed reports that a caller invoked Regenerate after Service.Close.
var ErrServiceClosed = errors.New("capture service is closed")

// BrowserSession abstracts headless browser calls needed to capture screenshots.
type BrowserSession interface {
	Get(ctx context.Context, url string) error
	WaitForElement(ctx context.Context, id string, timeout time.Duration) error
	WindowScreenshot(ctx context.Context) ([]byte, error)
	ElementScreenshot(ctx context.Context, id string) ([]byte, error)
	Close() error
}

type readinessWaiter interface {
	WaitReady(ctx context.Context, timeout time.Duration) error
}

// BrowserFactory creates a BrowserSession and manages browser resources.
type BrowserFactory interface {
	NewSession(ctx context.Context) (BrowserSession, error)
	Close() error
}

// Converter transforms a PNG screenshot into a packed 4bpp image file.
type Converter interface {
	Convert(ctx context.Context, inputPNG, outputImg string) error
}

// GoConverter converts PNG screenshots into 4bpp framebuffers using pure Go.
type GoConverter struct {
	Options imgconv.Options
}

// Convert decodes inputPNG and writes outputImg and outputImg+".sha" atomically.
func (gc GoConverter) Convert(ctx context.Context, inputPNG, outputImg string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return imgconv.ConvertFile(inputPNG, outputImg, gc.Options)
}

// ChromedpFactory manages a persistent headless Chromium process using Chrome DevTools Protocol.
type ChromedpFactory struct {
	ExecPath  string
	NoSandbox bool

	mu            sync.Mutex
	closed        bool
	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
}

func (cf *ChromedpFactory) allocatorFlags() map[string]any {
	flags := map[string]any{
		"window-size":               "1080,1920",
		"disable-gpu":               true,
		"force-device-scale-factor": "1",
		"hide-scrollbars":           true,
	}
	if cf.NoSandbox {
		flags["no-sandbox"] = true
	}
	return flags
}

func (cf *ChromedpFactory) allocatorOptions() []chromedp.ExecAllocatorOption {
	opts := make([]chromedp.ExecAllocatorOption, 0, len(chromedp.DefaultExecAllocatorOptions)+6)
	opts = append(opts, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.WindowSize(1080, 1920),
		chromedp.DisableGPU,
		chromedp.Flag("force-device-scale-factor", "1"),
		chromedp.Flag("hide-scrollbars", true),
	)
	if cf.NoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	if cf.ExecPath != "" {
		opts = append(opts, chromedp.ExecPath(cf.ExecPath))
	}
	return opts
}

func (cf *ChromedpFactory) ensureBrowserLocked(ctx context.Context) (context.Context, error) {
	if cf.closed {
		return nil, ErrServiceClosed
	}
	if cf.browserCtx != nil && cf.browserCtx.Err() == nil {
		return cf.browserCtx, nil
	}

	cf.stopBrowserLocked()

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), cf.allocatorOptions()...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)

	// Bind browser lifetime to browserCtx while honoring ctx cancellation during startup.
	stop := context.AfterFunc(ctx, browserCancel)
	err := chromedp.Do(browserCtx)
	if !stop() {
		browserCancel()
		allocCancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("start headless browser: %w", err)
	}
	if err != nil {
		browserCancel()
		allocCancel()
		return nil, fmt.Errorf("start headless browser: %w", err)
	}

	cf.allocCtx = allocCtx
	cf.allocCancel = allocCancel
	cf.browserCtx = browserCtx
	cf.browserCancel = browserCancel
	return browserCtx, nil
}

func (cf *ChromedpFactory) stopBrowserLocked() error {
	browserCtx := cf.browserCtx
	browserCancel := cf.browserCancel
	allocCancel := cf.allocCancel
	cf.allocCtx = nil
	cf.allocCancel = nil
	cf.browserCtx = nil
	cf.browserCancel = nil

	var closeErr error
	if browserCtx != nil && browserCtx.Err() == nil {
		graceCtx, graceCancel := context.WithTimeout(browserCtx, closeGraceTimeout)
		closeErr = chromedp.Cancel(graceCtx)
		graceCancel()
	}
	if browserCancel != nil {
		browserCancel()
	}
	if allocCancel != nil {
		allocCancel()
	}
	return closeErr
}

func (cf *ChromedpFactory) resetBrowser(staleBrowserCtx context.Context) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	if cf.browserCtx != staleBrowserCtx || cf.browserCtx == nil {
		return
	}
	_ = cf.stopBrowserLocked()
}

func (cf *ChromedpFactory) openTab(ctx context.Context, browserCtx context.Context) (*chromedpSession, error) {
	tabCtx, tabCancel := chromedp.NewContext(browserCtx, chromedp.WithNewWindow(false))
	stop := context.AfterFunc(ctx, tabCancel)
	err := chromedp.Do(tabCtx)
	if !stop() {
		tabCancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("open browser tab: %w", err)
	}
	if err != nil {
		tabCancel()
		return nil, fmt.Errorf("open browser tab: %w", err)
	}
	return &chromedpSession{
		factory:    cf,
		browserCtx: browserCtx,
		tabCtx:     tabCtx,
		tabCancel:  tabCancel,
	}, nil
}

// NewSession opens a tab in the persistent headless browser and restarts the browser if it exited.
func (cf *ChromedpFactory) NewSession(ctx context.Context) (BrowserSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		cf.mu.Lock()
		browserCtx, err := cf.ensureBrowserLocked(ctx)
		cf.mu.Unlock()
		if err != nil {
			return nil, err
		}

		sess, tabErr := cf.openTab(ctx, browserCtx)
		if tabErr == nil {
			return sess, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Restart the browser once when tab creation fails on a stale browser process.
		cf.resetBrowser(browserCtx)
		if attempt == 1 {
			return nil, tabErr
		}
	}
	return nil, errors.New("start headless browser failed")
}

// Close stops the persistent headless browser and releases allocator resources.
func (cf *ChromedpFactory) Close() error {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	cf.closed = true
	return cf.stopBrowserLocked()
}

type chromedpSession struct {
	factory    *ChromedpFactory
	browserCtx context.Context
	tabCtx     context.Context
	tabCancel  context.CancelFunc
	failed     atomic.Bool
	closeOnce  sync.Once
	closeErr   error
}

func (s *chromedpSession) stepContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	stepCtx, cancel := context.WithTimeout(s.tabCtx, timeout)
	stop := context.AfterFunc(parent, cancel)
	return stepCtx, func() {
		stop()
		cancel()
	}
}

func (s *chromedpSession) recordStepError(parent context.Context, err error) {
	if err != nil && parent.Err() == nil {
		s.failed.Store(true)
	}
}

func (s *chromedpSession) Get(ctx context.Context, targetURL string) error {
	stepCtx, cancel := s.stepContext(ctx, navigateTimeout)
	defer cancel()
	err := chromedp.Do(stepCtx, chromedp.Navigate(targetURL))
	s.recordStepError(ctx, err)
	return err
}

func (s *chromedpSession) WaitForElement(ctx context.Context, id string, timeout time.Duration) error {
	stepCtx, cancel := s.stepContext(ctx, timeout)
	defer cancel()
	err := chromedp.Do(stepCtx, chromedp.WaitVisible(chromedp.ID(id)))
	s.recordStepError(ctx, err)
	return err
}

func (s *chromedpSession) WaitReady(ctx context.Context, timeout time.Duration) error {
	stepCtx, cancel := s.stepContext(ctx, timeout)
	defer cancel()
	return chromedp.Do(stepCtx, chromedp.Poll[chromedp.Void](
		widgetReadyScriptJS,
		chromedp.WithPollingInterval(widgetPollInterval),
		chromedp.WithPollingTimeout(timeout),
	))
}

func (s *chromedpSession) WindowScreenshot(ctx context.Context) ([]byte, error) {
	stepCtx, cancel := s.stepContext(ctx, screenshotTimeout)
	defer cancel()
	data, err := chromedp.Run(stepCtx, chromedp.CaptureScreenshot())
	s.recordStepError(ctx, err)
	return data, err
}

func (s *chromedpSession) ElementScreenshot(ctx context.Context, id string) ([]byte, error) {
	stepCtx, cancel := s.stepContext(ctx, screenshotTimeout)
	defer cancel()
	data, err := chromedp.Run(stepCtx, chromedp.Screenshot(chromedp.ID(id), chromedp.NodeVisible))
	s.recordStepError(ctx, err)
	return data, err
}

func (s *chromedpSession) Close() error {
	s.closeOnce.Do(func() {
		if s.tabCtx.Err() == nil {
			graceCtx, graceCancel := context.WithTimeout(s.tabCtx, closeGraceTimeout)
			s.closeErr = chromedp.Cancel(graceCtx)
			graceCancel()
		}
		s.tabCancel()
		if s.factory != nil && (s.failed.Load() || s.browserCtx.Err() != nil) {
			s.factory.resetBrowser(s.browserCtx)
		}
	})
	return s.closeErr
}

// Service serializes screenshot capture, image conversion, and file deletion.
type Service struct {
	cfg            *config.Config
	viewBaseURL    string
	browsers       BrowserFactory
	converter      Converter
	settleDelay    time.Duration
	captureTimeout time.Duration
	nowFn          func() time.Time

	stateMu   sync.Mutex
	closed    bool
	svcCtx    context.Context
	svcCancel context.CancelFunc

	sem       chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// NewService constructs a capture Service using the given configuration and dependencies.
func NewService(cfg *config.Config, browsers BrowserFactory, converter Converter) *Service {
	if cfg == nil {
		panic("capture.NewService: cfg must not be nil")
	}
	cfgCopy := *cfg
	if cfgCopy.Location == nil {
		cfgCopy.Location = time.UTC
	}
	if browsers == nil {
		browsers = &ChromedpFactory{
			ExecPath:  cfgCopy.ChromePath,
			NoSandbox: cfgCopy.ChromeNoSandbox,
		}
	}
	if converter == nil {
		converter = GoConverter{}
	}
	svcCtx, svcCancel := context.WithCancel(context.Background())
	return &Service{
		cfg:            &cfgCopy,
		viewBaseURL:    buildViewBaseURL(cfgCopy.LocalAddr),
		browsers:       browsers,
		converter:      converter,
		settleDelay:    widgetSettleDelay,
		captureTimeout: captureTimeout,
		nowFn:          time.Now,
		svcCtx:         svcCtx,
		svcCancel:      svcCancel,
		sem:            make(chan struct{}, 1),
	}
}

func buildViewBaseURL(localAddr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(localAddr))
	if err != nil {
		return "http://" + localAddr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (s *Service) isClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closed
}

// Close cancels active captures, waits for completion, and closes the browser factory.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		s.closed = true
		cancel := s.svcCancel
		s.stateMu.Unlock()
		if cancel != nil {
			cancel()
		}

		// Acquire the semaphore slot to wait for any active capture before closing browsers.
		if s.sem != nil {
			s.sem <- struct{}{}
			defer func() { <-s.sem }()
		}
		if s.browsers != nil {
			s.closeErr = s.browsers.Close()
		}
	})
	return s.closeErr
}

// Regenerate captures #screenshot_content for dateStr and writes a 4bpp image.
func (s *Service) Regenerate(ctx context.Context, dateStr string) error {
	if _, err := time.Parse("2006-01-02", dateStr); err != nil {
		return fmt.Errorf("invalid date %q: %w", dateStr, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.isClosed() {
		return ErrServiceClosed
	}

	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.svcCtx.Done():
		return ErrServiceClosed
	}
	defer func() { <-s.sem }()

	if s.isClosed() {
		return ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	runCtx, cancelRun := context.WithTimeout(ctx, s.captureTimeout)
	stopSvcCancel := context.AfterFunc(s.svcCtx, cancelRun)
	defer func() {
		stopSvcCancel()
		cancelRun()
	}()

	session, err := s.browsers.NewSession(runCtx)
	if err != nil {
		return fmt.Errorf("open browser session: %w", err)
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
			log.Printf("Close browser session failed: %v", closeErr)
		}
	}()

	targetURL := fmt.Sprintf("%s/?date=%s", s.viewBaseURL, dateStr)
	if err := session.Get(runCtx, targetURL); err != nil {
		return fmt.Errorf("navigate to view URL: %w", err)
	}

	if err := session.WaitForElement(runCtx, "screenshot_content", waitElementTimeout); err != nil {
		return fmt.Errorf("wait for screenshot_content: %w", err)
	}

	if err := s.waitForWeatherWidget(runCtx, session); err != nil {
		return err
	}

	todayStr := s.nowFn().In(s.cfg.Location).Format("2006-01-02")
	if dateStr == todayStr {
		winPNG, err := session.WindowScreenshot(runCtx)
		if err != nil {
			return fmt.Errorf("capture window screenshot: %w", err)
		}
		if err := atomicfile.Write(s.cfg.WindowScreenshotPath(dateStr), winPNG, 0600); err != nil {
			return fmt.Errorf("write window screenshot: %w", err)
		}
	}

	elemPNG, err := session.ElementScreenshot(runCtx, "screenshot_content")
	if err != nil {
		return fmt.Errorf("capture element screenshot: %w", err)
	}
	screenshotPath := s.cfg.ScreenshotPath(dateStr)
	if err := atomicfile.Write(screenshotPath, elemPNG, 0600); err != nil {
		return fmt.Errorf("write element screenshot: %w", err)
	}

	imgPath := s.cfg.ImagePath(dateStr)
	if err := s.converter.Convert(runCtx, screenshotPath, imgPath); err != nil {
		return fmt.Errorf("convert screenshot: %w", err)
	}

	// The firmware uses this SHA-256 digest only to detect display changes, not as a security integrity check.
	// The firmware skips local hashing to save CPU time and power.
	hashPath := s.cfg.HashPath(dateStr)
	hexHash, err := hashFileSHA256(imgPath)
	if err != nil {
		return fmt.Errorf("hash converted image: %w", err)
	}
	existingHash, err := os.ReadFile(hashPath)
	if err != nil || strings.TrimSpace(string(existingHash)) != hexHash {
		if err := atomicfile.Write(hashPath, []byte(hexHash), 0600); err != nil {
			return fmt.Errorf("write image hash: %w", err)
		}
	}

	return nil
}

func (s *Service) waitForWeatherWidget(ctx context.Context, session BrowserSession) error {
	if strings.TrimSpace(s.cfg.WeatherWidgetHTML) == "" || s.settleDelay <= 0 {
		return nil
	}
	if waiter, ok := session.(readinessWaiter); ok {
		if err := waiter.WaitReady(ctx, s.settleDelay); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("Warning: wait for weather widget readiness exceeded the timeout: %v", err)
		}
		return nil
	}

	timer := time.NewTimer(s.settleDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// CleanupOldFiles deletes dated content, screenshot, and image files older than RetentionDays.
func (s *Service) CleanupOldFiles(now time.Time) {
	localNow := now.In(s.cfg.Location)
	cutoff := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, s.cfg.Location).AddDate(0, 0, -s.cfg.RetentionDays)
	dirs := []string{
		s.cfg.ContentsDir(),
		s.cfg.ScreenshotsDir(),
		s.cfg.ImagesDir(),
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.Printf("Read directory %q failed: %v", dir, err)
			}
			continue
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".tmp-") {
				info, err := entry.Info()
				if err != nil || now.Sub(info.ModTime()) < staleTempMaxAge {
					continue
				}
				targetPath := filepath.Join(dir, name)
				if err := os.Remove(targetPath); err != nil {
					log.Printf("Delete stale temp file %q failed: %v", targetPath, err)
				} else {
					log.Printf("Deleted stale temp file %s", targetPath)
				}
				continue
			}
			dateStr := strings.Split(strings.Split(name, ".")[0], "_")[0]
			if len(dateStr) != len("2006-01-02") {
				continue
			}
			dt, err := time.ParseInLocation("2006-01-02", dateStr, s.cfg.Location)
			if err != nil || !dt.Before(cutoff) {
				continue
			}
			targetPath := filepath.Join(dir, entry.Name())
			if err := os.Remove(targetPath); err != nil {
				log.Printf("Delete expired file %q failed: %v", targetPath, err)
			} else {
				log.Printf("Deleted expired file %s", targetPath)
			}
		}
	}
}

func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

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
	"time"

	"github.com/chromedp/chromedp"

	"github.com/neoautomata/brd/internal/atomicfile"
	"github.com/neoautomata/brd/internal/config"
	"github.com/neoautomata/brd/internal/imgconv"
)

const (
	captureTimeout     = 35 * time.Second
	navigateTimeout    = 20 * time.Second
	waitElementTimeout = 10 * time.Second
	screenshotTimeout  = 10 * time.Second
	closeGraceTimeout  = 5 * time.Second
	widgetSettleDelay  = 1 * time.Second
)

// ErrServiceClosed indicates Regenerate was called after Service.Close.
var ErrServiceClosed = errors.New("capture service is closed")

// BrowserSession abstracts headless browser calls needed to capture screenshots.
type BrowserSession interface {
	Get(ctx context.Context, url string) error
	WaitForElement(ctx context.Context, id string, timeout time.Duration) error
	WindowScreenshot(ctx context.Context) ([]byte, error)
	ElementScreenshot(ctx context.Context, id string) ([]byte, error)
	Close() error
}

// BrowserFactory creates a BrowserSession and manages any browser resources.
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

// Convert decodes inputPNG and atomically writes outputImg and outputImg+".sha".
func (gc GoConverter) Convert(ctx context.Context, inputPNG, outputImg string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return imgconv.ConvertFile(inputPNG, outputImg, gc.Options)
}

// ChromedpFactory starts isolated headless Chromium sessions using Chrome DevTools Protocol.
type ChromedpFactory struct {
	ExecPath string
}

// NewSession starts a headless Chromium process bound to ctx and returns a BrowserSession.
func (cf *ChromedpFactory) NewSession(ctx context.Context) (BrowserSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts := make([]chromedp.ExecAllocatorOption, 0, len(chromedp.DefaultExecAllocatorOptions)+6)
	opts = append(opts, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.WindowSize(1080, 1920),
		chromedp.DisableGPU,
		chromedp.NoSandbox,
		chromedp.Flag("force-device-scale-factor", "1"),
		chromedp.Flag("hide-scrollbars", true),
	)
	if cf.ExecPath != "" {
		opts = append(opts, chromedp.ExecPath(cf.ExecPath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)

	// Start the browser immediately on tabCtx so later per-step timeouts do not kill the browser.
	if err := chromedp.Do(tabCtx); err != nil {
		tabCancel()
		allocCancel()
		return nil, fmt.Errorf("start headless browser: %w", err)
	}

	return &chromedpSession{
		tabCtx:      tabCtx,
		tabCancel:   tabCancel,
		allocCancel: allocCancel,
	}, nil
}

// Close satisfies BrowserFactory; per-session allocators clean up their own browser processes.
func (cf *ChromedpFactory) Close() error {
	return nil
}

type chromedpSession struct {
	tabCtx      context.Context
	tabCancel   context.CancelFunc
	allocCancel context.CancelFunc
	closeOnce   sync.Once
	closeErr    error
}

func (s *chromedpSession) stepContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	stepCtx, cancel := context.WithTimeout(s.tabCtx, timeout)
	stop := context.AfterFunc(parent, cancel)
	return stepCtx, func() {
		stop()
		cancel()
	}
}

func (s *chromedpSession) Get(ctx context.Context, targetURL string) error {
	stepCtx, cancel := s.stepContext(ctx, navigateTimeout)
	defer cancel()
	return chromedp.Do(stepCtx, chromedp.Navigate(targetURL))
}

func (s *chromedpSession) WaitForElement(ctx context.Context, id string, timeout time.Duration) error {
	stepCtx, cancel := s.stepContext(ctx, timeout)
	defer cancel()
	return chromedp.Do(stepCtx, chromedp.WaitVisible(chromedp.ID(id)))
}

func (s *chromedpSession) WindowScreenshot(ctx context.Context) ([]byte, error) {
	stepCtx, cancel := s.stepContext(ctx, screenshotTimeout)
	defer cancel()
	return chromedp.Run(stepCtx, chromedp.CaptureScreenshot())
}

func (s *chromedpSession) ElementScreenshot(ctx context.Context, id string) ([]byte, error) {
	stepCtx, cancel := s.stepContext(ctx, screenshotTimeout)
	defer cancel()
	return chromedp.Run(stepCtx, chromedp.Screenshot(chromedp.ID(id), chromedp.NodeVisible))
}

func (s *chromedpSession) Close() error {
	s.closeOnce.Do(func() {
		if s.tabCtx.Err() == nil {
			graceCtx, graceCancel := context.WithTimeout(s.tabCtx, closeGraceTimeout)
			s.closeErr = chromedp.Cancel(graceCtx)
			graceCancel()
		}
		s.tabCancel()
		s.allocCancel()
	})
	return s.closeErr
}

// Service serializes screenshot capture, image conversion, and retention cleanup.
type Service struct {
	cfg            *config.Config
	viewBaseURL    string
	browsers       BrowserFactory
	converter      Converter
	settleDelay    time.Duration
	captureTimeout time.Duration

	stateMu   sync.Mutex
	closed    bool
	svcCtx    context.Context
	svcCancel context.CancelFunc

	mu sync.Mutex
}

// NewService constructs a capture Service using the given configuration and dependencies.
func NewService(cfg *config.Config, browsers BrowserFactory, converter Converter) *Service {
	if browsers == nil {
		browsers = &ChromedpFactory{
			ExecPath: cfg.ChromePath,
		}
	}
	if converter == nil {
		converter = GoConverter{}
	}
	svcCtx, svcCancel := context.WithCancel(context.Background())
	return &Service{
		cfg:            cfg,
		viewBaseURL:    buildViewBaseURL(cfg.LocalAddr),
		browsers:       browsers,
		converter:      converter,
		settleDelay:    widgetSettleDelay,
		captureTimeout: captureTimeout,
		svcCtx:         svcCtx,
		svcCancel:      svcCancel,
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

// Close cancels any active capture, waits for it to release resources, and closes the browser factory.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.stateMu.Lock()
	s.closed = true
	cancel := s.svcCancel
	s.stateMu.Unlock()
	if cancel != nil {
		cancel()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.browsers == nil {
		return nil
	}
	return s.browsers.Close()
}

// Regenerate captures the #screenshot_content element for dateStr and converts it to a 4bpp image.
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

	s.mu.Lock()
	defer s.mu.Unlock()

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

	if s.settleDelay > 0 {
		timer := time.NewTimer(s.settleDelay)
		select {
		case <-runCtx.Done():
			timer.Stop()
			return runCtx.Err()
		case <-timer.C:
		}
	}

	winPNG, err := session.WindowScreenshot(runCtx)
	if err != nil {
		return fmt.Errorf("capture window screenshot: %w", err)
	}
	if err := atomicfile.Write(s.cfg.WindowScreenshotPath(dateStr), winPNG, 0600); err != nil {
		return fmt.Errorf("write window screenshot: %w", err)
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

	hashPath := s.cfg.HashPath(dateStr)
	if _, err := os.Stat(hashPath); err != nil {
		hexHash, err := hashFileSHA256(imgPath)
		if err != nil {
			return fmt.Errorf("hash converted image: %w", err)
		}
		if err := atomicfile.Write(hashPath, []byte(hexHash), 0600); err != nil {
			return fmt.Errorf("write image hash: %w", err)
		}
	}

	return nil
}

// CleanupOldFiles removes dated content, screenshot, and image files older than RetentionDays.
func (s *Service) CleanupOldFiles(now time.Time) {
	thresh := now.In(s.cfg.Location).AddDate(0, 0, -s.cfg.RetentionDays)
	dirs := []string{
		s.cfg.ContentsDir(),
		s.cfg.ScreenshotsDir(),
		s.cfg.ImagesDir(),
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			dateStr := strings.Split(strings.Split(entry.Name(), ".")[0], "_")[0]
			if len(dateStr) != len("2006-01-02") {
				continue
			}
			dt, err := time.ParseInLocation("2006-01-02", dateStr, s.cfg.Location)
			if err != nil || dt.After(thresh) {
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

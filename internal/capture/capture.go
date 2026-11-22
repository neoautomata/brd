package capture

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tebeka/selenium"
	"github.com/tebeka/selenium/chrome"

	"github.com/neoautomata/brd/internal/atomicfile"
	"github.com/neoautomata/brd/internal/config"
	"github.com/neoautomata/brd/internal/imgconv"
)

const (
	waitElementTimeout = 10 * time.Second
	widgetSettleDelay  = 1 * time.Second
)

// BrowserSession abstracts WebDriver calls needed to capture screenshots.
type BrowserSession interface {
	Get(url string) error
	WaitForElement(id string, timeout time.Duration) error
	WindowScreenshot() ([]byte, error)
	ElementScreenshot(id string) ([]byte, error)
	Close() error
}

// BrowserFactory creates a BrowserSession and manages any driver subprocess.
type BrowserFactory interface {
	NewSession() (BrowserSession, error)
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

// SeleniumFactory manages a headless ChromeDriver subprocess and sessions.
type SeleniumFactory struct {
	DriverPath string
	Port       int

	mu      sync.Mutex
	service *selenium.Service
}

// NewSession opens a headless Chromium session, restarting ChromeDriver once if a prior service died.
func (sf *SeleniumFactory) NewSession() (BrowserSession, error) {
	if err := sf.ensureService(); err != nil {
		return nil, err
	}

	driver, err := sf.dialRemote()
	if err != nil {
		log.Printf("Selenium remote connect failed, restarting chromedriver: %v", err)
		sf.resetService()
		if startErr := sf.ensureService(); startErr != nil {
			return nil, fmt.Errorf("restart chromedriver: %w", startErr)
		}
		driver, err = sf.dialRemote()
		if err != nil {
			return nil, fmt.Errorf("connect selenium remote after restart: %w", err)
		}
	}
	return &seleniumSession{driver: driver}, nil
}

// Close stops the running ChromeDriver service if active.
func (sf *SeleniumFactory) Close() error {
	sf.mu.Lock()
	svc := sf.service
	sf.service = nil
	sf.mu.Unlock()
	if svc != nil {
		return svc.Stop()
	}
	return nil
}

func (sf *SeleniumFactory) ensureService() error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if sf.service != nil {
		return nil
	}
	svc, err := selenium.NewChromeDriverService(sf.DriverPath, sf.Port)
	if err != nil {
		return fmt.Errorf("start chromedriver service: %w", err)
	}
	sf.service = svc
	return nil
}

func (sf *SeleniumFactory) resetService() {
	sf.mu.Lock()
	svc := sf.service
	sf.service = nil
	sf.mu.Unlock()
	if svc != nil {
		_ = svc.Stop()
	}
}

func (sf *SeleniumFactory) dialRemote() (selenium.WebDriver, error) {
	caps := selenium.Capabilities{}
	caps.AddChrome(chrome.Capabilities{Args: []string{
		"--window-size=1080,1920",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--disable-gpu",
		"--headless",
	}})
	urlPrefix := fmt.Sprintf("http://127.0.0.1:%d/wd/hub", sf.Port)
	return selenium.NewRemote(caps, urlPrefix)
}

type seleniumSession struct {
	driver selenium.WebDriver
}

func (s *seleniumSession) Get(targetURL string) error {
	return s.driver.Get(targetURL)
}

func (s *seleniumSession) WaitForElement(id string, timeout time.Duration) error {
	return s.driver.WaitWithTimeout(func(wd selenium.WebDriver) (bool, error) {
		elem, err := wd.FindElement(selenium.ByID, id)
		if err != nil {
			return false, nil
		}
		displayed, err := elem.IsDisplayed()
		return displayed && err == nil, nil
	}, timeout)
}

func (s *seleniumSession) WindowScreenshot() ([]byte, error) {
	return s.driver.Screenshot()
}

func (s *seleniumSession) ElementScreenshot(id string) ([]byte, error) {
	elem, err := s.driver.FindElement(selenium.ByID, id)
	if err != nil {
		return nil, err
	}
	return elem.Screenshot(true)
}

func (s *seleniumSession) Close() error {
	return s.driver.Quit()
}

// Service serializes screenshot capture, image conversion, and retention cleanup.
type Service struct {
	cfg         *config.Config
	viewBaseURL string
	browsers    BrowserFactory
	converter   Converter
	settleDelay time.Duration

	mu sync.Mutex
}

// NewService constructs a capture Service using the given configuration and dependencies.
func NewService(cfg *config.Config, browsers BrowserFactory, converter Converter) *Service {
	if browsers == nil {
		browsers = &SeleniumFactory{
			DriverPath: cfg.ChromeDriverPath,
			Port:       cfg.ChromeDriverPort,
		}
	}
	if converter == nil {
		converter = GoConverter{}
	}
	return &Service{
		cfg:         cfg,
		viewBaseURL: buildViewBaseURL(cfg.LocalAddr),
		browsers:    browsers,
		converter:   converter,
		settleDelay: widgetSettleDelay,
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

// Close releases underlying browser driver resources.
func (s *Service) Close() error {
	if s == nil || s.browsers == nil {
		return nil
	}
	return s.browsers.Close()
}

// Regenerate captures the #screenshot_content element for dateStr and converts it to a 4bpp image.
func (s *Service) Regenerate(ctx context.Context, dateStr string) error {
	if _, err := time.Parse("2006-01-02", dateStr); err != nil {
		return fmt.Errorf("invalid date %q: %w", dateStr, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	session, err := s.browsers.NewSession()
	if err != nil {
		return fmt.Errorf("open browser session: %w", err)
	}
	defer session.Close()

	targetURL := fmt.Sprintf("%s/?date=%s", s.viewBaseURL, dateStr)
	if err := session.Get(targetURL); err != nil {
		return fmt.Errorf("navigate to view URL: %w", err)
	}

	if err := session.WaitForElement("screenshot_content", waitElementTimeout); err != nil {
		return fmt.Errorf("wait for screenshot_content: %w", err)
	}

	if s.settleDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.settleDelay):
		}
	}

	winPNG, err := session.WindowScreenshot()
	if err != nil {
		return fmt.Errorf("capture window screenshot: %w", err)
	}
	if err := atomicfile.Write(s.cfg.WindowScreenshotPath(dateStr), winPNG, 0600); err != nil {
		return fmt.Errorf("write window screenshot: %w", err)
	}

	elemPNG, err := session.ElementScreenshot("screenshot_content")
	if err != nil {
		return fmt.Errorf("capture element screenshot: %w", err)
	}
	screenshotPath := s.cfg.ScreenshotPath(dateStr)
	if err := atomicfile.Write(screenshotPath, elemPNG, 0600); err != nil {
		return fmt.Errorf("write element screenshot: %w", err)
	}

	imgPath := s.cfg.ImagePath(dateStr)
	if err := s.converter.Convert(ctx, screenshotPath, imgPath); err != nil {
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

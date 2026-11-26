package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimezone             = "UTC"
	defaultTailnetHostname      = "brd"
	defaultTailnetAddr          = ":80"
	defaultLocalAddr            = "127.0.0.1:1234"
	defaultRegenIntervalSeconds = 300
	defaultRetentionDays        = 7
	defaultTransitCacheSeconds  = 600
	defaultTransitMaxDepartures = 4
)

// TransitConfig holds public transit stationboard settings.
type TransitConfig struct {
	StationboardURL      string   `json:"stationboard_url"`
	CacheDurationSeconds int      `json:"cache_duration_seconds"`
	ExcludedStopID       string   `json:"excluded_stop_id"`
	ExcludedStopName     string   `json:"excluded_stop_name"`
	Lines                []string `json:"lines"`
	MaxDepartures        int      `json:"max_departures"`
}

// CacheDuration returns the transit cache lifetime as a time.Duration.
func (tc TransitConfig) CacheDuration() time.Duration {
	return time.Duration(tc.CacheDurationSeconds) * time.Second
}

// Config holds validated runtime settings for the brd server.
type Config struct {
	DataDir              string        `json:"data_dir"`
	Timezone             string        `json:"timezone"`
	TailnetHostname      string        `json:"tailnet_hostname"`
	TailnetAddr          string        `json:"tailnet_addr"`
	LocalAddr            string        `json:"local_addr"`
	TrustWebAuthHeader   bool          `json:"trust_webauth_header"`
	TrustedProxyTags     []string      `json:"trusted_proxy_tags"`
	ChromePath           string        `json:"chrome_path"`
	ChromeDriverPath     string        `json:"chromedriver_path"`
	ChromeDriverPort     int           `json:"chromedriver_port"`
	RegenIntervalSeconds int           `json:"regen_interval_seconds"`
	RetentionDays        int           `json:"retention_days"`
	AdminUsers           []string      `json:"admin_users"`
	EditorUsers          []string      `json:"editor_users"`
	Calendars            []string      `json:"calendars"`
	Transit              TransitConfig `json:"transit"`
	WeatherWidgetHTML    string        `json:"weather_widget_html"`

	Location *time.Location `json:"-"`
}

// Load reads, parses, and validates a JSON configuration file from path.
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("config path is empty")
	}
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	return Parse(data, filepath.Dir(absPath))
}

// Parse decodes raw JSON bytes into a validated Config.
// Relative paths resolve against baseDir when baseDir is non-empty.
func Parse(data []byte, baseDir string) (*Config, error) {
	cfg := &Config{
		Timezone:             defaultTimezone,
		TailnetHostname:      defaultTailnetHostname,
		TailnetAddr:          defaultTailnetAddr,
		LocalAddr:            defaultLocalAddr,
		RegenIntervalSeconds: defaultRegenIntervalSeconds,
		RetentionDays:        defaultRetentionDays,
		Transit: TransitConfig{
			CacheDurationSeconds: defaultTransitCacheSeconds,
			Lines:                []string{"9", "14"},
			MaxDepartures:        defaultTransitMaxDepartures,
		},
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decode config JSON: %w", err)
	}

	if err := cfg.validateAndNormalize(baseDir); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validateAndNormalize(baseDir string) error {
	if err := validatePathString("data_dir", c.DataDir); err != nil {
		return err
	}
	if !filepath.IsAbs(c.DataDir) && baseDir != "" {
		c.DataDir = filepath.Join(baseDir, c.DataDir)
	}
	absDataDir, err := filepath.Abs(filepath.Clean(c.DataDir))
	if err != nil {
		return fmt.Errorf("canonicalize data_dir: %w", err)
	}
	c.DataDir = absDataDir

	loc, err := time.LoadLocation(strings.TrimSpace(c.Timezone))
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", c.Timezone, err)
	}
	c.Timezone = strings.TrimSpace(c.Timezone)
	c.Location = loc

	c.TailnetHostname = strings.ToLower(strings.TrimSpace(c.TailnetHostname))
	if !isValidHostname(c.TailnetHostname) {
		return fmt.Errorf("invalid tailnet_hostname %q", c.TailnetHostname)
	}

	if err := validateAddr("tailnet_addr", c.TailnetAddr); err != nil {
		return err
	}
	if err := validateAddr("local_addr", c.LocalAddr); err != nil {
		return err
	}

	if trimmed := strings.TrimSpace(c.ChromePath); trimmed != "" {
		if err := validatePathString("chrome_path", trimmed); err != nil {
			return err
		}
		c.ChromePath = filepath.Clean(trimmed)
	} else {
		c.ChromePath = ""
	}

	if trimmed := strings.TrimSpace(c.ChromeDriverPath); trimmed != "" {
		if err := validatePathString("chromedriver_path", trimmed); err != nil {
			return err
		}
		c.ChromeDriverPath = filepath.Clean(trimmed)
	}
	if c.ChromeDriverPort != 0 && (c.ChromeDriverPort < 1 || c.ChromeDriverPort > 65535) {
		return fmt.Errorf("chromedriver_port %d is out of range", c.ChromeDriverPort)
	}
	if c.RegenIntervalSeconds <= 0 {
		return fmt.Errorf("regen_interval_seconds must be positive, got %d", c.RegenIntervalSeconds)
	}
	if c.RetentionDays <= 0 {
		return fmt.Errorf("retention_days must be positive, got %d", c.RetentionDays)
	}

	var normErr error
	c.AdminUsers, normErr = normalizeStringList("admin_users", c.AdminUsers, true)
	if normErr != nil {
		return normErr
	}
	c.EditorUsers, normErr = normalizeStringList("editor_users", c.EditorUsers, true)
	if normErr != nil {
		return normErr
	}
	c.Calendars, normErr = normalizeStringList("calendars", c.Calendars, false)
	if normErr != nil {
		return normErr
	}
	c.TrustedProxyTags, normErr = normalizeStringList("trusted_proxy_tags", c.TrustedProxyTags, true)
	if normErr != nil {
		return normErr
	}
	for i, tag := range c.TrustedProxyTags {
		if !strings.HasPrefix(tag, "tag:") || len(tag) <= len("tag:") {
			return fmt.Errorf("trusted_proxy_tags[%d] %q must start with \"tag:\"", i, tag)
		}
	}

	if c.Transit.CacheDurationSeconds <= 0 {
		return fmt.Errorf("transit.cache_duration_seconds must be positive, got %d", c.Transit.CacheDurationSeconds)
	}
	if c.Transit.MaxDepartures <= 0 {
		return fmt.Errorf("transit.max_departures must be positive, got %d", c.Transit.MaxDepartures)
	}
	if strings.TrimSpace(c.Transit.StationboardURL) != "" {
		u, err := url.Parse(strings.TrimSpace(c.Transit.StationboardURL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("invalid transit.stationboard_url %q", c.Transit.StationboardURL)
		}
		c.Transit.StationboardURL = u.String()
	}
	c.Transit.Lines, normErr = normalizeStringList("transit.lines", c.Transit.Lines, false)
	if normErr != nil {
		return normErr
	}

	return nil
}

// RegenInterval returns the image regeneration period as a time.Duration.
func (c *Config) RegenInterval() time.Duration {
	return time.Duration(c.RegenIntervalSeconds) * time.Second
}

// ContentsDir returns the directory that stores per-day HTML notes.
func (c *Config) ContentsDir() string { return filepath.Join(c.DataDir, "contents") }

// ScreenshotsDir returns the directory that stores captured PNG screenshots.
func (c *Config) ScreenshotsDir() string { return filepath.Join(c.DataDir, "screenshots") }

// ImagesDir returns the directory that stores converted 4bpp display images.
func (c *Config) ImagesDir() string { return filepath.Join(c.DataDir, "imgs") }

// TailscaleDir returns the directory that stores tsnet state.
func (c *Config) TailscaleDir() string { return filepath.Join(c.DataDir, "tailscale") }

// TokenFile returns the path to the OAuth2 token cache file.
func (c *Config) TokenFile() string { return filepath.Join(c.DataDir, "token.json") }

// RefreshTokenFile returns the path to the refresh timestamp marker file.
func (c *Config) RefreshTokenFile() string {
	return filepath.Join(c.DataDir, "refresh_tok_obtained.empty")
}

// ClientSecretFile returns the path to the Google OAuth2 client secret file.
func (c *Config) ClientSecretFile() string { return filepath.Join(c.DataDir, "client_secret.json") }

// ContentPath returns the HTML note path for a YYYY-MM-DD date string.
func (c *Config) ContentPath(dateStr string) string {
	return filepath.Join(c.ContentsDir(), dateStr+".html")
}

// ScreenshotPath returns the cropped element screenshot path for dateStr.
func (c *Config) ScreenshotPath(dateStr string) string {
	return filepath.Join(c.ScreenshotsDir(), dateStr+".png")
}

// WindowScreenshotPath returns the full browser screenshot path for dateStr.
func (c *Config) WindowScreenshotPath(dateStr string) string {
	return filepath.Join(c.ScreenshotsDir(), dateStr+"_window.png")
}

// ImagePath returns the converted 4bpp image path for dateStr.
func (c *Config) ImagePath(dateStr string) string {
	return filepath.Join(c.ImagesDir(), dateStr)
}

// HashPath returns the SHA-256 digest file path for dateStr.
func (c *Config) HashPath(dateStr string) string {
	return filepath.Join(c.ImagesDir(), dateStr+".sha")
}

func validatePathString(field, val string) error {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		return fmt.Errorf("%s is required", field)
	}
	if strings.ContainsRune(trimmed, '\x00') {
		return fmt.Errorf("%s contains null byte", field)
	}
	return nil
}

func validateAddr(field, addr string) error {
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", field, addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port in %s %q", field, addr)
	}
	return nil
}

func isValidHostname(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

func normalizeStringList(field string, items []string, lower bool) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			return nil, fmt.Errorf("%s[%d] is empty", field, i)
		}
		if lower {
			trimmed = strings.ToLower(trimmed)
		}
		out = append(out, trimmed)
	}
	return out, nil
}

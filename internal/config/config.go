package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	maxRegenIntervalSeconds = 604800 // 7 days
	maxRetentionDays        = 3650   // 10 years
	maxTransitCacheSeconds  = 86400  // 24 hours
)

// TransitConfig holds stationboard settings for public transit.
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
	ChromeNoSandbox      bool          `json:"chrome_no_sandbox"`
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
// Parse resolves relative paths against baseDir when baseDir is not empty.
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
			MaxDepartures:        defaultTransitMaxDepartures,
		},
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decode config JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, errors.New("decode config JSON: trailing data after JSON object")
	}

	if err := cfg.validateAndNormalize(baseDir); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validateAndNormalize(baseDir string) error {
	c.DataDir = strings.TrimSpace(c.DataDir)
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

	c.Timezone = strings.TrimSpace(c.Timezone)
	if c.Timezone == "" {
		return errors.New("timezone must not be empty")
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", c.Timezone, err)
	}
	c.Location = loc

	c.TailnetHostname = strings.ToLower(strings.TrimSpace(c.TailnetHostname))
	if !isValidHostname(c.TailnetHostname) {
		return fmt.Errorf("invalid tailnet_hostname %q", c.TailnetHostname)
	}

	c.TailnetAddr = strings.TrimSpace(c.TailnetAddr)
	if err := validateAddr("tailnet_addr", c.TailnetAddr); err != nil {
		return err
	}
	c.LocalAddr = strings.TrimSpace(c.LocalAddr)
	if err := validateLocalAddr("local_addr", c.LocalAddr); err != nil {
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

	if c.RegenIntervalSeconds <= 0 {
		return fmt.Errorf("regen_interval_seconds must be positive, got %d", c.RegenIntervalSeconds)
	}
	if c.RegenIntervalSeconds > maxRegenIntervalSeconds {
		return fmt.Errorf("regen_interval_seconds exceeds maximum %d, got %d", maxRegenIntervalSeconds, c.RegenIntervalSeconds)
	}
	if c.RetentionDays <= 0 {
		return fmt.Errorf("retention_days must be positive, got %d", c.RetentionDays)
	}
	if c.RetentionDays > maxRetentionDays {
		return fmt.Errorf("retention_days exceeds maximum %d, got %d", maxRetentionDays, c.RetentionDays)
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
	if len(c.TrustedProxyTags) > 0 && !c.TrustWebAuthHeader {
		return errors.New("trusted_proxy_tags requires trust_webauth_header to be true")
	}
	if c.TrustWebAuthHeader && len(c.TrustedProxyTags) == 0 {
		return errors.New("trust_webauth_header requires at least one entry in trusted_proxy_tags")
	}

	if c.Transit.CacheDurationSeconds <= 0 {
		return fmt.Errorf("transit.cache_duration_seconds must be positive, got %d", c.Transit.CacheDurationSeconds)
	}
	if c.Transit.CacheDurationSeconds > maxTransitCacheSeconds {
		return fmt.Errorf("transit.cache_duration_seconds exceeds maximum %d, got %d", maxTransitCacheSeconds, c.Transit.CacheDurationSeconds)
	}
	if c.Transit.MaxDepartures <= 0 {
		return fmt.Errorf("transit.max_departures must be positive, got %d", c.Transit.MaxDepartures)
	}
	c.Transit.StationboardURL = strings.TrimSpace(c.Transit.StationboardURL)
	if c.Transit.StationboardURL != "" {
		u, err := url.Parse(c.Transit.StationboardURL)
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

// ImagesDir returns the directory that stores 4bpp display images.
func (c *Config) ImagesDir() string { return filepath.Join(c.DataDir, "imgs") }

// TailscaleDir returns the directory that stores tsnet state.
func (c *Config) TailscaleDir() string { return filepath.Join(c.DataDir, "tailscale") }

// TokenFile returns the path to the cache file for OAuth2 tokens.
func (c *Config) TokenFile() string { return filepath.Join(c.DataDir, "token.json") }

// RefreshTokenFile returns the path to the marker file for refresh timestamps.
func (c *Config) RefreshTokenFile() string {
	return filepath.Join(c.DataDir, "refresh_tok_obtained.empty")
}

// ClientSecretFile returns the path to the client secret file for Google OAuth2.
func (c *Config) ClientSecretFile() string { return filepath.Join(c.DataDir, "client_secret.json") }

// ContentPath returns the HTML note path for a YYYY-MM-DD date string.
func (c *Config) ContentPath(dateStr string) string {
	return filepath.Join(c.ContentsDir(), dateStr+".html")
}

// ScreenshotPath returns the path to the cropped screenshot for dateStr.
func (c *Config) ScreenshotPath(dateStr string) string {
	return filepath.Join(c.ScreenshotsDir(), dateStr+".png")
}

// WindowScreenshotPath returns the path to the full browser screenshot for dateStr.
func (c *Config) WindowScreenshotPath(dateStr string) string {
	return filepath.Join(c.ScreenshotsDir(), dateStr+"_window.png")
}

// ImagePath returns the path to the 4bpp image for dateStr.
func (c *Config) ImagePath(dateStr string) string {
	return filepath.Join(c.ImagesDir(), dateStr)
}

// HashPath returns the path to the SHA-256 digest file for dateStr.
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

func splitAndValidateAddr(field, addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", 0, fmt.Errorf("invalid %s %q: %w", field, addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port in %s %q", field, addr)
	}
	return host, port, nil
}

func validateAddr(field, addr string) error {
	_, _, err := splitAndValidateAddr(field, addr)
	return err
}

func validateLocalAddr(field, addr string) error {
	host, _, err := splitAndValidateAddr(field, addr)
	if err != nil {
		return err
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%s %q must use a loopback host", field, addr)
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

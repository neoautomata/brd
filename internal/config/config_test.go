package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseValidConfig(t *testing.T) {
	raw := `{
		"data_dir": "/var/lib/brd/../brd",
		"timezone": "Europe/Zurich",
		"tailnet_hostname": "BRD-Host",
		"tailnet_addr": ":80",
		"local_addr": "127.0.0.1:1234",
		"trust_webauth_header": true,
		"trusted_proxy_tags": [" TAG:Caddy "],
		"chrome_no_sandbox": true,
		"regen_interval_seconds": 120,
		"retention_days": 14,
		"admin_users": [" Admin@Example.COM "],
		"editor_users": ["Editor@Example.COM"],
		"calendars": ["cal1@example.com"],
		"transit": {
			"stationboard_url": "  https://transport.opendata.ch/v1/stationboard?id=8591390&limit=30  ",
			"cache_duration_seconds": 300,
			"excluded_stop_id": "8503023",
			"excluded_stop_name": "Test Stop",
			"lines": ["9", "14"],
			"max_departures": 2
		}
	}`

	cfg, err := Parse([]byte(raw), "/opt/brd")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.DataDir != "/var/lib/brd" {
		t.Errorf("DataDir = %q, want /var/lib/brd", cfg.DataDir)
	}
	if cfg.TailnetHostname != "brd-host" {
		t.Errorf("TailnetHostname = %q, want brd-host", cfg.TailnetHostname)
	}
	if cfg.AdminUsers[0] != "admin@example.com" {
		t.Errorf("AdminUsers[0] = %q, want admin@example.com", cfg.AdminUsers[0])
	}
	if cfg.EditorUsers[0] != "editor@example.com" {
		t.Errorf("EditorUsers[0] = %q, want editor@example.com", cfg.EditorUsers[0])
	}
	if !cfg.TrustWebAuthHeader {
		t.Error("TrustWebAuthHeader = false, want true")
	}
	if len(cfg.TrustedProxyTags) != 1 || cfg.TrustedProxyTags[0] != "tag:caddy" {
		t.Errorf("TrustedProxyTags = %v, want [tag:caddy]", cfg.TrustedProxyTags)
	}
	if !cfg.ChromeNoSandbox {
		t.Error("ChromeNoSandbox = false, want true")
	}
	if cfg.RegenInterval() != 120*time.Second {
		t.Errorf("RegenInterval() = %v, want 2m", cfg.RegenInterval())
	}
	if cfg.Transit.CacheDuration() != 300*time.Second {
		t.Errorf("Transit.CacheDuration() = %v, want 5m", cfg.Transit.CacheDuration())
	}
	if cfg.Transit.StationboardURL != "https://transport.opendata.ch/v1/stationboard?id=8591390&limit=30" {
		t.Errorf("Transit.StationboardURL = %q", cfg.Transit.StationboardURL)
	}
	if cfg.Transit.MaxDepartures != 2 {
		t.Errorf("Transit.MaxDepartures = %d, want 2", cfg.Transit.MaxDepartures)
	}
	if cfg.Location == nil || cfg.Location.String() != "Europe/Zurich" {
		t.Errorf("Location = %v, want Europe/Zurich", cfg.Location)
	}
	if cfg.ContentPath("2026-06-15") != "/var/lib/brd/contents/2026-06-15.html" {
		t.Errorf("ContentPath = %q", cfg.ContentPath("2026-06-15"))
	}
	if cfg.ScreenshotPath("2026-06-15") != "/var/lib/brd/screenshots/2026-06-15.png" {
		t.Errorf("ScreenshotPath = %q", cfg.ScreenshotPath("2026-06-15"))
	}
	if cfg.WindowScreenshotPath("2026-06-15") != "/var/lib/brd/screenshots/2026-06-15_window.png" {
		t.Errorf("WindowScreenshotPath = %q", cfg.WindowScreenshotPath("2026-06-15"))
	}
	if cfg.ImagePath("2026-06-15") != "/var/lib/brd/imgs/2026-06-15" {
		t.Errorf("ImagePath = %q", cfg.ImagePath("2026-06-15"))
	}
	if cfg.HashPath("2026-06-15") != "/var/lib/brd/imgs/2026-06-15.sha" {
		t.Errorf("HashPath = %q", cfg.HashPath("2026-06-15"))
	}
	if cfg.TailscaleDir() != "/var/lib/brd/tailscale" {
		t.Errorf("TailscaleDir = %q", cfg.TailscaleDir())
	}
	if cfg.TokenFile() != "/var/lib/brd/token.json" {
		t.Errorf("TokenFile = %q", cfg.TokenFile())
	}
	if cfg.RefreshTokenFile() != "/var/lib/brd/refresh_tok_obtained.empty" {
		t.Errorf("RefreshTokenFile = %q", cfg.RefreshTokenFile())
	}
	if cfg.ClientSecretFile() != "/var/lib/brd/client_secret.json" {
		t.Errorf("ClientSecretFile = %q", cfg.ClientSecretFile())
	}

	relCfg, err := Parse([]byte(`{"data_dir": " ./data ", "local_addr": " 127.0.0.1:1234 ", "tailnet_addr": " :80 ", "chrome_path": " /usr/bin/chromium ", "transit": {"stationboard_url": "   "}}`), "/opt/brd")
	if err != nil {
		t.Fatalf("Parse relative data_dir failed: %v", err)
	}
	if relCfg.DataDir != "/opt/brd/data" {
		t.Errorf("relative DataDir = %q, want /opt/brd/data", relCfg.DataDir)
	}
	if relCfg.Timezone != "UTC" || relCfg.Location != time.UTC {
		t.Errorf("default Timezone/Location = %q/%v, want UTC", relCfg.Timezone, relCfg.Location)
	}
	if relCfg.ChromeNoSandbox {
		t.Error("default ChromeNoSandbox = true, want false")
	}
	if relCfg.LocalAddr != "127.0.0.1:1234" || relCfg.TailnetAddr != ":80" || relCfg.ChromePath != "/usr/bin/chromium" {
		t.Errorf("trimmed addrs/chrome_path unexpected: local=%q tailnet=%q chrome=%q",
			relCfg.LocalAddr, relCfg.TailnetAddr, relCfg.ChromePath)
	}
	if relCfg.Transit.StationboardURL != "" {
		t.Errorf("whitespace StationboardURL = %q, want empty", relCfg.Transit.StationboardURL)
	}
	if len(relCfg.Transit.Lines) != 0 {
		t.Errorf("default Transit.Lines = %v, want empty", relCfg.Transit.Lines)
	}
	if relCfg.Transit.MaxDepartures != 4 {
		t.Errorf("default Transit.MaxDepartures = %d, want 4", relCfg.Transit.MaxDepartures)
	}
}

func TestParseLoopbackAndUpperBoundsAccepted(t *testing.T) {
	loopbackAddrs := []string{
		"127.0.0.1:1234",
		"127.0.0.2:8080",
		"[::1]:1234",
		"localhost:1234",
		"LOCALHOST:65535",
	}
	for _, addr := range loopbackAddrs {
		t.Run("local_addr="+addr, func(t *testing.T) {
			raw := `{"data_dir":"/tmp/brd","local_addr":"` + addr + `"}`
			cfg, err := Parse([]byte(raw), "")
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", addr, err)
			}
			if cfg.LocalAddr != addr {
				t.Errorf("LocalAddr = %q, want %q", cfg.LocalAddr, addr)
			}
		})
	}

	t.Run("exact upper bounds accepted", func(t *testing.T) {
		raw := `{
			"data_dir": "/tmp/brd",
			"regen_interval_seconds": 604800,
			"retention_days": 3650,
			"transit": {"cache_duration_seconds": 86400}
		}`
		cfg, err := Parse([]byte(raw), "")
		if err != nil {
			t.Fatalf("Parse at exact upper bounds failed: %v", err)
		}
		if cfg.RegenIntervalSeconds != maxRegenIntervalSeconds || cfg.RetentionDays != maxRetentionDays || cfg.Transit.CacheDurationSeconds != maxTransitCacheSeconds {
			t.Errorf("unexpected bounded values: %+v", cfg)
		}
	})
}

func TestParseValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{
			name:    "missing data_dir",
			json:    `{}`,
			wantErr: "data_dir is required",
		},
		{
			name:    "data_dir null byte",
			json:    `{"data_dir":"/tmp/brd\u0000x"}`,
			wantErr: "data_dir contains null byte",
		},
		{
			name:    "chrome_path null byte",
			json:    `{"data_dir":"/tmp/brd","chrome_path":"/usr/bin/chrome\u0000x"}`,
			wantErr: "chrome_path contains null byte",
		},
		{
			name:    "explicit empty timezone",
			json:    `{"data_dir":"/tmp/brd","timezone":""}`,
			wantErr: "timezone must not be empty",
		},
		{
			name:    "whitespace-only timezone",
			json:    `{"data_dir":"/tmp/brd","timezone":"   "}`,
			wantErr: "timezone must not be empty",
		},
		{
			name:    "invalid timezone",
			json:    `{"data_dir":"/tmp/brd","timezone":"Mars/Olympus"}`,
			wantErr: "load timezone",
		},
		{
			name:    "invalid hostname with underscore",
			json:    `{"data_dir":"/tmp/brd","tailnet_hostname":"bad_host"}`,
			wantErr: "invalid tailnet_hostname",
		},
		{
			name:    "invalid hostname with leading dash",
			json:    `{"data_dir":"/tmp/brd","tailnet_hostname":"-brd"}`,
			wantErr: "invalid tailnet_hostname",
		},
		{
			name:    "invalid tailnet_addr format",
			json:    `{"data_dir":"/tmp/brd","tailnet_addr":"no-port"}`,
			wantErr: "invalid tailnet_addr",
		},
		{
			name:    "invalid tailnet_addr port out of range",
			json:    `{"data_dir":"/tmp/brd","tailnet_addr":":70000"}`,
			wantErr: "invalid port in tailnet_addr",
		},
		{
			name:    "invalid local_addr",
			json:    `{"data_dir":"/tmp/brd","local_addr":"not-a-host-port"}`,
			wantErr: "invalid local_addr",
		},
		{
			name:    "local_addr wildcard host rejected",
			json:    `{"data_dir":"/tmp/brd","local_addr":":1234"}`,
			wantErr: "must use a loopback host",
		},
		{
			name:    "local_addr 0.0.0.0 rejected",
			json:    `{"data_dir":"/tmp/brd","local_addr":"0.0.0.0:1234"}`,
			wantErr: "must use a loopback host",
		},
		{
			name:    "local_addr ipv6 unspecified rejected",
			json:    `{"data_dir":"/tmp/brd","local_addr":"[::]:1234"}`,
			wantErr: "must use a loopback host",
		},
		{
			name:    "local_addr LAN IP rejected",
			json:    `{"data_dir":"/tmp/brd","local_addr":"192.168.1.50:1234"}`,
			wantErr: "must use a loopback host",
		},
		{
			name:    "local_addr non-loopback hostname rejected",
			json:    `{"data_dir":"/tmp/brd","local_addr":"example.com:1234"}`,
			wantErr: "must use a loopback host",
		},
		{
			name:    "local_addr port zero rejected",
			json:    `{"data_dir":"/tmp/brd","local_addr":"127.0.0.1:0"}`,
			wantErr: "invalid port in local_addr",
		},
		{
			name:    "negative regen interval",
			json:    `{"data_dir":"/tmp/brd","regen_interval_seconds":-5}`,
			wantErr: "regen_interval_seconds must be positive",
		},
		{
			name:    "overflow regen interval",
			json:    `{"data_dir":"/tmp/brd","regen_interval_seconds":604801}`,
			wantErr: "regen_interval_seconds exceeds maximum",
		},
		{
			name:    "zero retention days",
			json:    `{"data_dir":"/tmp/brd","retention_days":0}`,
			wantErr: "retention_days must be positive",
		},
		{
			name:    "overflow retention days",
			json:    `{"data_dir":"/tmp/brd","retention_days":3651}`,
			wantErr: "retention_days exceeds maximum",
		},
		{
			name:    "legacy chromedriver_path rejected",
			json:    `{"data_dir":"/tmp/brd","chromedriver_path":"/usr/bin/chromedriver"}`,
			wantErr: "decode config JSON",
		},
		{
			name:    "legacy chromedriver_port rejected",
			json:    `{"data_dir":"/tmp/brd","chromedriver_port":9515}`,
			wantErr: "decode config JSON",
		},
		{
			name:    "trust_webauth_header true with empty trusted_proxy_tags",
			json:    `{"data_dir":"/tmp/brd","trust_webauth_header":true,"trusted_proxy_tags":[]}`,
			wantErr: "trust_webauth_header requires at least one entry in trusted_proxy_tags",
		},
		{
			name:    "trust_webauth_header false with non-empty trusted_proxy_tags",
			json:    `{"data_dir":"/tmp/brd","trust_webauth_header":false,"trusted_proxy_tags":["tag:caddy"]}`,
			wantErr: "trusted_proxy_tags requires trust_webauth_header to be true",
		},
		{
			name:    "invalid transit url scheme",
			json:    `{"data_dir":"/tmp/brd","transit":{"stationboard_url":"file:///etc/passwd","cache_duration_seconds":60}}`,
			wantErr: "invalid transit.stationboard_url",
		},
		{
			name:    "zero transit cache_duration_seconds",
			json:    `{"data_dir":"/tmp/brd","transit":{"cache_duration_seconds":0}}`,
			wantErr: "transit.cache_duration_seconds must be positive",
		},
		{
			name:    "overflow transit cache_duration_seconds",
			json:    `{"data_dir":"/tmp/brd","transit":{"cache_duration_seconds":86401}}`,
			wantErr: "transit.cache_duration_seconds exceeds maximum",
		},
		{
			name:    "negative transit max_departures",
			json:    `{"data_dir":"/tmp/brd","transit":{"max_departures":-1}}`,
			wantErr: "transit.max_departures must be positive",
		},
		{
			name:    "zero transit max_departures",
			json:    `{"data_dir":"/tmp/brd","transit":{"max_departures":0}}`,
			wantErr: "transit.max_departures must be positive",
		},
		{
			name:    "empty transit lines entry",
			json:    `{"data_dir":"/tmp/brd","transit":{"lines":["9","  "]}}`,
			wantErr: "transit.lines[1] is empty",
		},
		{
			name:    "empty admin user entry",
			json:    `{"data_dir":"/tmp/brd","admin_users":[" "]}`,
			wantErr: "admin_users[0] is empty",
		},
		{
			name:    "empty editor user entry",
			json:    `{"data_dir":"/tmp/brd","editor_users":[" "]}`,
			wantErr: "editor_users[0] is empty",
		},
		{
			name:    "empty calendar entry",
			json:    `{"data_dir":"/tmp/brd","calendars":["cal@example.com",""]}`,
			wantErr: "calendars[1] is empty",
		},
		{
			name:    "trusted proxy tag missing prefix",
			json:    `{"data_dir":"/tmp/brd","trust_webauth_header":true,"trusted_proxy_tags":["caddy"]}`,
			wantErr: `must start with "tag:"`,
		},
		{
			name:    "trusted proxy tag empty name after prefix",
			json:    `{"data_dir":"/tmp/brd","trust_webauth_header":true,"trusted_proxy_tags":["tag:"]}`,
			wantErr: `must start with "tag:"`,
		},
		{
			name:    "trusted proxy tag empty item",
			json:    `{"data_dir":"/tmp/brd","trust_webauth_header":true,"trusted_proxy_tags":[" "]}`,
			wantErr: "trusted_proxy_tags[0] is empty",
		},
		{
			name:    "unknown field rejected",
			json:    `{"data_dir":"/tmp/brd","unknown_key":true}`,
			wantErr: "decode config JSON",
		},
		{
			name:    "trailing JSON data rejected",
			json:    `{"data_dir":"/tmp/brd"}{"extra":1}`,
			wantErr: "trailing data",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.json), "")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	content := `{"data_dir": "./data"}`
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	wantDataDir := filepath.Join(dir, "data")
	if cfg.DataDir != wantDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, wantDataDir)
	}
	if cfg.Timezone != "UTC" {
		t.Errorf("default Timezone = %q, want UTC", cfg.Timezone)
	}
	if cfg.LocalAddr != "127.0.0.1:1234" {
		t.Errorf("default LocalAddr = %q, want 127.0.0.1:1234", cfg.LocalAddr)
	}

	if _, err := Load(""); err == nil {
		t.Error("Load with empty path should fail")
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("Load with missing file should fail")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json failed to load: %v", err)
	}
	if cfg.TrustWebAuthHeader {
		t.Error("config.example.json TrustWebAuthHeader = true, want false")
	}
	if len(cfg.TrustedProxyTags) != 0 {
		t.Errorf("config.example.json TrustedProxyTags = %v, want empty", cfg.TrustedProxyTags)
	}
	if cfg.WeatherWidgetHTML != "" {
		t.Errorf("config.example.json WeatherWidgetHTML = %q, want empty", cfg.WeatherWidgetHTML)
	}
	if cfg.ChromeNoSandbox {
		t.Error("config.example.json ChromeNoSandbox = true, want false")
	}
	if len(cfg.AdminUsers) != 1 || len(cfg.EditorUsers) != 1 || len(cfg.Calendars) != 6 {
		t.Errorf("unexpected counts: admins=%d editors=%d calendars=%d",
			len(cfg.AdminUsers), len(cfg.EditorUsers), len(cfg.Calendars))
	}
	for _, list := range [][]string{cfg.AdminUsers, cfg.EditorUsers, cfg.Calendars} {
		for _, item := range list {
			if strings.Contains(strings.ToLower(item), "@gmail.com") {
				t.Errorf("config.example.json contains @gmail.com placeholder: %q", item)
			}
		}
	}
}

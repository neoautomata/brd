package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseValidConfig(t *testing.T) {
	raw := `{
		"data_dir": "/var/lib/brd/../brd",
		"timezone": "Europe/Zurich",
		"tailnet_hostname": "BRD-Host",
		"tailnet_addr": ":80",
		"local_addr": "127.0.0.1:1234",
		"regen_interval_seconds": 120,
		"retention_days": 14,
		"admin_users": [" Admin@Example.COM "],
		"editor_users": ["Editor@Example.COM"],
		"trusted_proxy_tags": [" TAG:Caddy "],
		"calendars": ["cal1@example.com"],
		"transit": {
			"stationboard_url": "https://transport.opendata.ch/v1/stationboard?id=8591390&limit=30",
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
	if len(cfg.TrustedProxyTags) != 1 || cfg.TrustedProxyTags[0] != "tag:caddy" {
		t.Errorf("TrustedProxyTags = %v, want [tag:caddy]", cfg.TrustedProxyTags)
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

	relCfg, err := Parse([]byte(`{"data_dir": "./data"}`), "/opt/brd")
	if err != nil {
		t.Fatalf("Parse relative data_dir failed: %v", err)
	}
	if relCfg.DataDir != "/opt/brd/data" {
		t.Errorf("relative DataDir = %q, want /opt/brd/data", relCfg.DataDir)
	}
	if relCfg.Transit.MaxDepartures != 4 {
		t.Errorf("default Transit.MaxDepartures = %d, want 4", relCfg.Transit.MaxDepartures)
	}
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
			name:    "invalid local_addr",
			json:    `{"data_dir":"/tmp/brd","local_addr":"not-a-host-port"}`,
			wantErr: "invalid local_addr",
		},
		{
			name:    "negative regen interval",
			json:    `{"data_dir":"/tmp/brd","regen_interval_seconds":-5}`,
			wantErr: "regen_interval_seconds must be positive",
		},
		{
			name:    "zero retention days",
			json:    `{"data_dir":"/tmp/brd","retention_days":0}`,
			wantErr: "retention_days must be positive",
		},
		{
			name:    "out of range chromedriver port",
			json:    `{"data_dir":"/tmp/brd","chromedriver_port":70000}`,
			wantErr: "chromedriver_port",
		},
		{
			name:    "invalid transit url scheme",
			json:    `{"data_dir":"/tmp/brd","transit":{"stationboard_url":"file:///etc/passwd","cache_duration_seconds":60}}`,
			wantErr: "invalid transit.stationboard_url",
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
			name:    "empty admin user entry",
			json:    `{"data_dir":"/tmp/brd","admin_users":[" "]}`,
			wantErr: "admin_users[0] is empty",
		},
		{
			name:    "trusted proxy tag missing prefix",
			json:    `{"data_dir":"/tmp/brd","trusted_proxy_tags":["caddy"]}`,
			wantErr: `must start with "tag:"`,
		},
		{
			name:    "trusted proxy tag empty name after prefix",
			json:    `{"data_dir":"/tmp/brd","trusted_proxy_tags":["tag:"]}`,
			wantErr: `must start with "tag:"`,
		},
		{
			name:    "trusted proxy tag empty item",
			json:    `{"data_dir":"/tmp/brd","trusted_proxy_tags":[" "]}`,
			wantErr: "trusted_proxy_tags[0] is empty",
		},
		{
			name:    "unknown field rejected",
			json:    `{"data_dir":"/tmp/brd","unknown_key":true}`,
			wantErr: "decode config JSON",
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
	content := `{"data_dir": "` + filepath.Join(dir, "data") + `"}`
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
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
	if len(cfg.AdminUsers) != 1 || len(cfg.EditorUsers) != 1 || len(cfg.Calendars) != 6 {
		t.Errorf("unexpected counts: admins=%d editors=%d calendars=%d",
			len(cfg.AdminUsers), len(cfg.EditorUsers), len(cfg.Calendars))
	}
}

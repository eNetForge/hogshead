package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
data_dir = "/tmp/m"
listen = ["127.0.0.1:9000", "[::1]:9000"]
hostname = "s3.example.com"

[tls]
mode = "acme"
acme_email = "ops@example.com"

[limits]
auth_fail_window = "30s"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/tmp/m" || len(cfg.Listen) != 2 {
		t.Fatalf("unexpected: %+v", cfg)
	}
	if cfg.Admin.Socket != "/tmp/m/admin.sock" {
		t.Fatalf("admin socket should follow data_dir: %q", cfg.Admin.Socket)
	}
	if cfg.Limits.AuthFailWindow.Std() != 30*time.Second {
		t.Fatalf("duration parse: %v", cfg.Limits.AuthFailWindow)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if d := cfg.ACMEDomains(); len(d) != 2 || d[1] != "*.s3.example.com" {
		t.Fatalf("acme domains: %v", d)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("data_dir = \"/x\"\nbogus = 1\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestApplyEnv(t *testing.T) {
	cfg := Default("/d")
	cfg, err := ApplyEnv(cfg, []string{
		"HOGSHEAD_LISTEN=0.0.0.0:8000, 0.0.0.0:8001",
		"HOGSHEAD_TLS_MODE=file",
		"HOGSHEAD_TLS_CERT_FILE=/c",
		"HOGSHEAD_TLS_KEY_FILE=/k",
		"HOGSHEAD_TLS_DNS_API_TOKEN=abc",
		"UNRELATED=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Listen) != 2 || cfg.Listen[1] != "0.0.0.0:8001" {
		t.Fatalf("listen: %v", cfg.Listen)
	}
	if cfg.TLS.Mode != "file" || cfg.TLS.DNS.Settings["api_token"] != "abc" {
		t.Fatalf("unexpected: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEnv(cfg, []string{"HOGSHEAD_NOPE=1"}); err == nil {
		t.Fatal("expected error for unknown env key")
	}
	if _, err := ApplyEnv(cfg, []string{"HOGSHEAD_LOG_JSON=maybe"}); err == nil {
		t.Fatal("expected error for bad bool")
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Config){
		"no data dir":     func(c *Config) { c.DataDir = "" },
		"no listen":       func(c *Config) { c.Listen = nil },
		"bad listen":      func(c *Config) { c.Listen = []string{"nope"} },
		"bad log level":   func(c *Config) { c.LogLevel = "loud" },
		"bad tls mode":    func(c *Config) { c.TLS.Mode = "maybe" },
		"file no cert":    func(c *Config) { c.TLS.Mode = "file" },
		"acme no domains": func(c *Config) { c.TLS.Mode = "acme" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default("/d")
			mut(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if err := Default("/d").Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestRestartRequired(t *testing.T) {
	old := Default("/d")
	cur := old
	cur.LogLevel = "debug"
	if got := RestartRequired(old, cur); len(got) != 0 {
		t.Fatalf("log level is live: %v", got)
	}
	cur.Listen = []string{"0.0.0.0:1"}
	cur.Admin.Socket = "/x"
	got := RestartRequired(old, cur)
	if len(got) != 2 || got[0] != "listen" || got[1] != "admin.socket" {
		t.Fatalf("got %v", got)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	c := Default("/d")
	c.Hostname = "h"
	b, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Hostname != "h" || back.DataDir != "/d" {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestApplyEnvSkipsCommandLineVariables(t *testing.T) {
	cfg, err := ApplyEnv(Default("/data"), []string{
		"HOGSHEAD_CONFIG=/data/config.toml",
		"HOGSHEAD_SERVER=https://s3.example.com",
		"HOGSHEAD_INIT_ACCESS_KEY=AKIDEXAMPLE",
		"HOGSHEAD_TEST_LOG=1",
		"HOGSHEAD_NAME=cask1",
	})
	if err != nil {
		t.Fatalf("command line variables must not be read as settings: %v", err)
	}
	if cfg.Name != "cask1" {
		t.Fatalf("name = %q, want cask1", cfg.Name)
	}
	if _, err := ApplyEnv(Default("/data"), []string{"HOGSHEAD_NO_SUCH=1"}); err == nil {
		t.Fatal("unknown setting accepted")
	}
}

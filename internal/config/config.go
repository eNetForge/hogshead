// Package config loads the node-local configuration file and environment
// overrides. Only machine-local facts live here: everything else is server state
// stored in metadata.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// EnvPrefix is the prefix for environment variable overrides.
const EnvPrefix = "HOGSHEAD_"

// Config is the node-local configuration.
type Config struct {
	// Name is a human-friendly label for this server, shown in status, logs,
	// the UI and LAN discovery. Defaults to the machine's hostname.
	Name     string   `toml:"name"`
	DataDir  string   `toml:"data_dir"`
	Listen   []string `toml:"listen"`
	Hostname string   `toml:"hostname"`
	LogLevel string   `toml:"log_level"`
	LogJSON  bool     `toml:"log_json"`
	// AccessLog is a file that receives one JSON line per S3 request
	// (empty disables it). It applies live and is reopened on every config
	// reload, so logrotate's postrotate can send SIGHUP.
	AccessLog string `toml:"access_log"`

	TLS       TLS       `toml:"tls"`
	Limits    Limits    `toml:"limits"`
	Admin     Admin     `toml:"admin"`
	Storage   Storage   `toml:"storage"`
	Metrics   Metrics   `toml:"metrics"`
	Discovery Discovery `toml:"discovery"`
	Cellar    Cellar    `toml:"cellar"`
}

// Cellar holds the node-local part of cellar membership; everything else
// about a cellar is replicated state managed through the API and UI.
type Cellar struct {
	// Advertise is the URL other members use to reach this server
	// (scheme://host:port). Empty derives it from the first non-loopback
	// address and the listen port.
	Advertise string `toml:"advertise"`
}

// Discovery controls the LAN beacon that lets servers find each other.
type Discovery struct {
	// Enabled announces this server on the local network (UDP multicast
	// 239.255.77.77:9470) and lists peers heard there. On by default.
	Enabled bool `toml:"enabled"`
}

// Storage tunes the local chunk store.
type Storage struct {
	// Compress enables zstd compression of chunks that shrink by at least 1/16.
	Compress bool `toml:"compress"`
	// ChunkSize in bytes; default 4 MiB.
	ChunkSize int `toml:"chunk_size"`
	// KeyFile holds the 32-byte master key for encryption at rest. Default
	// <data_dir>/master.key; encryption is on when the file exists.
	KeyFile string `toml:"key_file"`
}

// Metrics configures the Prometheus endpoint at /metrics.
type Metrics struct {
	// Token protects /metrics on the S3 listener (bearer or ?token=). Empty
	// disables it there; the admin unix socket always serves /metrics.
	Token string `toml:"token"`
}

// TLS configures how the S3 listener terminates TLS.
type TLS struct {
	Mode      string   `toml:"mode"` // off | file | acme
	CertFile  string   `toml:"cert_file"`
	KeyFile   string   `toml:"key_file"`
	ACMEEmail string   `toml:"acme_email"`
	ACMECA    string   `toml:"acme_ca"` // directory URL, empty = Let's Encrypt
	Domains   []string `toml:"domains"`
	DNS       DNS01    `toml:"dns"`
}

// DNS01 configures the ACME DNS-01 challenge provider.
type DNS01 struct {
	Provider string            `toml:"provider"`
	Settings map[string]string `toml:"settings"`
}

// Duration is a time.Duration that encodes as a string such as "30s" in TOML.
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Limits holds request limits that apply live.
type Limits struct {
	MaxObjectSize   int64    `toml:"max_object_size"`
	MaxPartSize     int64    `toml:"max_part_size"`
	AuthFailBurst   int      `toml:"auth_fail_burst"`
	AuthFailWindow  Duration `toml:"auth_fail_window"`
	RequestTimeout  Duration `toml:"request_timeout"`
	IdleTimeout     Duration `toml:"idle_timeout"`
	MaxHeaderBytes  int      `toml:"max_header_bytes"`
	ClockSkewWindow Duration `toml:"clock_skew_window"`
}

// Admin configures the local admin socket used by the CLI.
type Admin struct {
	Socket string `toml:"socket"`
}

// Default returns a Config populated with defaults relative to dataDir.
func Default(dataDir string) Config {
	return Config{
		Discovery: Discovery{Enabled: true},
		DataDir:   dataDir,
		Listen:    []string{"0.0.0.0:9000"},
		LogLevel:  "info",
		TLS:       TLS{Mode: "off"},
		Limits: Limits{
			MaxObjectSize:   5 * 1024 * 1024 * 1024 * 1024, // 5 TiB, S3 limit
			MaxPartSize:     5 * 1024 * 1024 * 1024,        // 5 GiB
			AuthFailBurst:   20,
			AuthFailWindow:  Duration(time.Minute),
			RequestTimeout:  0,
			IdleTimeout:     Duration(2 * time.Minute),
			MaxHeaderBytes:  64 * 1024,
			ClockSkewWindow: Duration(15 * time.Minute),
		},
		Admin:   Admin{Socket: filepath.Join(dataDir, "admin.sock")},
		Storage: Storage{ChunkSize: 4 << 20},
	}
}

// Load reads the TOML file at path (if non-empty), applies environment
// overrides and validates the result.
func Load(path string) (Config, error) {
	cfg := Default("/var/lib/hogshead")
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		cfg, err = Parse(data)
		if err != nil {
			return Config{}, err
		}
	}
	cfg, err := ApplyEnv(cfg, os.Environ())
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Parse decodes TOML on top of defaults. It does not apply env or validate.
func Parse(data []byte) (Config, error) {
	var raw struct {
		DataDir string `toml:"data_dir"`
	}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	dataDir := raw.DataDir
	if dataDir == "" {
		dataDir = "/var/lib/hogshead"
	}
	cfg := Default(dataDir)
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var sme *toml.StrictMissingError
		if errors.As(err, &sme) {
			return Config{}, fmt.Errorf("parse config: unknown fields:\n%s", sme.String())
		}
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Admin.Socket == Default("/var/lib/hogshead").Admin.Socket && cfg.DataDir != "/var/lib/hogshead" {
		cfg.Admin.Socket = filepath.Join(cfg.DataDir, "admin.sock")
	}
	return cfg, nil
}

// ApplyEnv returns a copy of cfg with HOGSHEAD_* overrides applied.
// Keys are upper-case with sections joined by underscores, for example
// HOGSHEAD_DATA_DIR, HOGSHEAD_LISTEN, HOGSHEAD_TLS_MODE, HOGSHEAD_LOG_LEVEL.
// Variables the command line reads itself (see reservedEnv) are skipped.
func ApplyEnv(cfg Config, environ []string) (Config, error) {
	out := cfg
	for _, kv := range environ {
		if !strings.HasPrefix(kv, EnvPrefix) {
			continue
		}
		k, v, _ := strings.Cut(strings.TrimPrefix(kv, EnvPrefix), "=")
		if reservedEnv(k) {
			continue
		}
		if err := out.set(k, v); err != nil {
			return Config{}, fmt.Errorf("env %s%s: %w", EnvPrefix, k, err)
		}
	}
	return out, nil
}

// reservedEnv reports whether a HOGSHEAD_ variable belongs to the command
// line rather than the configuration: where the config file is, which server
// the admin commands talk to, and the first-run values of `serve --init`.
func reservedEnv(key string) bool {
	switch key {
	case "CONFIG", "SERVER":
		return true
	}
	return strings.HasPrefix(key, "INIT_") || strings.HasPrefix(key, "TEST_")
}

func (c *Config) set(key, v string) error {
	switch key {
	case "DATA_DIR":
		c.DataDir = v
	case "LISTEN":
		c.Listen = splitList(v)
	case "HOSTNAME":
		c.Hostname = v
	case "NAME":
		c.Name = v
	case "CELLAR_ADVERTISE":
		c.Cellar.Advertise = v
	case "DISCOVERY_ENABLED":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return err
		}
		c.Discovery.Enabled = b
	case "LOG_LEVEL":
		c.LogLevel = v
	case "ACCESS_LOG":
		c.AccessLog = v
	case "LOG_JSON":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return err
		}
		c.LogJSON = b
	case "TLS_MODE":
		c.TLS.Mode = v
	case "TLS_CERT_FILE":
		c.TLS.CertFile = v
	case "TLS_KEY_FILE":
		c.TLS.KeyFile = v
	case "TLS_ACME_EMAIL":
		c.TLS.ACMEEmail = v
	case "TLS_ACME_CA":
		c.TLS.ACMECA = v
	case "TLS_DOMAINS":
		c.TLS.Domains = splitList(v)
	case "TLS_DNS_PROVIDER":
		c.TLS.DNS.Provider = v
	case "ADMIN_SOCKET":
		c.Admin.Socket = v
	case "STORAGE_COMPRESS":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return err
		}
		c.Storage.Compress = b
	case "STORAGE_KEY_FILE":
		c.Storage.KeyFile = v
	case "METRICS_TOKEN":
		c.Metrics.Token = v
	default:
		if strings.HasPrefix(key, "TLS_DNS_") {
			if c.TLS.DNS.Settings == nil {
				c.TLS.DNS.Settings = map[string]string{}
			}
			c.TLS.DNS.Settings[strings.ToLower(strings.TrimPrefix(key, "TLS_DNS_"))] = v
			return nil
		}
		return errors.New("unknown setting")
	}
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks the configuration for errors that would prevent startup.
func (c Config) Validate() error {
	var errs []error
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir is required"))
	}
	if len(c.Listen) == 0 {
		errs = append(errs, errors.New("listen must have at least one address"))
	}
	for _, l := range c.Listen {
		if _, _, err := net.SplitHostPort(l); err != nil {
			errs = append(errs, fmt.Errorf("listen %q: %w", l, err))
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level %q: must be debug, info, warn or error", c.LogLevel))
	}
	switch c.TLS.Mode {
	case "off":
	case "file":
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			errs = append(errs, errors.New("tls.mode=file requires cert_file and key_file"))
		}
	case "acme":
		if len(c.TLS.Domains) == 0 && c.Hostname == "" {
			errs = append(errs, errors.New("tls.mode=acme requires tls.domains or hostname"))
		}
	default:
		errs = append(errs, fmt.Errorf("tls.mode %q: must be off, file or acme", c.TLS.Mode))
	}
	if c.Storage.ChunkSize != 0 && (c.Storage.ChunkSize < 64*1024 || c.Storage.ChunkSize > 64<<20) {
		errs = append(errs, errors.New("storage.chunk_size must be between 64 KiB and 64 MiB"))
	}
	return errors.Join(errs...)
}

// MasterKeyPath returns the encryption key file path.
func (c Config) MasterKeyPath() string {
	if c.Storage.KeyFile != "" {
		return c.Storage.KeyFile
	}
	return filepath.Join(c.DataDir, "master.key")
}

// ACMEDomains returns the domains to request certificates for.
func (c Config) ACMEDomains() []string {
	if len(c.TLS.Domains) > 0 {
		return c.TLS.Domains
	}
	if c.Hostname != "" {
		return []string{c.Hostname, "*." + c.Hostname}
	}
	return nil
}

// RestartRequired reports which settings differ between old and new in ways
// that cannot be applied live.
func RestartRequired(old, cur Config) []string {
	var out []string
	if old.DataDir != cur.DataDir {
		out = append(out, "data_dir")
	}
	if strings.Join(old.Listen, ",") != strings.Join(cur.Listen, ",") {
		out = append(out, "listen")
	}
	if old.Admin.Socket != cur.Admin.Socket {
		out = append(out, "admin.socket")
	}
	if old.Storage.KeyFile != cur.Storage.KeyFile {
		out = append(out, "storage.key_file")
	}
	if old.TLS.Mode != cur.TLS.Mode {
		out = append(out, "tls.mode")
	}
	return out
}

// Marshal renders a config as TOML.
func Marshal(c Config) ([]byte, error) {
	return toml.Marshal(c)
}

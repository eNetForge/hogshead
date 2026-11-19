package cmds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/ids"
	"github.com/enetforge/hogshead/internal/meta"
	"github.com/enetforge/hogshead/internal/server"
)

// Environment variables read by `serve --init` on the first start only.
const (
	envInitAccessKey = "HOGSHEAD_INIT_ACCESS_KEY"
	envInitSecretKey = "HOGSHEAD_INIT_SECRET_KEY" //nolint:gosec // G101: a variable name, not a credential
	envInitPassword  = "HOGSHEAD_INIT_PASSWORD"   //nolint:gosec // G101: a variable name, not a credential
)

const (
	minSecretKeyLen = 16
	minPasswordLen  = 8
)

var accessKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]{3,128}$`)

// setupOptions describes a first-time setup: where things live and,
// optionally, credentials chosen by the operator instead of generated ones.
type setupOptions struct {
	DataDir, ConfigOut, Listen, Hostname, Name, Admin string
	Encrypt, Compress                                 bool
	AccessKey, SecretKey, Password                    string
}

// setupResult is what a first-time setup created.
type setupResult struct {
	Cfg                                   config.Config
	Admin, AccessKey, SecretKey, Password string
}

// validate rejects preset credentials that are too weak or malformed.
func (o setupOptions) validate() error {
	if (o.AccessKey == "") != (o.SecretKey == "") {
		return fmt.Errorf("%s and %s must be set together", envInitAccessKey, envInitSecretKey)
	}
	if o.AccessKey != "" && !accessKeyPattern.MatchString(o.AccessKey) {
		return fmt.Errorf("%s must be 3 to 128 letters and digits", envInitAccessKey)
	}
	if o.SecretKey != "" && len(o.SecretKey) < minSecretKeyLen {
		return fmt.Errorf("%s must be at least %d characters", envInitSecretKey, minSecretKeyLen)
	}
	if o.Password != "" && len(o.Password) < minPasswordLen {
		return fmt.Errorf("%s must be at least %d characters", envInitPassword, minPasswordLen)
	}
	return nil
}

// setup creates the data directory, the admin user with an access key and a
// web UI password, and writes the config file. It refuses to run when the
// config file already exists.
func setup(ctx context.Context, o setupOptions) (setupResult, error) {
	if _, err := os.Stat(o.ConfigOut); err == nil {
		return setupResult{}, fmt.Errorf("%s already exists; remove it or pass --config", o.ConfigOut)
	}
	if err := o.validate(); err != nil {
		return setupResult{}, err
	}
	cfg := config.Default(o.DataDir)
	cfg.Listen = []string{o.Listen}
	cfg.Hostname = o.Hostname
	cfg.Name = o.Name
	cfg.Storage.Compress = o.Compress
	cfg, err := config.ApplyEnv(cfg, os.Environ())
	if err != nil {
		return setupResult{}, err
	}
	if err := cfg.Validate(); err != nil {
		return setupResult{}, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return setupResult{}, fmt.Errorf("create data dir: %w", err)
	}
	if o.Encrypt {
		if err := server.WriteMasterKey(cfg.MasterKeyPath()); err != nil {
			return setupResult{}, fmt.Errorf("write master key: %w", err)
		}
	}
	res := setupResult{Cfg: cfg, Admin: o.Admin, AccessKey: o.AccessKey, SecretKey: o.SecretKey, Password: o.Password}
	if res.AccessKey == "" {
		res.AccessKey, res.SecretKey = ids.AccessKeyID(), ids.SecretKey()
	}
	if res.Password == "" {
		res.Password = ids.Password()
	}
	core, err := server.Open(ctx, cfg, newLogger(cfg))
	if err != nil {
		return setupResult{}, err
	}
	if err := createAdmin(ctx, core, res); err != nil {
		_ = core.Close()
		return setupResult{}, err
	}
	if err := core.Close(); err != nil {
		return setupResult{}, err
	}
	data, err := config.Marshal(cfg)
	if err != nil {
		return setupResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(o.ConfigOut), 0o750); err != nil {
		return setupResult{}, fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(o.ConfigOut, data, 0o600); err != nil {
		return setupResult{}, fmt.Errorf("write config: %w", err)
	}
	return res, nil
}

// createAdmin creates (or reuses) the admin user and gives it the access key
// and web UI password from res.
func createAdmin(ctx context.Context, core *server.Core, res setupResult) error {
	now := time.Now().UnixMilli()
	uid := ids.UserID()
	if _, err := core.Propose(ctx, &meta.CreateUser{ID: uid, Name: res.Admin, Role: meta.RoleAdmin, Now: now}); err != nil {
		var me *meta.Error
		if !errors.As(err, &me) || me.Code != meta.CodeUserExists {
			return err
		}
		u, gerr := core.DB.GetUserByName(ctx, res.Admin)
		if gerr != nil {
			return gerr
		}
		uid = u.ID
	}
	if _, err := core.Propose(ctx, &meta.CreateAccessKey{AccessKeyID: res.AccessKey, Secret: res.SecretKey, UserID: uid, Label: "quickstart", Now: now}); err != nil {
		return err
	}
	hash, err := admin.HashPassword(res.Password)
	if err != nil {
		return err
	}
	if _, err := core.Propose(ctx, &meta.SetUserPassword{ID: uid, Hash: hash}); err != nil {
		return err
	}
	_, err = core.Propose(ctx, &meta.Audit{Actor: "quickstart", Action: "key.create", Target: res.Admin, Now: now})
	return err
}

// initIfNeeded performs the first-time setup for `serve --init` when the
// config file does not exist yet, and prints the credentials once. It returns
// the config path to serve with.
func initIfNeeded(ctx context.Context, cfgPath string) (string, error) {
	if cfgPath == "" {
		cfgPath = defaultConfigOut()
	}
	if _, err := os.Stat(cfgPath); err == nil {
		return cfgPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("check config: %w", err)
	}
	dataDir := os.Getenv(config.EnvPrefix + "DATA_DIR")
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	preset := os.Getenv(envInitAccessKey) != ""
	res, err := setup(ctx, setupOptions{
		DataDir: dataDir, ConfigOut: cfgPath, Listen: "0.0.0.0:9000", Admin: "admin",
		AccessKey: os.Getenv(envInitAccessKey), SecretKey: os.Getenv(envInitSecretKey), Password: os.Getenv(envInitPassword),
	})
	if err != nil {
		return "", fmt.Errorf("first-time setup: %w", err)
	}
	secret, password := res.SecretKey, res.Password
	if preset {
		secret = "(from " + envInitSecretKey + ")"
	}
	if os.Getenv(envInitPassword) != "" {
		password = "(from " + envInitPassword + ")"
	}
	fmt.Printf(`First start: hogshead created %s.

Admin credentials (shown once, store them now):

  AWS_ACCESS_KEY_ID=%s
  AWS_SECRET_ACCESS_KEY=%s

Web UI login (/_ui/):

  user:     %s
  password: %s

`, cfgPath, res.AccessKey, secret, res.Admin, password)
	return cfgPath, nil
}

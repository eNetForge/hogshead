package cmds

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enetforge/hogshead/internal/cli"
)

func quickstartCmd() *cli.Command {
	var (
		dataDir, cfgOut, listen, hostname, adminName, name string
		systemd, encrypt, compress                         bool
	)
	return &cli.Command{
		Name:    "quickstart",
		Summary: "Create a data dir, config file and admin key, then print how to start",
		Usage:   "[flags]",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&dataDir, "data-dir", defaultDataDir(), "data directory")
			fs.StringVar(&cfgOut, "config", defaultConfigOut(), "config file to write")
			fs.StringVar(&listen, "listen", "0.0.0.0:9000", "S3 listen address")
			fs.StringVar(&hostname, "hostname", "", "hostname for virtual-host style buckets (optional)")
			fs.StringVar(&name, "name", "", "a human-friendly name for this server (default: the hostname)")
			fs.StringVar(&adminName, "admin", "admin", "admin user name")
			fs.BoolVar(&systemd, "systemd", false, "also write a systemd unit to stdout")
			fs.BoolVar(&encrypt, "encrypt", false, "generate a master key and encrypt chunks at rest")
			fs.BoolVar(&compress, "compress", false, "compress chunks with zstd")
		},
		Run: func(ctx context.Context, args []string) error {
			res, err := setup(ctx, setupOptions{
				DataDir: dataDir, ConfigOut: cfgOut, Listen: listen, Hostname: hostname, Name: name,
				Admin: adminName, Encrypt: encrypt, Compress: compress,
			})
			if err != nil {
				return err
			}
			addr := displayAddr(res.Cfg.Listen[0])
			fmt.Printf(`hogshead is ready.

  config:      %s
  data dir:    %s
  endpoint:    http://%s

Admin credentials (shown once, store them now):

  AWS_ACCESS_KEY_ID=%s
  AWS_SECRET_ACCESS_KEY=%s

Web UI login (http://%s/_ui/):

  user:     %s
  password: %s

Start the server:

  hogshead serve --config %s
%s
Try it:

  aws --endpoint-url http://%s s3 mb s3://first
`, cfgOut, res.Cfg.DataDir, addr, res.AccessKey, res.SecretKey, addr, res.Admin, res.Password, cfgOut, encryptNote(encrypt, res.Cfg.MasterKeyPath()), addr)
			if systemd {
				fmt.Print(systemdUnit(cfgOut))
			}
			return nil
		},
	}
}

func defaultDataDir() string {
	if os.Geteuid() == 0 {
		return "/var/lib/hogshead"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "hogshead")
}

func defaultConfigOut() string {
	if os.Geteuid() == 0 {
		return "/etc/hogshead/config.toml"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hogshead", "config.toml")
}

func displayAddr(listen string) string {
	if len(listen) > 0 && (listen[0] == ':' || len(listen) >= 8 && listen[:8] == "0.0.0.0:") {
		host, _ := os.Hostname()
		if host == "" {
			host = "localhost"
		}
		return host + listen[len(listen)-len(portOf(listen)):]
	}
	return listen
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ""
}

func systemdUnit(cfgPath string) string {
	return fmt.Sprintf(`
# /etc/systemd/system/hogshead.service
[Unit]
Description=hogshead object store
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/hogshead serve --config %s
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
User=hogshead
Group=hogshead
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/hogshead

[Install]
WantedBy=multi-user.target
`, cfgPath)
}

func encryptNote(on bool, path string) string {
	if !on {
		return ""
	}
	return fmt.Sprintf("\nEncryption at rest is ON. Back up the master key or the data is unrecoverable:\n\n  %s\n", path)
}

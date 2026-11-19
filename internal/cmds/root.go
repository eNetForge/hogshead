// Package cmds defines the hogshead command tree. Management commands are
// clients of the admin API; only serve and quickstart touch local state.
package cmds

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/enetforge/hogshead/internal/cli"
	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/version"
)

// Root returns the command tree.
func Root() *cli.Command {
	return &cli.Command{
		Name:    "hogshead",
		Summary: "A small S3-compatible object store.",
		Subcommands: []*cli.Command{
			serveCmd(),
			quickstartCmd(),
			statusCmd(),
			userCmd(),
			keyCmd(),
			bucketCmd(),
			globalReplicateCmd(),
			peersCmd(),
			cellarCmd(),
			usageCmd(),
			errorsCmd(),
			transferCmd(),
			auditCmd(),
			settingCmd(),
			configCmd(),
			apiCmd(),
			{Name: "version", Summary: "Print the version", Run: func(context.Context, []string) error {
				fmt.Println(version.Version)
				return nil
			}},
		},
	}
}

func defaultConfigPath() string {
	if p := os.Getenv("HOGSHEAD_CONFIG"); p != "" {
		return p
	}
	for _, p := range []string{"/etc/hogshead/config.toml", "hogshead.toml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultConfigPath(), "path to config file (env HOGSHEAD_CONFIG)")
}

func newLogger(cfg config.Config) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: lvl}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

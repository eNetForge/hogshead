package cmds

import (
	"context"
	"flag"
	"fmt"
	"net/http"

	"github.com/enetforge/hogshead/internal/cli"
	"github.com/enetforge/hogshead/internal/config"
	"github.com/enetforge/hogshead/internal/server"
)

// HandlerFactory builds the HTTP handlers for a core given the config path.
// cmd/hogshead installs the real one via SetHandlerFactory so this package
// does not import every layer.
type HandlerFactory func(configPath string) func(core *server.Core) (s3 http.Handler, admin http.Handler, err error)

var handlerFactory HandlerFactory = func(string) func(*server.Core) (http.Handler, http.Handler, error) {
	return func(core *server.Core) (http.Handler, http.Handler, error) {
		placeholder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not wired", http.StatusNotImplemented)
		})
		return placeholder, placeholder, nil
	}
}

// SetHandlerFactory installs the function that builds the HTTP handlers.
func SetHandlerFactory(f HandlerFactory) { handlerFactory = f }

func serveCmd() *cli.Command {
	var (
		cfgPath *string
		doInit  *bool
	)
	return &cli.Command{
		Name:    "serve",
		Summary: "Run the server",
		Usage:   "[--config path] [--init]",
		Flags: func(fs *flag.FlagSet) {
			cfgPath = configFlag(fs)
			doInit = fs.Bool("init", false, "on the first start, create the config file, data dir and admin credentials (as quickstart does), then serve")
		},
		Run: func(ctx context.Context, args []string) error {
			if *doInit {
				p, err := initIfNeeded(ctx, *cfgPath)
				if err != nil {
					return err
				}
				*cfgPath = p
			}
			cfg, err := config.Load(*cfgPath)
			if err != nil {
				return err
			}
			log := newLogger(cfg)
			log.Info("starting hogshead", "data_dir", cfg.DataDir, "config", *cfgPath)
			core, err := server.Open(ctx, cfg, log)
			if err != nil {
				return err
			}
			defer func() { _ = core.Close() }()
			tlsConf, err := server.TLSProvider(ctx, core)
			if err != nil {
				return err
			}
			s3h, adminh, err := handlerFactory(*cfgPath)(core)
			if err != nil {
				return err
			}
			if *cfgPath != "" {
				go server.WatchConfig(ctx, *cfgPath, core)
			}
			ln := server.NewListeners(core, s3h, adminh, tlsConf)
			if err := ln.Serve(ctx); err != nil {
				return fmt.Errorf("serve: %w", err)
			}
			log.Info("shutdown complete")
			return nil
		},
	}
}

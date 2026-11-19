// Command hogshead is a small S3-compatible object store.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/enetforge/hogshead/internal/cli"
	"github.com/enetforge/hogshead/internal/cmds"
	"github.com/enetforge/hogshead/internal/wire"
)

func main() {
	cmds.SetHandlerFactory(wire.Handlers)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := cli.Execute(ctx, cmds.Root(), os.Args[1:], os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, cli.ErrUsage):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

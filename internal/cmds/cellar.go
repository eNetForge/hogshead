package cmds

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/cellar"
	"github.com/enetforge/hogshead/internal/cli"
)

// cellarCmd manages cellar membership.
func cellarCmd() *cli.Command {
	var cf *connFlags
	var name, ack, advertise string
	var wait, merge bool
	conn := func(fs *flag.FlagSet) { cf = addConnFlags(fs) }
	return &cli.Command{Name: "cellar", Summary: "Group servers into a cellar that holds a full copy of each other's data, users and settings", Subcommands: []*cli.Command{
		{Name: "status", Summary: "Show the cellar, its members and pending join requests", Usage: "[flags]", Flags: conn,
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				st, err := c.Cellar(ctx)
				if err != nil {
					return err
				}
				return cf.out(st, func(w io.Writer) { printCellar(w, st) })
			}},
		{Name: "init", Summary: "Create a new cellar with this server as its first member", Usage: "--name n [--write-ack local|one|all]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&name, "name", "", "cellar name")
				fs.StringVar(&ack, "write-ack", "", "writes are acknowledged after: local commit, one other member (default) or all members")
			},
			Run: func(ctx context.Context, args []string) error {
				if name == "" {
					return errors.New("usage: cellar init --name <name>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				st, err := c.CellarInit(ctx, admin.CellarInitRequest{Name: name, WriteAck: ack})
				if err != nil {
					return err
				}
				return cf.out(st, func(w io.Writer) { printCellar(w, st) })
			}},
		{Name: "join", Summary: "Ask a cellar member to admit this server (an admin there approves)", Usage: "<member-address> [--advertise URL] [--wait]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&advertise, "advertise", "", "URL members use to reach this server (default: derived)")
				fs.BoolVar(&wait, "wait", false, "wait until the request is approved and the join completes")
				fs.BoolVar(&merge, "merge", false, "allow joining with existing buckets and add them to the cellar (the cellar's configuration and users win clashes)")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: cellar join <host[:port]|url> [flags]")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				st, err := c.CellarJoin(ctx, admin.CellarJoinRequest{Address: args[0], Advertise: advertise, Merge: merge})
				if err != nil {
					return err
				}
				if !wait {
					fmt.Println("join requested; waiting for an administrator of the cellar to approve this server")
					return cf.out(st, func(w io.Writer) { printCellar(w, st) })
				}
				for !st.Active {
					if st.Joining != nil && st.Joining.LastError != "" {
						return fmt.Errorf("join failed: %s", st.Joining.LastError)
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(2 * time.Second):
					}
					if st, err = c.Cellar(ctx); err != nil {
						return err
					}
				}
				return cf.out(st, func(w io.Writer) { printCellar(w, st) })
			}},
		{Name: "cancel-join", Summary: "Withdraw an outstanding join request", Usage: "[flags]", Flags: conn,
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				if err := c.CellarCancelJoin(ctx); err != nil {
					return err
				}
				fmt.Println("join request withdrawn")
				return nil
			}},
		{Name: "approve", Summary: "Approve a pending join request", Usage: "<server-id>", Flags: conn,
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: cellar approve <server-id>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				st, err := c.CellarApprove(ctx, args[0])
				if err != nil {
					return err
				}
				return cf.out(st, func(w io.Writer) { printCellar(w, st) })
			}},
		{Name: "reject", Summary: "Reject a pending join request", Usage: "<server-id>", Flags: conn,
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: cellar reject <server-id>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				if err := c.CellarReject(ctx, args[0]); err != nil {
					return err
				}
				fmt.Println("rejected", args[0])
				return nil
			}},
		{Name: "remove", Summary: "Remove a member and rotate the cellar key", Usage: "<server-id>", Flags: conn,
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: cellar remove <server-id>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				if err := c.CellarRemove(ctx, args[0]); err != nil {
					return err
				}
				fmt.Println("removed", args[0])
				return nil
			}},
		{Name: "leave", Summary: "Leave the cellar (data already copied stays everywhere)", Usage: "[flags]", Flags: conn,
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				if err := c.CellarLeave(ctx); err != nil {
					return err
				}
				fmt.Println("left the cellar")
				return nil
			}},
		{Name: "set", Summary: "Change the cellar name or write acknowledgement level", Usage: "[--name n] [--write-ack local|one|all]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&name, "name", "", "new cellar name")
				fs.StringVar(&ack, "write-ack", "", "local, one or all")
			},
			Run: func(ctx context.Context, args []string) error {
				if name == "" && ack == "" {
					return errors.New("pass --name and/or --write-ack")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				st, err := c.CellarUpdate(ctx, admin.CellarUpdateRequest{Name: name, WriteAck: ack})
				if err != nil {
					return err
				}
				return cf.out(st, func(w io.Writer) { printCellar(w, st) })
			}},
		{Name: "forget-approval", Summary: "Forget a consumed join approval record", Usage: "<server-id>", Flags: conn,
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: cellar forget-approval <server-id>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				return c.CellarForgetApproval(ctx, args[0])
			}},
	}}
}

func printCellar(w io.Writer, st *cellar.Status) {
	if !st.Active {
		_, _ = fmt.Fprintln(w, "not in a cellar")
		if st.Joining != nil {
			_, _ = fmt.Fprintf(w, "join requested at %s via %s; waiting for approval", st.Joining.RequestedAt.Local().Format("15:04:05"), st.Joining.Address)
			if st.Joining.LastError != "" {
				_, _ = fmt.Fprintf(w, " (last error: %s)", st.Joining.LastError)
			}
			_, _ = fmt.Fprintln(w)
		}
		return
	}
	row(w, "cellar", st.Name)
	row(w, "id", st.ID)
	row(w, "write ack", st.WriteAck)
	ready := "yes"
	if !st.Ready {
		ready = fmt.Sprintf("catching up (%d changes pending)", st.Backlog)
	}
	row(w, "ready", ready)
	if st.CheckError != "" {
		row(w, "check error", st.CheckError)
	}
	_, _ = fmt.Fprintln(w)
	row(w, "MEMBER", "ID", "ENDPOINT", "VERSION", "LAG", "STATE")
	for _, m := range st.Members {
		state := "in sync"
		switch {
		case m.Self:
			state = "this server"
		case m.LastError != "":
			state = "error: " + m.LastError
		case m.Backfilling:
			state = "backfilling"
		case !m.Enabled:
			state = "paused"
		case m.Lag > 0:
			state = "syncing"
		}
		row(w, m.Name, m.ID, m.Endpoint, m.Version, m.Lag, state)
	}
	if len(st.Pending) > 0 {
		_, _ = fmt.Fprintln(w)
		row(w, "PENDING", "ID", "ENDPOINT", "VERSION", "BRINGS", "REQUESTED")
		for _, p := range st.Pending {
			brings := "empty"
			if p.Buckets > 0 {
				brings = fmt.Sprintf("%d bucket(s) (merge)", p.Buckets)
			}
			row(w, p.Name, p.ID, p.Endpoint, p.Version, brings, p.RequestedAt.Local().Format("2006-01-02 15:04:05"))
		}
		_, _ = fmt.Fprintln(w, "approve with: hogshead cellar approve <id>")
	}
}

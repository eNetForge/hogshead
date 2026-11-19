package cmds

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/cli"
	"github.com/enetforge/hogshead/internal/transfer"
)

// transferCmd copies objects between a local bucket and another S3 service.
func transferCmd() *cli.Command {
	return &cli.Command{Name: "transfer", Summary: "Copy objects to or from another S3 service (migrate, clone, seed a standby)", Subcommands: []*cli.Command{
		transferStartCmd("export", "Copy a local bucket to a bucket on another S3 service", "--to"),
		transferStartCmd("import", "Copy a bucket from another S3 service into a local bucket", "--from"),
		{Name: "list", Summary: "List jobs with progress", Usage: "[flags]", Flags: func(fs *flag.FlagSet) { transferConn = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				c, err := transferConn.client()
				if err != nil {
					return err
				}
				jobs, err := c.Transfers(ctx)
				if err != nil {
					return err
				}
				return transferConn.out(jobs, func(w io.Writer) { printTransfers(w, jobs) })
			}},
		{Name: "show", Summary: "Show one job with its first errors", Usage: "<id>", Flags: func(fs *flag.FlagSet) { transferConn = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: transfer show <id>")
				}
				c, err := transferConn.client()
				if err != nil {
					return err
				}
				j, err := c.GetTransfer(ctx, args[0])
				if err != nil {
					return err
				}
				return transferConn.out(j, func(w io.Writer) { printTransfer(w, j) })
			}},
		{Name: "cancel", Summary: "Cancel a running job, or remove a finished job's record", Usage: "<id>", Flags: func(fs *flag.FlagSet) { transferConn = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: transfer cancel <id>")
				}
				c, err := transferConn.client()
				if err != nil {
					return err
				}
				if err := c.CancelTransfer(ctx, args[0]); err != nil {
					return err
				}
				fmt.Println("ok", args[0])
				return nil
			}},
	}}
}

var transferConn *connFlags

func transferStartCmd(direction, summary, endpointFlag string) *cli.Command {
	var cf *connFlags
	var endpoint, remoteBucket, remotePrefix, prefix, ak, sk, region, overwrite string
	var pathStyle, virtualHost, dryRun, wait bool
	var concurrency int
	flagName := endpointFlag[2:]
	return &cli.Command{Name: direction, Summary: summary,
		Usage: "<bucket> " + endpointFlag + " https://host:9000 --bucket remote --dest-key AK --dest-secret SK [--prefix p] [--remote-prefix p] [--overwrite if-changed|always|never] [--dry-run] [--concurrency n] [--wait]",
		Flags: func(fs *flag.FlagSet) {
			cf = addConnFlags(fs)
			fs.StringVar(&endpoint, flagName, "", "remote endpoint URL")
			fs.StringVar(&remoteBucket, "bucket", "", "remote bucket (created on export when missing)")
			fs.StringVar(&remotePrefix, "remote-prefix", "", "key prefix on the remote side")
			fs.StringVar(&prefix, "prefix", "", "key prefix on the local side; keys are mapped by swapping the prefixes")
			fs.StringVar(&ak, "dest-key", "", "remote access key id")
			fs.StringVar(&sk, "dest-secret", "", "remote secret key")
			fs.StringVar(&region, "region", "", "remote region (default us-east-1)")
			fs.StringVar(&overwrite, "overwrite", "", "if-changed (default), always or never")
			fs.BoolVar(&pathStyle, "path-style", true, "use path-style addressing at the remote")
			fs.BoolVar(&virtualHost, "virtual-host", false, "use virtual-host style addressing at the remote (AWS)")
			fs.BoolVar(&dryRun, "dry-run", false, "count what would be copied without writing")
			fs.IntVar(&concurrency, "concurrency", 0, "parallel copies (default 4, max 16)")
			fs.BoolVar(&wait, "wait", false, "wait for the job to finish and print the result")
		},
		Run: func(ctx context.Context, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: transfer %s <bucket> [flags]", direction)
			}
			c, err := cf.client()
			if err != nil {
				return err
			}
			ps := pathStyle && !virtualHost
			req := admin.TransferRequest{Direction: direction, Bucket: args[0], Prefix: prefix, Endpoint: endpoint, RemoteBucket: remoteBucket,
				RemotePrefix: remotePrefix, AccessKey: ak, SecretKey: sk, Region: region, PathStyle: &ps, Overwrite: overwrite, DryRun: dryRun, Concurrency: concurrency}
			j, err := c.StartTransfer(ctx, req)
			if err != nil {
				return err
			}
			if !wait {
				return cf.out(j, func(w io.Writer) { printTransfer(w, j) })
			}
			for !terminal(j.State) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
				if j, err = c.GetTransfer(ctx, j.ID); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "\r%s: listed %d copied %d skipped %d failed %d", j.State, j.Listed, j.Copied, j.Skipped, j.Failed)
			}
			fmt.Fprintln(os.Stderr)
			if err := cf.out(j, func(w io.Writer) { printTransfer(w, j) }); err != nil {
				return err
			}
			if j.State != transfer.StateDone {
				return fmt.Errorf("transfer %s %s", j.ID, j.State)
			}
			return nil
		}}
}

func terminal(state string) bool {
	switch state {
	case transfer.StateDone, transfer.StateFailed, transfer.StateCancelled, transfer.StateInterrupted:
		return true
	}
	return false
}

func printTransfers(w io.Writer, jobs []transfer.Job) {
	row(w, "ID", "DIRECTION", "LOCAL", "REMOTE", "STATE", "LISTED", "COPIED", "SKIPPED", "FAILED", "BYTES", "STARTED")
	for _, j := range jobs {
		started := "-"
		if !j.StartedAt.IsZero() {
			started = j.StartedAt.Local().Format("2006-01-02 15:04:05")
		}
		row(w, j.ID, j.Spec.Direction, j.Spec.Bucket+"/"+j.Spec.Prefix, j.Spec.Remote.Endpoint+"/"+j.Spec.Remote.Bucket+"/"+j.Spec.RemotePrefix, j.State, j.Listed, j.Copied, j.Skipped, j.Failed, j.Bytes, started)
	}
}

func printTransfer(w io.Writer, j *transfer.Job) {
	printTransfers(w, []transfer.Job{*j})
	if j.Spec.DryRun {
		_, _ = fmt.Fprintln(w, "dry run: nothing was written")
	}
	if j.Error != "" {
		_, _ = fmt.Fprintln(w, "error:", j.Error)
	}
	for _, e := range j.Errors {
		_, _ = fmt.Fprintln(w, "  ", e)
	}
}

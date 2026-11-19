package cmds

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/enetforge/hogshead/internal/cli"
	"github.com/enetforge/hogshead/internal/meta"
)

// usageCmd shows S3 usage counters.
func usageCmd() *cli.Command {
	var cf *connFlags
	var since, until, by, bucket, user, key, scope string
	return &cli.Command{Name: "usage", Summary: "S3 usage: requests, errors and bytes by bucket, user, access key, class or hour",
		Usage: "[--since 24h|7d|RFC3339] [--until ...] [--by bucket|user|key|class|hour[,...]] [--bucket b] [--user u] [--key ak]",
		Flags: func(fs *flag.FlagSet) {
			cf = addConnFlags(fs)
			fs.StringVar(&since, "since", "24h", "start of the window: a duration before now (24h, 7d) or an RFC 3339 time")
			fs.StringVar(&until, "until", "", "end of the window (default now)")
			fs.StringVar(&by, "by", "bucket", "group by: bucket, user, key, class, hour, or a comma-separated combination; empty for one total")
			fs.StringVar(&bucket, "bucket", "", "only this bucket")
			fs.StringVar(&user, "user", "", "only this user")
			fs.StringVar(&key, "key", "", "only this access key")
			fs.StringVar(&scope, "scope", "local", "local: this server; cellar: every member summed (add member to --by for a breakdown)")
		},
		Run: func(ctx context.Context, args []string) error {
			c, err := cf.client()
			if err != nil {
				return err
			}
			params := url.Values{}
			if since != "" {
				params.Set("from", since)
			}
			if until != "" {
				params.Set("to", until)
			}
			if by != "" {
				params.Set("by", by)
			}
			for k, v := range map[string]string{"bucket": bucket, "user": user, "key": key, "scope": scope} {
				if v != "" {
					params.Set(k, v)
				}
			}
			res, err := c.Usage(ctx, params)
			if err != nil {
				return err
			}
			return cf.out(res, func(w io.Writer) {
				printUsage(w, res.Rows, res.Totals, res.By, res.AccessLog)
				if res.Scope == "cellar" {
					for _, m := range res.Members {
						if !m.OK {
							_, _ = fmt.Fprintf(w, "member %s not included: %s\n", m.Name, m.Error)
						}
					}
				}
			})
		}}
}

func printUsage(w io.Writer, rows []meta.UsageRow, totals meta.UsageRow, by []string, accessLog string) {
	head := []any{}
	for _, d := range by {
		switch d {
		case "hour":
			head = append(head, "HOUR (UTC)")
		case "key":
			head = append(head, "ACCESS KEY")
		case "member":
			head = append(head, "MEMBER")
		default:
			head = append(head, stringsUpper(d))
		}
	}
	row(w, append(head, "REQUESTS", "ERRORS", "BYTES IN", "BYTES OUT")...)
	for _, r := range rows {
		cells := []any{}
		for _, d := range by {
			switch d {
			case "hour":
				cells = append(cells, time.Unix(r.Hour, 0).UTC().Format("2006-01-02 15:00"))
			case "bucket":
				cells = append(cells, orDash(r.Bucket))
			case "user":
				cells = append(cells, orDash(r.User))
			case "key":
				cells = append(cells, orDash(r.AccessKey))
			case "class":
				cells = append(cells, r.Class)
			case "member":
				cells = append(cells, orDash(r.Member))
			}
		}
		row(w, append(cells, r.Requests, r.Errors, humanBytes(r.BytesIn), humanBytes(r.BytesOut))...)
	}
	_, _ = fmt.Fprintf(w, "total: %d requests, %d errors, %s in, %s out\n", totals.Requests, totals.Errors, humanBytes(totals.BytesIn), humanBytes(totals.BytesOut))
	if accessLog != "" {
		_, _ = fmt.Fprintln(w, "access log:", accessLog)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func stringsUpper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

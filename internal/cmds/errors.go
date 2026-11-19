package cmds

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"

	"github.com/enetforge/hogshead/internal/cli"
)

// errorsCmd explains recent failed S3 requests.
func errorsCmd() *cli.Command {
	var cf *connFlags
	var since, bucket, user, code, scope string
	var recent int
	return &cli.Command{Name: "errors", Summary: "Recent failed S3 requests, grouped by cause, with an explanation",
		Usage: "[--since 24h|7d] [--bucket b] [--user u] [--code NoSuchKey] [--scope local|cellar] [--recent n]",
		Flags: func(fs *flag.FlagSet) {
			cf = addConnFlags(fs)
			fs.StringVar(&since, "since", "24h", "window: a duration before now or an RFC 3339 time")
			fs.StringVar(&bucket, "bucket", "", "only this bucket")
			fs.StringVar(&user, "user", "", "only this user")
			fs.StringVar(&code, "code", "", "only this S3 error code")
			fs.StringVar(&scope, "scope", "local", "local: this server; cellar: every member")
			fs.IntVar(&recent, "recent", 0, "also list the latest n individual requests")
		},
		Run: func(ctx context.Context, args []string) error {
			c, err := cf.client()
			if err != nil {
				return err
			}
			params := url.Values{}
			for k, v := range map[string]string{"from": since, "bucket": bucket, "user": user, "code": code, "scope": scope} {
				if v != "" {
					params.Set(k, v)
				}
			}
			res, err := c.Errors(ctx, params)
			if err != nil {
				return err
			}
			return cf.out(res, func(w io.Writer) {
				if res.Total == 0 {
					_, _ = fmt.Fprintln(w, "no failed requests in this window")
					return
				}
				row(w, "COUNT", "SEVERITY", "CODE", "STATUS", "OPERATION", "BUCKET", "USER", "LAST SEEN")
				for _, g := range res.Groups {
					row(w, g.Count, g.Severity, g.Code, g.Status, g.Op, orDash(g.Bucket), orDash(g.User), g.LastSeen.Local().Format("2006-01-02 15:04:05"))
				}
				_, _ = fmt.Fprintln(w)
				for _, g := range res.Groups {
					_, _ = fmt.Fprintf(w, "%s on %s: %s\n", g.Code, g.Op, g.Hint)
				}
				if recent > 0 {
					_, _ = fmt.Fprintln(w)
					row(w, "TIME", "STATUS", "CODE", "OPERATION", "BUCKET/KEY", "USER", "IP", "MESSAGE")
					for i, r := range res.Rows {
						if i >= recent {
							break
						}
						row(w, r.Time.Local().Format("01-02 15:04:05"), r.Status, orDash(r.ErrorCode), r.Op, r.Bucket+"/"+r.Key, orDash(r.User), r.IP, r.ErrorMessage)
					}
				}
				for _, m := range res.Members {
					if !m.OK {
						_, _ = fmt.Fprintf(w, "member %s not included: %s\n", m.Name, m.Error)
					}
				}
			})
		}}
}

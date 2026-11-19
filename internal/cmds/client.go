package cmds

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/enetforge/hogshead/internal/adminclient"
	"github.com/enetforge/hogshead/internal/auth"
	"github.com/enetforge/hogshead/internal/config"
)

// connFlags are shared by every management command.
type connFlags struct {
	cfgPath *string
	server  *string
	ak, sk  *string
	socket  *string
	jsonOut *bool
}

func addConnFlags(fs *flag.FlagSet) *connFlags {
	c := &connFlags{}
	c.cfgPath = configFlag(fs)
	c.server = fs.String("server", os.Getenv("HOGSHEAD_SERVER"), "admin API URL, e.g. https://s3.example.com (env HOGSHEAD_SERVER)")
	c.ak = fs.String("access-key", os.Getenv("AWS_ACCESS_KEY_ID"), "access key for --server (env AWS_ACCESS_KEY_ID)")
	c.sk = fs.String("secret-key", os.Getenv("AWS_SECRET_ACCESS_KEY"), "secret key for --server (env AWS_SECRET_ACCESS_KEY)")
	c.socket = fs.String("socket", os.Getenv("HOGSHEAD_ADMIN_SOCKET"), "admin unix socket path (default from config)")
	c.jsonOut = fs.Bool("json", false, "print JSON instead of a table")
	return c
}

// client builds an admin client from flags: --server with keys, else the
// local socket from --socket or the config file.
func (c *connFlags) client() (*adminclient.Client, error) {
	if *c.server != "" {
		if *c.ak == "" || *c.sk == "" {
			return nil, errors.New("--server needs --access-key and --secret-key (or AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY)")
		}
		return adminclient.NewHTTP(*c.server, auth.Credentials{AccessKeyID: *c.ak, SecretKey: *c.sk}), nil
	}
	sock := *c.socket
	if sock == "" {
		cfg, err := config.Load(*c.cfgPath)
		if err != nil {
			return nil, fmt.Errorf("cannot determine admin socket: %w (pass --socket or --server)", err)
		}
		sock = cfg.Admin.Socket
	}
	if _, err := os.Stat(sock); err != nil {
		return nil, fmt.Errorf("admin socket %s not found; is the server running? (pass --server to use a remote node)", sock)
	}
	u := os.Getenv("USER")
	if u == "" {
		u = "local"
	}
	return adminclient.NewSocket(sock).WithUser(u), nil
}

func (c *connFlags) out(v any, table func(w io.Writer)) error {
	if *c.jsonOut || table == nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	table(w)
	return w.Flush()
}

func row(w io.Writer, cols ...any) {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprint(c)
	}
	_, _ = fmt.Fprintln(w, strings.Join(parts, "\t"))
}

func readArgOrFile(s string) (string, error) {
	if strings.HasPrefix(s, "@") {
		b, err := os.ReadFile(strings.TrimPrefix(s, "@"))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if s == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return s, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

package cmds

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/adminclient"
	"github.com/enetforge/hogshead/internal/cli"
	"github.com/enetforge/hogshead/internal/replication"
)

func statusCmd() *cli.Command {
	var cf *connFlags
	return &cli.Command{
		Name: "status", Summary: "Show server status", Usage: "[flags]",
		Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
		Run: func(ctx context.Context, args []string) error {
			c, err := cf.client()
			if err != nil {
				return err
			}
			st, err := c.Status(ctx)
			if err != nil {
				return err
			}
			return cf.out(st, func(w io.Writer) {
				row(w, "version", st.Version)
				row(w, "name", st.NodeName)
				if st.ID != "" {
					row(w, "id", st.ID)
				}
				row(w, "uptime", st.Uptime)
				row(w, "buckets", st.Stats.Buckets)
				row(w, "objects", fmt.Sprintf("%d (%s)", st.Stats.Objects, humanBytes(st.Stats.Bytes)))
				row(w, "uploads", st.Stats.Uploads)
				row(w, "listen", strings.Join(st.Listen, ", "), "tls="+st.TLSMode)
				if len(st.PendingRestart) > 0 {
					row(w, "restart needed", strings.Join(st.PendingRestart, ", "))
				}
				if st.Workers.LastError != "" {
					row(w, "worker error", st.Workers.LastError)
				}
			})
		},
	}
}

func userCmd() *cli.Command {
	var cf *connFlags
	var role, buckets, password, passwordHash string
	var disable, enable, clear bool
	return &cli.Command{Name: "user", Summary: "Manage users", Subcommands: []*cli.Command{
		{Name: "list", Summary: "List users", Usage: "[flags]", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				us, err := c.ListUsers(ctx)
				if err != nil {
					return err
				}
				return cf.out(us, func(w io.Writer) {
					row(w, "NAME", "ROLE", "BUCKETS", "KEYS", "DISABLED", "CREATED")
					for _, u := range us {
						scope := "*"
						if len(u.Buckets) > 0 {
							scope = strings.Join(u.Buckets, ",")
						}
						row(w, u.Name, u.Role, scope, u.Keys, u.Disabled, u.CreatedAt.Format("2006-01-02"))
					}
				})
			}},
		{Name: "add", Summary: "Create a user", Usage: "<name> [--role admin|read-write|read-only] [--buckets a,b]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&role, "role", "read-write", "role")
				fs.StringVar(&buckets, "buckets", "", "comma separated bucket scope (default all)")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: user add <name>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				u, err := c.CreateUser(ctx, admin.CreateUserRequest{Name: args[0], Role: role, Buckets: splitCSV(buckets)})
				if err != nil {
					return err
				}
				return cf.out(u, func(w io.Writer) { row(w, "created user", u.Name, "role", u.Role) })
			}},
		{Name: "show", Summary: "Show a user", Usage: "<name>", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: user show <name>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				u, err := c.GetUser(ctx, args[0])
				if err != nil {
					return err
				}
				return cf.out(u, func(w io.Writer) {
					row(w, "name", u.Name)
					row(w, "role", u.Role)
					row(w, "buckets", strings.Join(u.Buckets, ","))
					row(w, "keys", u.Keys)
					row(w, "disabled", u.Disabled)
				})
			}},
		{Name: "passwd", Summary: "Set or clear a user's web UI password", Usage: "<name> [--password p | --clear]   (reads the password from stdin when neither is given)",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&password, "password", "", "the new password (at least 8 characters)")
				fs.BoolVar(&clear, "clear", false, "remove password login for the user")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: user passwd <name> [--password p | --clear]")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				pw := password
				if !clear && pw == "" {
					pw, err = readPassword()
					if err != nil {
						return err
					}
				}
				if clear {
					pw = ""
				}
				u, err := c.SetUserPassword(ctx, args[0], pw)
				if err != nil {
					return err
				}
				if pw == "" {
					fmt.Println("removed password login for", u.Name)
				} else {
					fmt.Println("password set for", u.Name)
				}
				return nil
			}},
		{Name: "import", Summary: "Create or replace a user wholesale (used by config sync and restores)", Usage: "<name> --role r [--buckets a,b] [--disabled] [--password-hash h]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&role, "role", "", "role: admin, read-write or read-only")
				fs.StringVar(&buckets, "buckets", "", "bucket scope, comma separated (empty for all)")
				fs.BoolVar(&disable, "disabled", false, "create the user disabled")
				fs.StringVar(&passwordHash, "password-hash", "", "bcrypt hash of the web UI password")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 || role == "" {
					return errors.New("usage: user import <name> --role r [flags]")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				b := []string{}
				if buckets != "" && buckets != "*" {
					b = splitCSV(buckets)
				}
				u, err := c.ImportUser(ctx, args[0], admin.UserImportRequest{Role: role, Buckets: b, Disabled: disable, PasswordHash: passwordHash})
				if err != nil {
					return err
				}
				return cf.out(u, func(w io.Writer) { row(w, "NAME", "ROLE", "DISABLED"); row(w, u.Name, u.Role, u.Disabled) })
			}},
		{Name: "set", Summary: "Change a user's role, scope or enabled state", Usage: "<name> [--role r] [--buckets a,b|*] [--disable|--enable]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&role, "role", "", "new role")
				fs.StringVar(&buckets, "buckets", "", "new bucket scope, comma separated; * for all")
				fs.BoolVar(&disable, "disable", false, "disable the user")
				fs.BoolVar(&enable, "enable", false, "enable the user")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: user set <name> [flags]")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				req := admin.UpdateUserRequest{}
				if role != "" {
					req.Role = &role
				}
				if buckets != "" {
					b := splitCSV(buckets)
					if buckets == "*" {
						b = []string{}
					}
					req.Buckets = &b
				}
				if disable || enable {
					d := disable
					req.Disabled = &d
				}
				u, err := c.UpdateUser(ctx, args[0], req)
				if err != nil {
					return err
				}
				return cf.out(u, func(w io.Writer) { row(w, "updated", u.Name, u.Role, "disabled="+fmt.Sprint(u.Disabled)) })
			}},
		{Name: "rm", Summary: "Delete a user and its keys", Usage: "<name>", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: user rm <name>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				if err := c.DeleteUser(ctx, args[0]); err != nil {
					return err
				}
				fmt.Println("deleted user", args[0])
				return nil
			}},
	}}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func keyCmd() *cli.Command {
	var cf *connFlags
	var label, user string
	var secret, keyLabel string
	var keyDisabled bool
	return &cli.Command{Name: "key", Summary: "Manage access keys", Subcommands: []*cli.Command{
		{Name: "import", Summary: "Create or replace an access key with a known secret (used by config sync and restores)", Usage: "<user> <access-key-id> --secret s [--label l] [--disabled]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&secret, "secret", "", "the secret access key")
				fs.StringVar(&keyLabel, "label", "", "label")
				fs.BoolVar(&keyDisabled, "disabled", false, "import the key disabled")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 2 || secret == "" {
					return errors.New("usage: key import <user> <access-key-id> --secret s [flags]")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				k, err := c.ImportKey(ctx, args[0], args[1], admin.KeyImportRequest{Secret: secret, Label: keyLabel, Disabled: keyDisabled})
				if err != nil {
					return err
				}
				return cf.out(k, func(w io.Writer) {
					row(w, "ACCESS KEY", "USER", "LABEL", "DISABLED")
					row(w, k.AccessKeyID, k.User, k.Label, k.Disabled)
				})
			}},
		{Name: "list", Summary: "List access keys", Usage: "[--user name]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&user, "user", "", "only this user's keys")
			},
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				ks, err := c.ListKeys(ctx, user)
				if err != nil {
					return err
				}
				return cf.out(ks, func(w io.Writer) {
					row(w, "ACCESS KEY", "USER", "LABEL", "DISABLED", "CREATED")
					for _, k := range ks {
						row(w, k.AccessKeyID, k.User, k.Label, k.Disabled, k.CreatedAt.Format("2006-01-02"))
					}
				})
			}},
		{Name: "create", Summary: "Create an access key for a user (secret shown once)", Usage: "<user> [--label text]",
			Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs); fs.StringVar(&label, "label", "", "label") },
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: key create <user>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				k, err := c.CreateKey(ctx, args[0], label)
				if err != nil {
					return err
				}
				return cf.out(k, func(w io.Writer) {
					row(w, "AWS_ACCESS_KEY_ID", k.AccessKeyID)
					row(w, "AWS_SECRET_ACCESS_KEY", k.SecretAccessKey)
				})
			}},
		{Name: "disable", Summary: "Disable an access key", Usage: "<access-key-id>", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error { return setKey(ctx, cf, args, true) }},
		{Name: "enable", Summary: "Enable an access key", Usage: "<access-key-id>", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error { return setKey(ctx, cf, args, false) }},
		{Name: "revoke", Summary: "Delete an access key", Usage: "<access-key-id>", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: key revoke <access-key-id>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				if err := c.DeleteKey(ctx, args[0]); err != nil {
					return err
				}
				fmt.Println("revoked", args[0])
				return nil
			}},
	}}
}

func setKey(ctx context.Context, cf *connFlags, args []string, disabled bool) error {
	if len(args) != 1 {
		return errors.New("usage: key enable|disable <access-key-id>")
	}
	c, err := cf.client()
	if err != nil {
		return err
	}
	k, err := c.UpdateKey(ctx, args[0], disabled)
	if err != nil {
		return err
	}
	return cf.out(k, func(w io.Writer) { row(w, k.AccessKeyID, "disabled="+fmt.Sprint(k.Disabled)) })
}

func bucketCmd() *cli.Command {
	var cf *connFlags
	var versioning, pol, cors, lc, owner string
	var lock, public, private bool
	var force bool
	return &cli.Command{Name: "bucket", Summary: "Manage buckets", Subcommands: []*cli.Command{
		{Name: "list", Summary: "List buckets", Usage: "[flags]", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				bs, err := c.ListBuckets(ctx)
				if err != nil {
					return err
				}
				return cf.out(bs, func(w io.Writer) {
					row(w, "NAME", "OWNER", "VERSIONING", "OBJECTS", "SIZE", "CREATED")
					for _, b := range bs {
						v := b.Versioning
						if v == "" {
							v = "off"
						}
						row(w, b.Name, b.OwnerName, v, b.Objects, humanBytes(b.Bytes), b.CreatedAt.Format("2006-01-02"))
					}
				})
			}},
		{Name: "create", Summary: "Create a bucket", Usage: "<name> [--versioning Enabled] [--object-lock] [--owner user]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&versioning, "versioning", "", "Enabled or Suspended")
				fs.BoolVar(&lock, "object-lock", false, "enable Object Lock (implies versioning)")
				fs.StringVar(&owner, "owner", "", "owner user name (default: you)")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: bucket create <name>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				b, err := c.CreateBucket(ctx, admin.CreateBucketRequest{Name: args[0], Versioning: versioning, ObjectLock: lock, Owner: owner})
				if err != nil {
					return err
				}
				return cf.out(b, func(w io.Writer) { row(w, "created bucket", b.Name) })
			}},
		{Name: "show", Summary: "Show a bucket with policy, CORS and lifecycle", Usage: "<name>", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: bucket show <name>")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				b, err := c.GetBucket(ctx, args[0])
				if err != nil {
					return err
				}
				return cf.out(b, func(w io.Writer) {
					row(w, "name", b.Name)
					row(w, "owner", b.OwnerName)
					row(w, "versioning", b.Versioning)
					row(w, "object lock", b.ObjectLock)
					row(w, "objects", fmt.Sprintf("%d (%s)", b.Objects, humanBytes(b.Bytes)))
					row(w, "policy", orDefault(b.Policy))
					row(w, "cors", orDefault(b.CORS))
					row(w, "lifecycle", orDefault(b.Lifecycle))
				})
			}},
		{Name: "set", Summary: "Change bucket settings", Usage: "<name> [--versioning s] [--policy json|@file|-] [--cors xml|@file] [--lifecycle xml|@file] [--public|--private]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.StringVar(&versioning, "versioning", "", "Enabled or Suspended")
				fs.StringVar(&pol, "policy", "", "bucket policy JSON, @file, - for stdin, or 'none' to remove")
				fs.StringVar(&cors, "cors", "", "CORS XML, @file, or 'none'")
				fs.StringVar(&lc, "lifecycle", "", "lifecycle XML, @file, or 'none'")
				fs.BoolVar(&public, "public", false, "grant anonymous read on all objects")
				fs.BoolVar(&private, "private", false, "remove the bucket policy")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: bucket set <name> [flags]")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				req := admin.UpdateBucketRequest{}
				if versioning != "" {
					req.Versioning = &versioning
				}
				for _, f := range []struct {
					in  string
					dst **string
				}{{pol, &req.Policy}, {cors, &req.CORS}, {lc, &req.Lifecycle}} {
					if f.in == "" {
						continue
					}
					v := ""
					if f.in != "none" {
						if v, err = readArgOrFile(f.in); err != nil {
							return err
						}
					}
					*f.dst = &v
				}
				if public || private {
					p := public
					req.PublicRead = &p
				}
				b, err := c.UpdateBucket(ctx, args[0], req)
				if err != nil {
					return err
				}
				return cf.out(b, func(w io.Writer) { row(w, "updated bucket", b.Name) })
			}},
		replicateCmd(),
		{Name: "rm", Summary: "Delete an empty bucket, or with --force a bucket and everything in it", Usage: "<name> [--force]",
			Flags: func(fs *flag.FlagSet) {
				cf = addConnFlags(fs)
				fs.BoolVar(&force, "force", false, "remove every object, version and delete marker first (refused under compliance retention or legal hold)")
			},
			Run: func(ctx context.Context, args []string) error {
				if len(args) != 1 {
					return errors.New("usage: bucket rm <name> [--force]")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				if err := c.DeleteBucket(ctx, args[0], force); err != nil {
					return err
				}
				if force {
					fmt.Println("deleted bucket", args[0], "and all its contents")
				} else {
					fmt.Println("deleted bucket", args[0])
				}
				return nil
			}},
	}}
}

func replicateCmd() *cli.Command {
	var cf *connFlags
	var to, dest, ak, sk, region, prefix, name string
	var pathStyle, enable, disable, remove, status bool
	return &cli.Command{Name: "replicate", Summary: "Replicate a bucket to one or more standby servers (active-standby)",
		Usage: "<bucket> [--name n] --to https://host:9000 --bucket dest --dest-key AK --dest-secret SK [--region r] [--path-style] [--prefix p] | --name n --disable|--enable|--remove | --status",
		Flags: func(fs *flag.FlagSet) {
			cf = addConnFlags(fs)
			fs.StringVar(&name, "name", "", "target name (default: the endpoint host); use it to address one of several targets")
			fs.StringVar(&to, "to", "", "destination endpoint URL")
			fs.StringVar(&dest, "bucket", "", "destination bucket")
			fs.StringVar(&ak, "dest-key", "", "destination access key id")
			fs.StringVar(&sk, "dest-secret", "", "destination secret key")
			fs.StringVar(&region, "region", "", "destination region (default us-east-1)")
			fs.StringVar(&prefix, "prefix", "", "only replicate keys with this prefix")
			fs.BoolVar(&pathStyle, "path-style", false, "use path-style addressing at the destination")
			fs.BoolVar(&enable, "enable", false, "resume a paused target")
			fs.BoolVar(&disable, "disable", false, "pause a target (for failover)")
			fs.BoolVar(&remove, "remove", false, "delete the target (all targets without --name)")
			fs.BoolVar(&status, "status", false, "show lag and last error")
		},
		Run: func(ctx context.Context, args []string) error {
			c, err := cf.client()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				rs, err := c.Replication(ctx)
				if err != nil {
					return err
				}
				return cf.out(rs, func(w io.Writer) { printReplication(w, rs) })
			}
			if len(args) != 1 {
				return errors.New("usage: bucket replicate <bucket> [flags]")
			}
			switch {
			case remove:
				if err := c.DeleteReplication(ctx, args[0], name); err != nil {
					return err
				}
				if name == "" {
					fmt.Println("removed all replication targets of", args[0])
				} else {
					fmt.Println("removed target", name, "from", args[0])
				}
				return nil
			case status:
				st, err := c.GetReplication(ctx, args[0])
				if err != nil {
					return err
				}
				return cf.out(st, func(w io.Writer) { printReplication(w, st) })
			}
			req := admin.ReplicationRequest{Endpoint: to, Bucket: dest, AccessKey: ak, SecretKey: sk, Region: region}
			if prefix != "" {
				req.Prefix = &prefix
			}
			if pathStyle {
				req.PathStyle = &pathStyle
			}
			if enable || disable {
				e := enable
				req.Enabled = &e
			}
			if name == "" && to == "" {
				if !enable && !disable {
					return errors.New("pass --to for a new target, or --name to address an existing one")
				}
				name, err = soleTarget(ctx, c, args[0])
				if err != nil {
					return err
				}
			}
			st, err := c.SetReplication(ctx, args[0], name, req)
			if err != nil {
				return err
			}
			return cf.out(st, func(w io.Writer) { printReplication(w, st) })
		}}
}

// globalReplicateCmd manages server-wide targets that replicate every bucket.
// peersCmd lists servers heard on the LAN.
func peersCmd() *cli.Command {
	var cf *connFlags
	return &cli.Command{Name: "peers", Summary: "List hogshead servers discovered on the local network", Usage: "[flags]",
		Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
		Run: func(ctx context.Context, args []string) error {
			c, err := cf.client()
			if err != nil {
				return err
			}
			ps, err := c.Peers(ctx)
			if err != nil {
				return err
			}
			return cf.out(ps, func(w io.Writer) {
				row(w, "NAME", "ENDPOINT", "VERSION", "LAST SEEN")
				for _, p := range ps {
					row(w, p.Name, p.Endpoint, p.Version, p.LastSeen.Local().Format("15:04:05"))
				}
				if len(ps) == 0 {
					_, _ = fmt.Fprintln(w, "no other servers heard; discovery uses UDP multicast 239.255.77.77:9470 on the local network")
				}
			})
		}}
}

func globalReplicateCmd() *cli.Command {
	var cf *connFlags
	var to, ak, sk, region, prefix, name string
	var pathStyle, enable, disable, remove, syncConfig, noSyncConfig bool
	return &cli.Command{Name: "replicate", Summary: "Replicate every bucket to one or more standby servers",
		Usage: "[--name n] --to https://host:9000 --dest-key AK --dest-secret SK [--region r] [--path-style] [--prefix p] | --name n --disable|--enable|--remove",
		Flags: func(fs *flag.FlagSet) {
			cf = addConnFlags(fs)
			fs.StringVar(&name, "name", "", "target name (default: the endpoint host)")
			fs.StringVar(&to, "to", "", "destination endpoint URL; buckets are mirrored to buckets of the same name")
			fs.StringVar(&ak, "dest-key", "", "destination access key id")
			fs.StringVar(&sk, "dest-secret", "", "destination secret key")
			fs.StringVar(&region, "region", "", "destination region (default us-east-1)")
			fs.StringVar(&prefix, "prefix", "", "only replicate keys with this prefix")
			fs.BoolVar(&pathStyle, "path-style", false, "use path-style addressing at the destination")
			fs.BoolVar(&enable, "enable", false, "resume a paused target")
			fs.BoolVar(&disable, "disable", false, "pause a target (for failover)")
			fs.BoolVar(&remove, "remove", false, "delete the target (all server-wide targets without --name)")
			fs.BoolVar(&syncConfig, "sync-config", false, "also mirror users, access keys, passwords and settings (the key must be an admin on the target)")
			fs.BoolVar(&noSyncConfig, "no-sync-config", false, "stop mirroring users and settings")
		},
		Run: func(ctx context.Context, args []string) error {
			if len(args) != 0 {
				return errors.New("usage: replicate [flags]; per-bucket targets are managed with 'bucket replicate'")
			}
			c, err := cf.client()
			if err != nil {
				return err
			}
			if remove {
				if err := c.DeleteGlobalReplication(ctx, name); err != nil {
					return err
				}
				if name == "" {
					fmt.Println("removed all server-wide replication targets")
				} else {
					fmt.Println("removed server-wide target", name)
				}
				return nil
			}
			if to == "" && !enable && !disable && name == "" {
				ts, err := c.GlobalReplication(ctx)
				if err != nil {
					return err
				}
				st, err := c.Replication(ctx)
				if err != nil {
					return err
				}
				return cf.out(map[string]any{"targets": ts, "status": st}, func(w io.Writer) {
					printGlobalTargets(w, ts)
					var inherited []replication.TargetStatus
					for _, s := range st {
						if s.Target.Inherited {
							inherited = append(inherited, s)
						}
					}
					if len(inherited) > 0 {
						_, _ = fmt.Fprintln(w)
						printReplication(w, inherited)
					}
				})
			}
			req := admin.ReplicationRequest{Endpoint: to, AccessKey: ak, SecretKey: sk, Region: region}
			if prefix != "" {
				req.Prefix = &prefix
			}
			if pathStyle {
				req.PathStyle = &pathStyle
			}
			if enable || disable {
				e := enable
				req.Enabled = &e
			}
			if syncConfig || noSyncConfig {
				sc := syncConfig
				req.SyncConfig = &sc
			}
			if name == "" && to == "" {
				return errors.New("pass --to for a new target, or --name to address an existing one")
			}
			ts, err := c.SetGlobalReplication(ctx, name, req)
			if err != nil {
				return err
			}
			return cf.out(ts, func(w io.Writer) { printGlobalTargets(w, ts) })
		}}
}

func printGlobalTargets(w io.Writer, ts []replication.Target) {
	row(w, "TARGET", "ENDPOINT", "ENABLED", "PREFIX", "PATH STYLE", "SYNC CONFIG")
	for _, t := range ts {
		row(w, t.Name, t.Endpoint, t.Enabled, t.Prefix, t.PathStyle, t.SyncConfig)
	}
}

// readPassword prompts for a password on stdin, twice when stdin is a
// terminal, without echo.
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "New password: ")
	p1, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat password: ")
	p2, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(p1) != string(p2) {
		return "", errors.New("passwords do not match")
	}
	return string(p1), nil
}

// soleTarget returns the name of the bucket's only target, or an error asking
// for --name when there are several.
func soleTarget(ctx context.Context, c *adminclient.Client, bucket string) (string, error) {
	st, err := c.GetReplication(ctx, bucket)
	if err != nil {
		return "", err
	}
	if len(st) != 1 {
		return "", fmt.Errorf("bucket %s has %d targets; pass --name", bucket, len(st))
	}
	return st[0].Target.Name, nil
}

func printReplication(w io.Writer, rs []replication.TargetStatus) {
	row(w, "BUCKET", "TARGET", "DESTINATION", "ENABLED", "LAG", "PUSHED", "LAST SYNC", "ERROR")
	for _, r := range rs {
		last := "never"
		if !r.LastSync.IsZero() {
			last = r.LastSync.Local().Format("2006-01-02 15:04:05")
		}
		name := r.Target.Name
		if r.Target.Inherited {
			name += " (server-wide)"
		}
		bucket, dest := r.Bucket, r.Target.Endpoint+"/"+r.Target.Bucket
		if r.Bucket == replication.ConfigBucket {
			bucket, dest = "(users & settings)", r.Target.Endpoint+"/_admin"
		}
		row(w, bucket, name, dest, r.Target.Enabled, r.Lag, r.Pushed, last, r.LastError)
	}
}

func orDefault(s string) string {
	if s == "" {
		return "(default)"
	}
	return s
}

func auditCmd() *cli.Command {
	var cf *connFlags
	var limit int
	var before int64
	return &cli.Command{Name: "audit", Summary: "Show the audit log", Usage: "[--limit n] [--before id]",
		Flags: func(fs *flag.FlagSet) {
			cf = addConnFlags(fs)
			fs.IntVar(&limit, "limit", 50, "entries to show")
			fs.Int64Var(&before, "before", 0, "show entries with an id below this")
		},
		Run: func(ctx context.Context, args []string) error {
			c, err := cf.client()
			if err != nil {
				return err
			}
			es, err := c.Audit(ctx, limit, before)
			if err != nil {
				return err
			}
			return cf.out(es, func(w io.Writer) {
				row(w, "ID", "TIME", "ACTOR", "ACTION", "TARGET", "DETAIL")
				for _, e := range es {
					row(w, e.ID, e.At.Local().Format("2006-01-02 15:04:05"), e.Actor, e.Action, e.Target, e.Detail)
				}
			})
		}}
}

func settingCmd() *cli.Command {
	var cf *connFlags
	return &cli.Command{Name: "setting", Summary: "Server settings", Subcommands: []*cli.Command{
		{Name: "list", Summary: "List settings", Usage: "[flags]", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				ss, err := c.Settings(ctx)
				if err != nil {
					return err
				}
				return cf.out(ss, func(w io.Writer) {
					row(w, "KEY", "VALUE", "DESCRIPTION")
					for _, s := range ss {
						row(w, s.Key, orDefault(s.Value), s.Description)
					}
				})
			}},
		{Name: "set", Summary: "Set a setting (empty value removes it)", Usage: "<key> <value>", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				if len(args) < 1 || len(args) > 2 {
					return errors.New("usage: setting set <key> [value]")
				}
				c, err := cf.client()
				if err != nil {
					return err
				}
				v := ""
				if len(args) == 2 {
					v = args[1]
				}
				if err := c.SetSetting(ctx, args[0], v); err != nil {
					return err
				}
				fmt.Println("set", args[0], "=", v)
				return nil
			}},
	}}
}

func configCmd() *cli.Command {
	var cf *connFlags
	return &cli.Command{Name: "config", Summary: "Node-local configuration", Subcommands: []*cli.Command{
		{Name: "show", Summary: "Show the running configuration", Usage: "[flags]", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				v, err := c.Config(ctx)
				if err != nil {
					return err
				}
				return cf.out(v, func(w io.Writer) {
					_, _ = fmt.Fprintln(w, v.TOML)
					if len(v.PendingRestart) > 0 {
						_, _ = fmt.Fprintln(w, "# restart needed for:", strings.Join(v.PendingRestart, ", "))
					}
				})
			}},
		{Name: "reload", Summary: "Re-read the configuration file", Usage: "[flags]", Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
			Run: func(ctx context.Context, args []string) error {
				c, err := cf.client()
				if err != nil {
					return err
				}
				v, err := c.ReloadConfig(ctx)
				if err != nil {
					return err
				}
				fmt.Println("reloaded", v.Path)
				if len(v.PendingRestart) > 0 {
					fmt.Println("restart needed for:", strings.Join(v.PendingRestart, ", "))
				}
				return nil
			}},
	}}
}

func apiCmd() *cli.Command {
	var cf *connFlags
	return &cli.Command{Name: "api", Summary: "Raw admin API call", Usage: "<METHOD> <path> [json-body|@file|-]", Hidden: true,
		Flags: func(fs *flag.FlagSet) { cf = addConnFlags(fs) },
		Run: func(ctx context.Context, args []string) error {
			if len(args) < 2 {
				return errors.New("usage: api <METHOD> <path> [body]")
			}
			c, err := cf.client()
			if err != nil {
				return err
			}
			var in any
			if len(args) == 3 {
				s, err := readArgOrFile(args[2])
				if err != nil {
					return err
				}
				in = jsonRaw(s)
			}
			var out any
			if err := c.Raw(ctx, strings.ToUpper(args[0]), args[1], in, &out); err != nil {
				return err
			}
			return cf.out(out, nil)
		}}
}

// CLIRoutes maps admin API routes to the CLI command that exposes them; a
// test asserts every route is covered.
var CLIRoutes = map[string]string{
	"GET /status":                        "status",
	"GET /me":                            "api",
	"GET /openapi.json":                  "api",
	"GET /users":                         "user list",
	"POST /users":                        "user add",
	"GET /users/{name}":                  "user show",
	"PATCH /users/{name}":                "user set",
	"DELETE /users/{name}":               "user rm",
	"PUT /users/{name}/password":         "user passwd",
	"PUT /users/{name}":                  "user import",
	"PUT /users/{name}/keys/{ak}":        "key import",
	"GET /peers":                         "peers",
	"GET /cellar":                        "cellar status",
	"GET /usage":                         "usage",
	"GET /errors":                        "errors",
	"POST /cellar/init":                  "cellar init",
	"PUT /cellar":                        "cellar set",
	"POST /cellar/join":                  "cellar join",
	"DELETE /cellar/join":                "cellar cancel-join",
	"POST /cellar/pending/{id}/approve":  "cellar approve",
	"DELETE /cellar/pending/{id}":        "cellar reject",
	"DELETE /cellar/members/{id}":        "cellar remove",
	"POST /cellar/leave":                 "cellar leave",
	"DELETE /cellar/approvals/{id}":      "cellar forget-approval",
	"GET /users/{name}/keys":             "key list",
	"POST /users/{name}/keys":            "key create",
	"GET /keys":                          "key list",
	"PATCH /keys/{id}":                   "key enable|disable",
	"DELETE /keys/{id}":                  "key revoke",
	"GET /buckets":                       "bucket list",
	"POST /buckets":                      "bucket create",
	"GET /buckets/{name}":                "bucket show",
	"PATCH /buckets/{name}":              "bucket set",
	"DELETE /buckets/{name}":             "bucket rm",
	"GET /audit":                         "audit",
	"GET /settings":                      "setting list",
	"PUT /settings/{key}":                "setting set",
	"GET /config":                        "config show",
	"POST /config/reload":                "config reload",
	"GET /replication":                   "bucket replicate",
	"GET /buckets/{name}/replication":    "bucket replicate",
	"PUT /buckets/{name}/replication":    "bucket replicate",
	"DELETE /buckets/{name}/replication": "bucket replicate",
	"GET /replication/global":            "replicate",
	"GET /transfers":                     "transfer list",
	"POST /transfers":                    "transfer export",
	"GET /transfers/{id}":                "transfer show",
	"DELETE /transfers/{id}":             "transfer cancel",
	"PUT /replication/global":            "replicate",
	"DELETE /replication/global":         "replicate",
}

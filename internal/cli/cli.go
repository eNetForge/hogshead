// Package cli is a tiny subcommand framework on top of the standard flag
// package. It exists so the binary has no dependency on a CLI library.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ErrUsage signals that usage was printed and the exit code should be 2.
var ErrUsage = errors.New("usage")

// Command is a leaf or group command.
type Command struct {
	Name    string
	Summary string
	Usage   string // one-line argument synopsis, e.g. "<bucket> [flags]"
	// Flags registers flags on fs; may be nil.
	Flags func(fs *flag.FlagSet)
	// Run executes the command with positional args after flag parsing.
	Run func(ctx context.Context, args []string) error
	// Subcommands makes this a group.
	Subcommands []*Command
	// Hidden commands are not listed in help.
	Hidden bool
}

// Execute dispatches args (without the program name) against root.
func Execute(ctx context.Context, root *Command, args []string, out io.Writer) error {
	return run(ctx, root, root.Name, args, out)
}

func run(ctx context.Context, c *Command, path string, args []string, out io.Writer) error {
	if len(c.Subcommands) > 0 {
		if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
			printGroupHelp(out, c, path)
			if len(args) == 0 {
				return ErrUsage
			}
			return nil
		}
		for _, sc := range c.Subcommands {
			if sc.Name == args[0] {
				return run(ctx, sc, path+" "+sc.Name, args[1:], out)
			}
		}
		_, _ = fmt.Fprintf(out, "unknown command %q\n\n", args[0])
		printGroupHelp(out, c, path)
		return ErrUsage
	}
	fs := flag.NewFlagSet(path, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(out, "Usage: %s %s\n\n%s\n", path, c.Usage, c.Summary)
		if countFlags(fs) > 0 {
			_, _ = fmt.Fprintln(out, "\nFlags:")
			fs.PrintDefaults()
		}
	}
	if c.Flags != nil {
		c.Flags(fs)
	}
	// Allow flags after positional arguments: parse, take one positional,
	// parse the rest again.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return ErrUsage
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
		if len(args) == 0 {
			break
		}
	}
	if c.Run == nil {
		fs.Usage()
		return ErrUsage
	}
	return c.Run(ctx, positional)
}

func countFlags(fs *flag.FlagSet) int {
	n := 0
	fs.VisitAll(func(*flag.Flag) { n++ })
	return n
}

func printGroupHelp(out io.Writer, c *Command, path string) {
	_, _ = fmt.Fprintf(out, "Usage: %s <command> [flags]\n\n%s\n\nCommands:\n", path, c.Summary)
	subs := make([]*Command, 0, len(c.Subcommands))
	for _, s := range c.Subcommands {
		if !s.Hidden {
			subs = append(subs, s)
		}
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name < subs[j].Name })
	w := 0
	for _, s := range subs {
		if len(s.Name) > w {
			w = len(s.Name)
		}
	}
	for _, s := range subs {
		_, _ = fmt.Fprintf(out, "  %s%s  %s\n", s.Name, strings.Repeat(" ", w-len(s.Name)), s.Summary)
	}
	_, _ = fmt.Fprintf(out, "\nRun '%s <command> -h' for details.\n", path)
}

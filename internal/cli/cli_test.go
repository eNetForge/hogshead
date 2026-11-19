package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestExecute(t *testing.T) {
	var got []string
	var verbose bool
	root := &Command{Name: "app", Summary: "An app", Subcommands: []*Command{
		{Name: "run", Summary: "Run it", Usage: "<thing>", Flags: func(fs *flag.FlagSet) { fs.BoolVar(&verbose, "v", false, "verbose") },
			Run: func(_ context.Context, args []string) error { got = args; return nil }},
		{Name: "group", Summary: "A group", Subcommands: []*Command{
			{Name: "leaf", Summary: "Leaf", Run: func(_ context.Context, args []string) error { got = append([]string{"leaf"}, args...); return nil }},
		}},
		{Name: "hidden", Hidden: true, Run: func(context.Context, []string) error { return errors.New("boom") }},
		{Name: "norun", Summary: "no run"},
	}}
	var out bytes.Buffer
	if err := Execute(context.Background(), root, []string{"run", "-v", "x", "y"}, &out); err != nil || !verbose || len(got) != 2 {
		t.Fatalf("run: %v %v %v", err, verbose, got)
	}
	verbose = false
	if err := Execute(context.Background(), root, []string{"run", "x", "-v", "y"}, &out); err != nil || !verbose || len(got) != 2 || got[1] != "y" {
		t.Fatalf("interspersed: %v %v %v", err, verbose, got)
	}
	if err := Execute(context.Background(), root, []string{"group", "leaf", "z"}, &out); err != nil || got[0] != "leaf" || got[1] != "z" {
		t.Fatalf("nested: %v %v", err, got)
	}
	out.Reset()
	if err := Execute(context.Background(), root, nil, &out); !errors.Is(err, ErrUsage) || !strings.Contains(out.String(), "run") || strings.Contains(out.String(), "hidden") {
		t.Fatalf("root help: %v %s", err, out.String())
	}
	out.Reset()
	if err := Execute(context.Background(), root, []string{"help"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Execute(context.Background(), root, []string{"nope"}, &out); !errors.Is(err, ErrUsage) || !strings.Contains(out.String(), "unknown command") {
		t.Fatal("unknown")
	}
	out.Reset()
	if err := Execute(context.Background(), root, []string{"run", "-h"}, &out); err != nil || !strings.Contains(out.String(), "Usage: app run <thing>") || !strings.Contains(out.String(), "-v") {
		t.Fatalf("leaf help: %v %s", err, out.String())
	}
	if err := Execute(context.Background(), root, []string{"run", "-bogus"}, &out); !errors.Is(err, ErrUsage) {
		t.Fatal("bad flag")
	}
	if err := Execute(context.Background(), root, []string{"hidden"}, &out); err == nil || err.Error() != "boom" {
		t.Fatal("hidden runs")
	}
	if err := Execute(context.Background(), root, []string{"norun"}, &out); !errors.Is(err, ErrUsage) {
		t.Fatal("norun")
	}
}

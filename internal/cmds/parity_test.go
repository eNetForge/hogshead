package cmds

import (
	"context"
	"strings"
	"testing"

	"github.com/enetforge/hogshead/internal/admin"
	"github.com/enetforge/hogshead/internal/cli"
	"github.com/enetforge/hogshead/internal/testutil"
)

// TestCLIParity asserts that every admin API route is reachable from a CLI
// command, and that every mapped command exists in the tree.
func TestCLIParity(t *testing.T) {
	n := testutil.Start(t)
	h := admin.New(admin.Deps{DB: n.Core.DB, Propose: n.Core.ProposeRaw, Cfg: n.Core.Cfg})
	for _, rt := range h.Routes() {
		key := rt.Method + " " + rt.Path
		cmd, ok := CLIRoutes[key]
		if !ok {
			t.Errorf("admin route %s has no CLI command", key)
			continue
		}
		for _, alt := range strings.Split(cmd, "|") {
			path := strings.Fields(strings.TrimSpace(alt))
			if len(path) == 0 {
				continue
			}
			// "key enable|disable" shares the prefix "key".
			if len(path) == 1 && strings.Contains(cmd, " ") && !strings.HasPrefix(alt, strings.Fields(cmd)[0]) {
				path = append([]string{strings.Fields(cmd)[0]}, path...)
			}
			if !commandExists(Root(), path) {
				t.Errorf("CLI command %q for %s does not exist", alt, key)
			}
		}
	}
	for key := range CLIRoutes {
		found := false
		for _, rt := range h.Routes() {
			if rt.Method+" "+rt.Path == key {
				found = true
			}
		}
		if !found {
			t.Errorf("CLIRoutes has stale entry %s", key)
		}
	}
}

func commandExists(root *cli.Command, path []string) bool {
	cur := root
	for _, p := range path {
		var next *cli.Command
		for _, sc := range cur.Subcommands {
			if sc.Name == p {
				next = sc
			}
		}
		if next == nil {
			return false
		}
		cur = next
	}
	return true
}

func TestRootHelp(t *testing.T) {
	var sb strings.Builder
	err := cli.Execute(context.Background(), Root(), []string{"help"}, &sb)
	if err != nil || !strings.Contains(sb.String(), "bucket") || !strings.Contains(sb.String(), "quickstart") {
		t.Fatalf("%v %s", err, sb.String())
	}
}

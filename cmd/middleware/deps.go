package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/dcimorra/middleware-public/internal/deps"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/settings"
)

func init() {
	subcommands["deps"] = depsCommand
	extraUsage += `  deps         status | check | install <claude|gemini>   (the agent CLIs under modules/node)
`
}

// depsCommand works on the files under the root, like `modules`: the
// running process sees a new binary on its next turn, because it looks
// the path up every time.
func depsCommand(args []string) error {
	fs := flag.NewFlagSet("deps", flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	dir := settings.Root(*root)
	dm := deps.New(dir, events.New(filepath.Join(dir, "data", "events")))
	dm.AutoUpdate = func() bool {
		s, err := settings.Load(dir)
		return err == nil && s.Deps.AutoUpdateOn()
	}
	progress := func(line string) { fmt.Println("  " + line) }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	switch {
	case len(rest) == 0 || rest[0] == "status":
		return printDeps(dm.Statuses())
	case rest[0] == "check":
		return printDeps(dm.Check(ctx, progress))
	case rest[0] == "install" && len(rest) == 2:
		if err := dm.Install(ctx, rest[1], progress); err != nil {
			return err
		}
		return printDeps(dm.Statuses())
	}
	return fmt.Errorf("usage: middleware deps status | check | install <%s>", joinNames())
}

func joinNames() string {
	out := ""
	for i, n := range deps.Names() {
		if i > 0 {
			out += "|"
		}
		out += n
	}
	return out
}

func printDeps(sts []deps.Status) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\tLATEST\tWHERE\tNOTE")
	for _, s := range sts {
		where := s.Path
		if s.Managed {
			where += " (managed)"
		}
		if !s.Installed {
			where = "not installed"
		}
		note := s.Error
		if s.Outdated {
			note = "update available"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Version, s.Latest, where, note)
	}
	return w.Flush()
}

package main

import (
	"fmt"
	"sort"

	"github.com/dcimorra/middleware-public/internal/app"
)

// extraUsage lists the subcommands other files register.
var extraUsage = ""

// hookBuilder is one thing that plugs into the app, with the place it
// takes in the chain. Order is explicit because the request chain is the
// product: app.Run wraps the proxy with the hooks in list order, so the
// first hook is the outermost, and "whichever file's init ran first" is
// not a rule anyone can read. The constants live in wire.go.
type hookBuilder struct {
	Name  string
	Order int
	Build func(a *app.App) app.Hook
}

// hookBuilders is what each file appends to in its init. hooks sorts it,
// so the order of the files on disk does not matter.
var hookBuilders []hookBuilder

func hooks(a *app.App) []app.Hook {
	sorted := append([]hookBuilder(nil), hookBuilders...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Order < sorted[j].Order })
	out := make([]app.Hook, 0, len(sorted))
	for _, b := range sorted {
		if h := b.Build(a); h != nil {
			out = append(out, h)
		}
	}
	return out
}

var subcommands = map[string]func(args []string) error{}

func dispatch(cmd string, args []string) error {
	if f, ok := subcommands[cmd]; ok {
		return f(args)
	}
	return fmt.Errorf("unknown command %q (try: middleware help)", cmd)
}

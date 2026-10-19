package main

import (
	"fmt"

	"github.com/dcimorra/middleware-public/internal/app"
)

// extraUsage lists the subcommands other files register.
var extraUsage = ""

// hooks returns what plugs into the app: api, panel, waf, inbound, modules…
// Each file that adds one appends to this list in its init.
var hookBuilders []func(a *app.App) app.Hook

func hooks(a *app.App) []app.Hook {
	out := make([]app.Hook, 0, len(hookBuilders))
	for _, b := range hookBuilders {
		if h := b(a); h != nil {
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

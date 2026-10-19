package main

import (
	"github.com/dcimorra/middleware-public/internal/api"
	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/cli"
)

// The API hook and the operator CLI. The hook answers the panel host and
// the unix socket; the CLI commands talk to it.
func init() {
	hookBuilders = append(hookBuilders, func(a *app.App) app.Hook { return api.NewHook(a, Version) })
	for _, name := range []string{"setup", "site", "events", "system", "token"} {
		subcommands[name] = cli.Command(name)
	}
	extraUsage += cli.Usage
}

package main

import (
	"context"
	"path/filepath"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/vitals"
)

var theVitals *vitals.Monitor

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "vitals", Order: orderGuard + 1, Build: func(a *app.App) app.Hook {
		theVitals = vitals.New(vitals.Options{DataDir: dataDir(a), SitesDir: filepath.Join(a.Root, "sites"), Bus: a.Bus})
		return hook{run: func(ctx context.Context) { go theVitals.Run(ctx.Done()) }}
	}})
}

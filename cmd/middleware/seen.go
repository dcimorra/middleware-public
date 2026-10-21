package main

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/edge"
	"github.com/dcimorra/middleware-public/internal/proxy"
	"github.com/dcimorra/middleware-public/internal/seen"
)

// orderSeen counts a site's callers after the WAF (a banned address
// flooding would only add noise) and before the security rules, so what
// they shut out is counted too: that is what a rule is written from.
const orderSeen = 22

var theSeen *seen.Book

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "seen", Order: orderSeen, Build: buildSeen})
}

func buildSeen(a *app.App) app.Hook {
	theSeen = seen.New(filepath.Join(dataDir(a), "seen"))
	return hook{
		wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if sn, ok := a.Engine.Lookup(proxy.Host(r)); ok && sn.Domain != "" {
					theSeen.Note(sn.Domain, edge.Country(r), r.UserAgent())
				}
				next.ServeHTTP(w, r)
			})
		},
		// Run must return: the loop goes on its own.
		run: func(ctx context.Context) { go flushLoop(ctx) },
	}
}

func flushLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			theSeen.Flush()
			return
		case <-t.C:
			theSeen.Flush()
		}
	}
}

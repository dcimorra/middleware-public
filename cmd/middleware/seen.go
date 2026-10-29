package main

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/app"
	"github.com/dcimorra/the-middleware/internal/edge"
	"github.com/dcimorra/the-middleware/internal/proxy"
	"github.com/dcimorra/the-middleware/internal/seen"
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
	return seenHook{a: a}
}

// seenHook counts, flushes every minute, and once more when the process
// stops (app.Stopper).
type seenHook struct{ a *app.App }

func (h seenHook) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sn, ok := h.a.Engine.Lookup(proxy.Host(r)); ok && sn.Domain != "" {
			theSeen.Note(sn.Domain, edge.Country(r), r.UserAgent())
		}
		next.ServeHTTP(w, r)
	})
}

func (h seenHook) Run(ctx context.Context, _ *app.App) error {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				theSeen.Flush()
			}
		}
	}()
	return nil
}

func (h seenHook) Stop() { theSeen.Flush() }

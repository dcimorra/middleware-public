package main

import (
	"context"
	"net/http"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/panel"
)

// The panel is static files; what answers the panel domain is decided by
// whoever mounts the API, which puts /api/v1 in front of these files. Until
// that wiring exists the panel takes the slot by itself so the shell can at
// least be opened, and only when nobody else has: a handler the API hook
// installed is never overridden here.
func init() {
	hookBuilders = append(hookBuilders, func(a *app.App) app.Hook { return panelHook{} })
}

type panelHook struct{}

func (panelHook) Wrap(next http.Handler) http.Handler { return next }

func (panelHook) Run(_ context.Context, a *app.App) error {
	if a.Proxy != nil && a.Proxy.Panel == nil {
		a.Proxy.Panel = panel.Handler()
	}
	return nil
}

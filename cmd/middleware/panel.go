package main

import (
	"context"
	"net/http"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/panel"
)

// The panel is static files behind the API: the API answers /api/v1 on the
// panel host and hands everything else to these files. The hook is built
// right after the API's so apiHook exists; it wraps nothing and only runs
// to install the UI.
func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "panel", Order: orderAPI + 1, Build: func(a *app.App) app.Hook { return panelHook{} }})
}

type panelHook struct{}

func (panelHook) Wrap(next http.Handler) http.Handler { return next }

func (panelHook) Run(_ context.Context, a *app.App) error {
	if apiHook != nil && apiHook.Server != nil {
		apiHook.Server.UI = panel.Handler()
		return nil
	}
	// Without the API (never in the real binary, but cheap to be safe) the
	// shell still opens so the failure is visible in a browser.
	if a.Proxy != nil && a.Proxy.Panel == nil {
		a.Proxy.Panel = panel.Handler()
	}
	return nil
}

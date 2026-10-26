package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/modules/openspec"
	"github.com/dcimorra/middleware-public/internal/specs"
)

// specsInit runs `openspec init` in a site's folder: with the site's own
// HOME, the managed CLIs and their Node on PATH, and OpenSpec's skills
// written for every agent the house can run now, so whichever answers the
// site's conversation finds them.
func specsInit(a *app.App, set *moduleSet) func(ctx context.Context, domain string) (string, error) {
	return func(ctx context.Context, domain string) (string, error) {
		bin, err := set.OpenSpec.Path()
		if err != nil {
			return "", err
		}
		dir := filepath.Join(a.Root, "sites", domain)
		if sn, ok := a.Engine.Lookup(domain); ok && sn.Domain == domain {
			dir = sn.Dir
		}
		// A site that serves its own folder would publish openspec/ to
		// anyone who asks for it: its files move under a root of their
		// own (public/, dist/) first.
		if mf, err := manifest.Load(dir); err == nil && servesItsFolder(mf) {
			return "", errors.New("this site serves its whole folder, so openspec/ would be public: move its files under a root of their own (static.root) first")
		}
		home := filepath.Join(a.Root, "data", "homes", domain)
		if err := os.MkdirAll(home, 0o700); err != nil {
			return "", err
		}
		var agents []string
		if chatManager != nil {
			for _, k := range chatManager.Providers() {
				if ok, _ := chatManager.ProviderInfo(k); ok {
					agents = append(agents, k)
				}
			}
		}
		out, err := specs.Init(ctx, dir, specs.InitOptions{Bin: bin, Home: home, Path: set.Deps.Path(), Agents: agents})
		level, msg := "", "OpenSpec set up in the project"
		if err != nil {
			level, msg = "warn", err.Error()
		}
		a.Bus.Emit(events.Event{Type: "specs.init", Subject: domain, Actor: "openspec", Level: level, Message: msg})
		return out, err
	}
}

// specsOnCreate sets every new site up for OpenSpec while the module is
// on. Hung on the site.created event and not on one of the ways a site is
// created: the panel, the CLI, the house's conversation and Mask all end
// in sites.Create, which says so on the bus.
func specsOnCreate(a *app.App, set *moduleSet, initSpecs func(context.Context, string) (string, error)) {
	ch, stop := a.Bus.Subscribe()
	defer stop()
	for e := range ch {
		if e.Type != "site.created" || !set.Manager.Enabled(openspec.Key) {
			continue
		}
		go func(domain string) { _, _ = initSpecs(context.Background(), domain) }(e.Subject)
	}
}

// servesItsFolder says the site's files are served from the folder itself
// rather than from a root inside it (public/, dist/).
func servesItsFolder(m *manifest.Manifest) bool {
	if m == nil || m.Mode() != manifest.ModeStatic || m.Upstream != "" {
		return false
	}
	return m.Static == nil || m.Static.Root == "" || m.Static.Root == "." || m.Static.Root == "./"
}

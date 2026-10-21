package main

import (
	"context"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/attack"
	"github.com/dcimorra/middleware-public/internal/edge"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/proxy"
	"github.com/dcimorra/middleware-public/internal/waf"
)

var theAttack *attack.Guard

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "attack", Order: orderAttack, Build: buildAttack})
}

func buildAttack(a *app.App) app.Hook {
	g, err := attack.New(attack.Options{
		DataDir:  dataDir(a),
		Host:     proxy.Host,
		ClientIP: edge.ClientIP,
		Global: func() *attack.Rules {
			if theWAF == nil {
				return nil
			}
			return theWAF.GlobalAttack()
		},
		AllowAgents: func() []string {
			if theWAF == nil {
				return nil
			}
			return theWAF.AllowAgents()
		},
		// Only the panel's own name: a request to localhost comes from
		// this machine, and that is the way back in when everything else
		// is behind the challenge.
		Panel: func(host string) bool { return host != "" && host == a.Proxy.PanelDomain() },
		Lookup: func(host string) (attack.Site, bool) {
			sn, ok := a.Engine.Lookup(host)
			if !ok || sn.Manifest == nil {
				return attack.Site{}, false
			}
			m := sn.Manifest
			site := attack.Site{Domain: sn.Domain, Title: m.Title, Rules: waf.Effective(m.Attack, m.WAF != nil && m.WAF.UnderAttack)}
			if m.Notice != nil {
				site.Theme = m.Notice.Theme
			}
			return site, true
		},
	})
	if err != nil {
		// Without its key the challenge cannot sign anything. Serving
		// without it is what the mode looked like before it was switched
		// on, so the event has to say so loudly.
		a.Bus.Emit(events.Event{Type: "attack.error", Actor: "engine", Level: "error", Message: "under-attack mode is not available, nothing will be challenged: " + err.Error()})
		return nil
	}
	theAttack = g
	return hook{wrap: g.Wrap, run: func(ctx context.Context) { go sweepExpiredAttacks(ctx, a) }}
}

// sweepExpiredAttacks writes a passed deadline down as off. The clock
// already stopped charging (Rules.Active looks at it on every request);
// this is so the manifest stops saying "under attack" about something
// that charges nothing, which is what whoever comes next reads. Both are
// needed: with only this, a manifest that cannot be written would charge
// for ever. The rest of the block (bits, skip) is kept, so switching it on
// again is one click.
func sweepExpiredAttacks(ctx context.Context, a *app.App) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		sweepOnce(a, time.Now())
	}
}

func sweepOnce(a *app.App, now time.Time) {
	if theWAF == nil {
		return
	}
	off := func(r *attack.Rules) { r.On, r.Until = false, "" }
	if g := theWAF.GlobalAttack(); g != nil && g.On && g.Expired(now) {
		report(a, "", theWAF.SetAttack("", off))
	}
	for _, sn := range a.Engine.Sites() {
		if m := sn.Manifest; m != nil && m.Attack != nil && m.Attack.On && m.Attack.Expired(now) {
			report(a, sn.Domain, theWAF.SetAttack(sn.Domain, off))
		}
	}
}

func report(a *app.App, domain string, err error) {
	if err == nil {
		return
	}
	where := strings.TrimSpace(domain)
	if where == "" {
		where = "the global policy"
	}
	a.Bus.Emit(events.Event{Type: "attack.error", Subject: domain, Actor: "engine", Level: "error",
		Message: "under-attack deadline passed but could not be written down in " + where + " (it no longer charges): " + err.Error()})
}

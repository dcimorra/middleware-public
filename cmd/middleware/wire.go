package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/acme"
	"github.com/dcimorra/middleware-public/internal/api"
	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/deps"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/firewall"
	"github.com/dcimorra/middleware-public/internal/helper"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/modules"
	"github.com/dcimorra/middleware-public/internal/modules/agents"
	"github.com/dcimorra/middleware-public/internal/modules/cloudflare"
	"github.com/dcimorra/middleware-public/internal/modules/github"
	"github.com/dcimorra/middleware-public/internal/modules/vpn"
	"github.com/dcimorra/middleware-public/internal/publish"
)

// The order of the hooks is the shape of the process, so it is written
// here once and not left to the order in which the files' inits ran.
//
// Request chain, outermost first: inbound (the ACME challenge and the
// doors, before anything else sees the request), waf (refuse the abusive
// before anything is spent on them), access (the door the operator
// closed), traffic (count whatever came out, error pages included), then
// the proxy. Everything after traffic wraps nothing and only runs: the
// api hangs from the proxy's panel slot, the chat, the tasks and the
// modules are services, the guard is a watchdog. The wire hook goes last
// because it reaches into every instance the others created.
const (
	orderInbound = 10
	orderWAF     = 20
	orderAccess  = 30
	orderTraffic = 40
	orderAPI     = 50
	orderChat    = 60
	orderTasks   = 70
	orderModules = 80
	orderGuard   = 90
	orderWire    = 100
)

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "wire", Order: orderWire, Build: buildWire})
}

// The agent modules are kept so the wiring can re-apply them at start.
var (
	agentClaude *agents.Module
	agentGemini *agents.Module
)

// registerExtraModules adds what buildModules cannot build on its own:
// the VPN, which needs the helper and the secrets box, and the two agent
// modules, which replace providers in the registry the chat uses. It runs
// in the service and in the CLI alike, so `middleware modules` lists them.
func registerExtraModules(mgr *modules.Manager, root string, bus *events.Bus, dm *deps.Manager) error {
	box, err := modules.OpenBoxSecrets(root)
	if err != nil {
		return err
	}
	v, err := vpn.New(vpn.Options{Root: root, Helper: helper.Dial(""), Secrets: box, Bus: bus})
	if err != nil {
		return fmt.Errorf("vpn: %w", err)
	}
	mgr.Register(modules.VPNModule(v))
	reg := providers()
	agentClaude = agents.Claude(reg)
	agentGemini = agents.Gemini(reg)
	for _, m := range []*agents.Module{agentClaude, agentGemini} {
		m.Deps = dm
		m.Store = mgr
		mgr.Register(m)
	}
	return nil
}

// wireHook is the integration layer: it gives the API the routes of every
// other package and does the housekeeping that needs more than one of
// them. It wraps nothing.
type wireHook struct {
	set       *moduleSet
	publisher *publish.Publisher
}

func buildWire(a *app.App) app.Hook {
	if apiHook == nil {
		a.Bus.Emit(events.Event{Type: "wire.error", Actor: "engine", Level: "error", Message: "the api hook was not built; nothing else is reachable"})
		return nil
	}
	srv := apiHook.Server
	w := &wireHook{}
	set, err := modulesFor(a)
	if err != nil {
		// modulesHook.Run fails the start with the same error; the
		// routes below are registered anyway so a partial process is at
		// least an honest one.
		a.Bus.Emit(events.Event{Type: "modules.error", Actor: "engine", Level: "error", Message: err.Error()})
	} else {
		w.set = set
		srv.RegisterModules(api.ModulesOptions{Manager: set.Manager, Public: []string{"POST " + github.Key + "/hook"}})
		if err := agents.Restore(context.Background(), set.Manager, agentClaude, agentGemini); err != nil {
			a.Bus.Emit(events.Event{Type: "module.restore_failed", Actor: "engine", Level: "error", Message: err.Error()})
		}
		srv.SetDeployer(githubDeployer(a, set))
		srv.RegisterDeps(api.DepsOptions{Manager: set.Deps})
		// The CLIs a turn runs come first in its PATH, with their Node.
		if chatManager != nil {
			chatManager.SetPath(set.Deps.Path)
		}
		go set.Deps.Run(context.Background())
	}
	if acmeIssuer != nil {
		acmeIssuer.SetDNS01(dns01For(cloudflareDNS{set: set}))
	}
	if inboundHook != nil {
		w.publisher = buildPublisher(a, set)
		srv.RegisterInbound(api.InboundOptions{Hook: inboundHook, DNS: cloudflareDNS{set: set}, Publisher: w.publisher})
	}
	srv.RegisterWAF(api.WAFOptions{WAF: theWAF, Traffic: theTraffic})
	srv.RegisterAgents(api.AgentsOptions{Registry: providers(), Chat: chatManager})
	if taskScheduler != nil {
		srv.RegisterTasks(api.TasksOptions{Scheduler: taskScheduler, Root: a.Root})
	}
	srv.RegisterGuard(api.GuardOptions{Guard: theGuard})
	// A block at layer 7 is pushed to the kernel through the helper, so
	// a flood stops before it reaches Go. Without the helper the firewall
	// says so once and the block stays at layer 7.
	fw := firewall.New(helper.Dial(""), a.Bus)
	if theWAF != nil {
		theWAF.OnBlock = func(ip net.IP, d time.Duration) { _ = fw.OnBlock(ip.String(), d) }
	}
	return w
}

func (w *wireHook) Wrap(next http.Handler) http.Handler { return next }

// Run reserves the site port range and says the process is ready. Neither
// can fail the start: an installation without the helper runs with the
// kernel's ephemeral range overlapping ours, which is a warning, not an
// outage.
func (w *wireHook) Run(ctx context.Context, a *app.App) error {
	hc := helper.Dial("")
	from, to := a.Settings.Ports.From, a.Settings.Ports.To
	if hc.Available() {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := hc.ReservePorts(rctx, from, to)
		cancel()
		if err != nil {
			a.Bus.Emit(events.Event{Type: "ports.reserve_failed", Actor: "engine", Level: "warn", Message: err.Error(), Data: map[string]any{"from": from, "to": to}})
		} else {
			a.Bus.Emit(events.Event{Type: "ports.reserved", Actor: "engine", Data: map[string]any{"from": from, "to": to}})
		}
	} else {
		a.Bus.Emit(events.Event{Type: "ports.not_reserved", Actor: "engine", Level: "warn", Message: "the helper is not reachable on " + hc.Path() + "; the site port range is not reserved from the kernel's ephemeral range"})
	}
	if w.publisher != nil {
		go w.publisher.Run(ctx)
	}
	var enabled []string
	if w.set != nil {
		for _, info := range w.set.Manager.List() {
			if info.Enabled {
				enabled = append(enabled, info.Key)
			}
		}
	}
	var doors []string
	if inboundHook != nil {
		st := inboundHook.Status().Doors
		if st.Tunnel.Open {
			doors = append(doors, "tunnel")
		}
		if st.Ports.Open {
			doors = append(doors, "ports")
		}
	}
	if enabled == nil {
		enabled = []string{}
	}
	if doors == nil {
		doors = []string{}
	}
	a.Bus.Emit(events.Event{Type: "middleware.ready", Actor: "engine", Message: fmt.Sprintf("modules: %s; doors: %s", orNone(enabled), orNone(doors)), Data: map[string]any{"modules": enabled, "doors": doors, "version": Version}})
	return nil
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// githubDeployer sends POST /sites/{domain}/deploy through the github
// module's runner when the module is enabled and the manifest has a
// deploy.github section: that path fetches the repository before it
// builds, which the manager's in-place build does not. Anything else is
// left to the manager.
func githubDeployer(a *app.App, set *moduleSet) api.Deployer {
	return func(domain string) (any, bool, error) {
		if set == nil || set.GitHub == nil || !set.Manager.Enabled(github.Key) {
			return nil, false, nil
		}
		dir := filepath.Join(a.Root, "sites", domain)
		mf, err := manifest.Load(dir)
		if err != nil || mf.Deploy == nil || mf.Deploy.GitHub == nil {
			return nil, false, nil
		}
		set.GitHub.Runner().Trigger(domain, dir, mf, "operator")
		return map[string]any{"ok": true, "via": github.Key}, true, nil
	}
}

// cloudflareDNS is the cloudflare module as the inbound section sees it.
type cloudflareDNS struct {
	set *moduleSet
}

func (d cloudflareDNS) Enabled() bool {
	return d.set != nil && d.set.Cloudflare != nil && d.set.Manager.Enabled(cloudflare.Key)
}

func (d cloudflareDNS) client() (*cloudflare.Client, error) {
	if !d.Enabled() {
		return nil, errors.New("cloudflare: not enabled")
	}
	return d.set.Cloudflare.Client()
}

// Describe lists the zones once, reads each zone that holds one of the
// domains once, and the tunnel's ingress once.
func (d cloudflareDNS) Describe(ctx context.Context, domains []string) (map[string]api.DNSInfo, error) {
	c, err := d.client()
	if err != nil {
		return nil, err
	}
	zones, err := c.Zones(ctx)
	if err != nil {
		return nil, err
	}
	byZone := map[string][]string{}
	for _, dom := range domains {
		z, err := cloudflare.MatchZone(zones, dom)
		if err != nil {
			continue
		}
		byZone[z.ID] = append(byZone[z.ID], strings.ToLower(dom))
	}
	out := map[string]api.DNSInfo{}
	for zoneID, names := range byZone {
		recs, err := c.DNSRecords(ctx, zoneID)
		if err != nil {
			return out, err
		}
		for _, name := range names {
			info := api.DNSInfo{}
			for _, r := range recs {
				if strings.EqualFold(r.Name, name) && (r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME") {
					info.DNS = r.Type + " " + r.Content
					break
				}
			}
			out[name] = info
		}
	}
	if c.TunnelID != "" {
		rules, err := c.TunnelIngress(ctx, "")
		if err != nil {
			return out, err
		}
		for _, r := range rules {
			name := strings.ToLower(r.Hostname)
			if info, ok := out[name]; ok {
				info.TunnelRoute = true
				out[name] = info
			}
		}
	}
	return out, nil
}

func (d cloudflareDNS) Publish(ctx context.Context, domain, door, ip string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	return c.Publish(ctx, domain, door, ip)
}

func (d cloudflareDNS) Unpublish(ctx context.Context, domain, ip string) error {
	c, err := d.client()
	if err != nil {
		return err
	}
	return c.Unpublish(ctx, domain, ip)
}

func (d cloudflareDNS) Covers(ctx context.Context, domain string) (bool, error) {
	c, err := d.client()
	if err != nil {
		return false, err
	}
	return c.Covers(ctx, domain)
}

var (
	_ api.DNSProvider = cloudflareDNS{}
	_ publish.DNS     = cloudflareDNS{}
)

// dns01For is the ACME dns-01 solver over the cloudflare module, asked
// each time whether the module is on and which zones it sees, so enabling
// it while the process runs is enough. The zone list is cached for a few
// minutes: Covers is asked once per name per order.
func dns01For(d cloudflareDNS) *acme.DNS01 {
	var mu sync.Mutex
	var zones []cloudflare.Zone
	var fetched time.Time
	return &acme.DNS01{
		Covers: func(domain string) bool {
			c, err := d.client()
			if err != nil {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			if time.Since(fetched) > 5*time.Minute {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				z, err := c.Zones(ctx)
				cancel()
				if err != nil {
					return false
				}
				zones, fetched = z, time.Now()
			}
			_, err = cloudflare.MatchZone(zones, domain)
			return err == nil
		},
		Set: func(ctx context.Context, fqdn, value string) (func(context.Context) error, error) {
			c, err := d.client()
			if err != nil {
				return nil, err
			}
			return c.SetTXT(ctx, fqdn, value)
		},
	}
}

// buildPublisher is the one place names are published. Its DNS is the
// cloudflare module while it is enabled, and nothing otherwise: names are
// then recorded as manual, with the reason, rather than silently skipped.
func buildPublisher(a *app.App, set *moduleSet) *publish.Publisher {
	if inboundHook == nil {
		return nil
	}
	dns := cloudflareDNS{set: set}
	p, err := publish.New(publish.Options{
		Root:  a.Root,
		Bus:   a.Bus,
		Doors: inboundHook,
		DNS: func() publish.DNS {
			if !dns.Enabled() {
				return nil
			}
			return dns
		},
		Hosts: a.Engine.Hosts,
	})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "publish.error", Actor: "engine", Level: "error", Message: "data/publish.json does not read, nothing is published: " + err.Error()})
		return nil
	}
	return p
}

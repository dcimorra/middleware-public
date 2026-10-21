package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/middleware-public/internal/acme"
	"github.com/dcimorra/middleware-public/internal/api"
	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/audit"
	"github.com/dcimorra/middleware-public/internal/autofix"
	"github.com/dcimorra/middleware-public/internal/chat"
	"github.com/dcimorra/middleware-public/internal/deps"
	"github.com/dcimorra/middleware-public/internal/domains"
	"github.com/dcimorra/middleware-public/internal/engine"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/firewall"
	"github.com/dcimorra/middleware-public/internal/helper"
	"github.com/dcimorra/middleware-public/internal/inbound"
	"github.com/dcimorra/middleware-public/internal/logrotate"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/modules"
	"github.com/dcimorra/middleware-public/internal/modules/agents"
	"github.com/dcimorra/middleware-public/internal/modules/cloudflare"
	"github.com/dcimorra/middleware-public/internal/modules/github"
	"github.com/dcimorra/middleware-public/internal/modules/vpn"
	"github.com/dcimorra/middleware-public/internal/pages"
	"github.com/dcimorra/middleware-public/internal/proxy"
	"github.com/dcimorra/middleware-public/internal/publish"
	"github.com/dcimorra/middleware-public/internal/push"
	"github.com/dcimorra/middleware-public/internal/repos"
	"github.com/dcimorra/middleware-public/internal/settings"
	"github.com/dcimorra/middleware-public/internal/shots"
	"github.com/dcimorra/middleware-public/internal/sitetoken"
	"github.com/dcimorra/middleware-public/internal/update"
)

// The order of the hooks is the shape of the process, so it is written
// here once and not left to the order in which the files' inits ran.
//
// Request chain, outermost first: inbound (the ACME challenge and the
// doors, before anything else sees the request), waf (refuse the abusive
// before anything is spent on them), security (probe paths, robots,
// browsers and countries: two lists are cheaper than a password), attack
// (the proof of work: outside the password, so a flood never reaches a
// key derivation without paying first), access
// (the door the operator closed), traffic (count whatever came out, error pages included), then
// the proxy. Everything after traffic wraps nothing and only runs: the
// api hangs from the proxy's panel slot, the chat, the tasks and the
// modules are services, the guard is a watchdog. The wire hook goes last
// because it reaches into every instance the others created.
const (
	orderInbound  = 10
	orderWAF      = 20
	orderSecurity = 25
	orderAttack   = 27
	orderAccess   = 30
	orderTraffic  = 40
	orderCache    = 45
	orderAPI      = 50
	orderChat     = 60
	orderTasks    = 70
	orderModules  = 80
	orderGuard    = 90
	orderWire     = 100
)

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "wire", Order: orderWire, Build: buildWire})
}

// The agent modules are kept so the wiring can re-apply them at start.
var (
	agentClaude *agents.Module
	agentGemini *agents.Module
)

// theVPN is the VPN module, for the inbound hook's VPN door.
var theVPN *vpn.VPN

// registerExtraModules adds what buildModules cannot build on its own:
// the VPN, which needs the helper and the secrets box, and the two agent
// modules, which replace providers in the registry the chat uses. It runs
// in the service and in the CLI alike, so `middleware modules` lists them.
func registerExtraModules(mgr *modules.Manager, root string, bus *events.Bus, dm *deps.Manager) error {
	box, err := modules.OpenBoxSecrets(root)
	if err != nil {
		return err
	}
	v, err := vpn.New(vpn.Options{Root: root, Helper: helper.Dial(""), Secrets: box, Bus: bus,
		// The address the ports door publishes, when the operator set
		// one; otherwise found the same way the publisher finds it.
		PublicIP: func(ctx context.Context) (string, error) {
			if st, err := inbound.LoadState(root); err == nil && st.PublicIP != "" {
				return st.PublicIP, nil
			}
			return publish.DetectPublicIP(ctx)
		}})
	if err != nil {
		return fmt.Errorf("vpn: %w", err)
	}
	mgr.Register(modules.VPNModule(v))
	theVPN = v
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
	shots     *shots.Shotter
	repos     *repos.Manager
	notifier  *push.Notifier
	auditor   *audit.Auditor
	fixer     *autofix.Fixer
	updater   *update.Updater
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
		// One deployment runner for the installation: the sites' own,
		// shared with the GitHub module, so two deploys of one site never
		// overlap whichever button or push started them.
		if mgr := srv.Sites(); mgr != nil && mgr.Deploys != nil && set.GitHub != nil {
			set.GitHub.UseRunner(mgr.Deploys)
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
	srv.RegisterWAF(api.WAFOptions{WAF: theWAF, Traffic: theTraffic, Attack: theAttack})
	srv.RegisterSecurity(api.SecurityOptions{Door: theSecurity, Country: edgeCountry})
	// Whichever of this and the inbound hook is built first makes it.
	if a.Proxy.Unrouted == nil {
		a.Proxy.Unrouted = proxy.NewUnrouted()
	}
	srv.RegisterUnrouted(a.Proxy.Unrouted)
	w.updater = buildUpdater(a)
	srv.RegisterUpdate(w.updater)
	w.shots = shots.New(shots.Options{
		Root:    a.Root,
		DataDir: dataDir(a),
		Bus:     a.Bus,
		Bare:    a.Proxy,
		Shootable: func(domain string) bool {
			sn, ok := a.Engine.Lookup(domain)
			if !ok || sn.Domain != domain || sn.Health == engine.HealthDown {
				return false
			}
			return sn.Mode == manifest.ModeStatic || sn.Mode == manifest.ModeUpstream || sn.Mode == manifest.ModeRun && sn.Alive
		},
		Domains: func() []string {
			var out []string
			for _, sn := range a.Engine.Sites() {
				if !manifest.IsProject(sn.Domain) {
					out = append(out, sn.Domain)
				}
			}
			return out
		},
	})
	srv.RegisterShots(w.shots)
	pages.SetHouseTheme(a.Settings.VisitorTheme)
	srv.OnSettingsSaved(func(st settings.Settings) { pages.SetHouseTheme(st.VisitorTheme) })
	srv.RegisterAgents(api.AgentsOptions{Registry: providers(), Chat: chatManager, ClaudeToken: func() string {
		if set == nil {
			return ""
		}
		cfg, err := set.Manager.Config(agents.ClaudeKey)
		if err != nil {
			return ""
		}
		return cfg["oauth_token"]
	}})
	if chatManager != nil {
		wireSiteDoor(a, srv, set)
	}
	if taskScheduler != nil {
		srv.RegisterTasks(api.TasksOptions{Scheduler: taskScheduler, Root: a.Root})
	}
	srv.RegisterGuard(api.GuardOptions{Guard: theGuard})
	// The repositories inside the sites, and the watcher that keeps the
	// marked ones pulled (internal/repos).
	rm := repos.New(repos.Options{DataDir: dataDir(a), Bus: a.Bus, Sites: func() []repos.Site {
		var out []repos.Site
		for _, sn := range a.Engine.Sites() {
			if sn.Manifest != nil {
				out = append(out, repos.Site{Domain: sn.Domain, Dir: sn.Dir})
			}
		}
		return out
	}})
	srv.RegisterRepos(rm)
	w.repos = rm
	// Web push to the operator's devices, and the notifier that decides
	// which events become one (internal/push).
	subject := "mailto:operator@localhost"
	if a.Settings.PanelDomain != "" {
		subject = "https://" + a.Settings.PanelDomain
	}
	ps, err := push.New(push.Options{DataDir: dataDir(a), Subject: subject})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "push.error", Actor: "engine", Level: "error", Message: "push notifications are off: " + err.Error()})
		ps = nil
	}
	srv.RegisterPush(ps)
	// npm audit over each site once a day: critical and new is an event
	// (and a push); never a fix (internal/audit).
	auditSites := func() []audit.Site {
		var out []audit.Site
		for _, sn := range a.Engine.Sites() {
			if sn.Manifest != nil {
				out = append(out, audit.Site{Domain: sn.Domain, Dir: sn.Dir})
			}
		}
		return out
	}
	w.auditor = audit.New(audit.Options{DataDir: dataDir(a), Bus: a.Bus, Sites: auditSites, NPM: func() string {
		// The house's own Node first (modules/node, kept up to date by
		// deps), then whatever npm the machine has.
		if w.set != nil && w.set.Deps != nil {
			if p := filepath.Join(w.set.Deps.Prefix(), "bin", "npm"); fileExists(p) {
				return p
			}
		}
		p, _ := exec.LookPath("npm")
		return p
	}})
	// Automatic repair of a site that went down (internal/autofix): off
	// unless Settings turns it on, with its brakes.
	if chatManager != nil {
		sm := srv.Sites()
		w.fixer = autofix.New(autofix.Options{
			DataDir: dataDir(a),
			Bus:     a.Bus,
			Settings: func() settings.Autofix {
				s, _ := settings.Load(a.Root)
				return s.Autofix
			},
			Targets: func() []autofix.Target {
				var out []autofix.Target
				for _, sn := range a.Engine.Sites() {
					if sn.Manifest == nil {
						continue
					}
					t := autofix.Target{Domain: sn.Domain, Mode: string(sn.Mode), Failures: sn.Failures, Error: sn.Error,
						OptOut: sn.Manifest.Autofix != nil && !*sn.Manifest.Autofix}
					for _, c := range sn.Checks {
						t.Checks = append(t.Checks, c.OK)
					}
					if sn.Manifest.Run != nil {
						t.Command = strings.Join(sn.Manifest.Run.Command, " ")
					}
					out = append(out, t)
				}
				return out
			},
			LogTail: func(domain string, n int) string {
				if sm == nil {
					return ""
				}
				s, _ := sm.Logs(domain, "", n)
				return s
			},
			Send: func(domain, prompt string) (string, error) {
				return chatManager.SendWith(domain, prompt, chat.SendOptions{})
			},
			Restart: func(domain string) error {
				if sn, ok := a.Engine.Lookup(domain); ok && sn.Mode != "run" {
					return nil // a static site has nothing to restart
				}
				return a.Engine.Restart(context.Background(), domain)
			},
		})
		srv.Handle("GET", api.Prefix+"/autofix", "The automatic repairs, oldest first (data/repairs.jsonl)", func(_ http.ResponseWriter, _ *http.Request) (any, error) {
			return w.fixer.Repairs(), nil
		})
	}
	srv.RegisterDomains(domains.New(domains.Options{DataDir: dataDir(a)}))
	if theCache != nil {
		srv.RegisterCache(theCache)
		srv.OnSettingsSaved(func(s settings.Settings) { theCache.SetBudget(s.Cache.Bytes()) })
	}
	srv.RegisterAudit(w.auditor, func(domain string) (audit.Site, bool) {
		sn, ok := a.Engine.Lookup(domain)
		return audit.Site{Domain: sn.Domain, Dir: sn.Dir}, ok && sn.Manifest != nil
	})
	if ps != nil {
		w.notifier = &push.Notifier{Bus: a.Bus, Sender: ps, OnError: func(err error) {
			a.Bus.Emit(events.Event{Type: "push.failed", Actor: "engine", Level: "warn", Message: err.Error()})
		}}
	}
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
	if w.shots != nil {
		go w.shots.Run(ctx)
	}
	if w.repos != nil {
		go w.repos.Run(ctx)
	}
	if w.notifier != nil {
		go w.notifier.Run(ctx)
	}
	if w.auditor != nil {
		go w.auditor.Run(ctx)
	}
	if w.fixer != nil {
		go w.fixer.Run(ctx)
	}
	if w.updater != nil {
		go w.updater.Run(ctx)
	}
	// The logs sites and tasks write, rotated at 10 MB with three kept.
	go logrotate.Run(ctx, []string{filepath.Join(dataDir(a), "logs", "*", "*.log")}, func(path string, err error) {
		if err != nil {
			a.Bus.Emit(events.Event{Type: "logs.rotate_failed", Actor: "engine", Level: "warn", Message: path + ": " + err.Error()})
			return
		}
		a.Bus.Emit(events.Event{Type: "logs.rotated", Actor: "engine", Message: path})
	})
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

// buildUpdater is the self-update: the helper installs, the supervisor
// restarts itself as the Restart button does.
func buildUpdater(a *app.App) *update.Updater {
	hc := helper.Dial("")
	return update.New(update.Options{
		Current: Version,
		Bus:     a.Bus,
		Auto: func() bool {
			s, err := settings.Load(a.Root)
			return err == nil && s.Update.Auto
		},
		Helper: hc.Available,
		Install: func(ctx context.Context, version string) error {
			_, err := hc.UpdateInstall(ctx, version)
			return err
		},
		Restart: func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) },
	})
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
		// The panel's name is published like a site's, so the operator
		// chooses its door (tunnel or 80/443) and neither is required.
		Hosts: func() map[string]string {
			hosts := a.Engine.Hosts()
			if d := a.Proxy.PanelDomain(); d != "" {
				if _, taken := hosts[d]; !taken {
					hosts[d] = publish.PanelOwner
				}
			}
			return hosts
		},
		VPNOnly: func(site string) bool {
			sn, ok := a.Engine.Lookup(site)
			// Only a site wholly inside the VPN is never published; one
			// with some paths in it is public everywhere else.
			return ok && sn.Manifest != nil && sn.Manifest.VPN.Whole()
		},
	})
	if err != nil {
		a.Bus.Emit(events.Event{Type: "publish.error", Actor: "engine", Level: "error", Message: "data/publish.json does not read, nothing is published: " + err.Error()})
		return nil
	}
	return p
}

// wireSiteDoor opens the site door: tokens derived from data/site-door.key,
// put in the environment of every site whose manifest has claude_api, and
// a socket of its own for sites in containers, where 127.0.0.1 is the
// container's and the loopback listener cannot be reached. The socket's
// folder is mounted, not the file: the socket is recreated at every start,
// and a container holding the file would keep the dead inode. Through that
// socket only the site door answers, never the panel: what calls it is
// what answers requests from the internet.
func wireSiteDoor(a *app.App, srv *api.Server, set *moduleSet) {
	tokens := sitetoken.New(dataDir(a))
	runDir := filepath.Join(dataDir(a), "run")
	sock := filepath.Join(runDir, "site.sock")
	// Where the loopback listener really is (MIDDLEWARE_LISTEN wins over
	// the settings), as an address a process can dial: the settings said
	// one port while a test instance listened on another, and the site's
	// calls went to whatever answered there.
	listen := app.ListenAddr(a.Settings.Listen)
	if host, port, err := net.SplitHostPort(listen); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		listen = net.JoinHostPort("127.0.0.1", port)
	}
	a.Engine.SetSiteEnv(func(domain string, m *manifest.Manifest) (map[string]string, []string) {
		env := map[string]string{}
		var mounts []string
		if m.ClaudeAPI != nil {
			if tok, err := tokens.Token(domain); err == nil {
				env["MIDDLEWARE_TOKEN"] = tok
				// The bare address, as in V1: the code calls
				// $MIDDLEWARE_API/api/site/… (or /api/v1/site/…).
				env["MIDDLEWARE_API"] = "http://" + listen
				if m.Run != nil && m.Run.Runtime == "docker" {
					env["MIDDLEWARE_API"] = "http://localhost"
					env["MIDDLEWARE_API_SOCKET"] = sock
					mounts = append(mounts, runDir)
				}
			}
		}
		if m.Claude && set != nil {
			// The house's subscription token, for a site that runs the CLI
			// itself: it reaches as far as the subscription does.
			if cfg, err := set.Manager.Config(agents.ClaudeKey); err == nil && cfg["oauth_token"] != "" {
				env["CLAUDE_CODE_OAUTH_TOKEN"] = cfg["oauth_token"]
			}
		}
		return env, mounts
	})
	srv.RegisterSiteDoor(api.SiteDoorOptions{
		Chat:   chatManager,
		Tokens: tokens,
		Bus:    a.Bus,
		SiteDir: func(domain string) string {
			if sn, ok := a.Engine.Lookup(domain); ok {
				return sn.Dir
			}
			return filepath.Join(a.Root, "sites", domain)
		},
		Available: func() bool { ok, _ := chatManager.ProviderInfo("claude"); return ok },
		API:       "http://" + listen,
		DefaultModel: func() string {
			if set == nil {
				return ""
			}
			cfg, _ := set.Manager.Config(agents.ClaudeKey)
			return cfg["model"]
		},
		Socket: sock,
		Domains: func() []string {
			var out []string
			for _, sn := range a.Engine.Sites() {
				out = append(out, sn.Domain)
			}
			return out
		},
		Models: func() []string {
			_, models := chatManager.ProviderInfo("claude")
			// Claude Code's aliases are names too.
			return append(models, "opus", "sonnet", "haiku")
		},
		Lookup: func(domain string) (*manifest.Manifest, bool) {
			sn, ok := a.Engine.Lookup(domain)
			if !ok || sn.Domain != domain {
				return nil, false
			}
			return sn.Manifest, sn.Manifest != nil
		},
		HouseDaily: func() float64 { return a.Settings.Agents.SiteDaily() },
		Spent:      chatManager.Ledger().Day,
	})
	ln, err := api.ListenSocket(sock)
	if err != nil {
		a.Bus.Emit(events.Event{Type: "sitedoor.socket_failed", Actor: "api", Level: "error", Message: err.Error()})
		return
	}
	_ = os.Chmod(sock, 0o666)
	h := srv.Panel()
	door := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := api.SiteDoorPath(r.URL.Path); !ok {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
	go func() { _ = (&http.Server{Handler: door, ReadHeaderTimeout: 15 * time.Second}).Serve(ln) }()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

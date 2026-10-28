// Package engine keeps what runs equal to what the manifests say. It is a
// reconcile loop, not an event handler: every tick it re-reads every
// manifest, resolves it, and acts on the difference with what is running.
// That is what makes a deployment "write a file": nothing has to be told.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/ports"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// Options configure an Engine.
type Options struct {
	SitesDir string
	DataDir  string
	Runtimes map[string]runtime.Runtime
	Ports    *ports.Allocator
	Bus      *events.Bus
	Tick     time.Duration
	// SiteEnv adds what the house gives a site's processes beyond its
	// manifest (the site door's token and address, folders to mount in a
	// container). It is part of the revision: opening or closing the door
	// restarts the site, or its process would keep a token the manifest
	// no longer grants.
	SiteEnv func(domain string, m *manifest.Manifest) (env map[string]string, mounts []string)
}

// SiteEnvFunc is Options.SiteEnv's type.
type SiteEnvFunc func(domain string, m *manifest.Manifest) (env map[string]string, mounts []string)

// Engine supervises every site under SitesDir.
type Engine struct {
	opts Options
	mu   sync.RWMutex
	// runCtx is Run's: what work the engine starts on someone's request
	// lives under, not under the request.
	runCtx context.Context
	sites  map[string]*Site
	// aliases maps every extra name to the site that owns it, rebuilt on
	// each tick from the manifests.
	aliases        map[string]string
	aliasConflicts map[string]string // said once, until resolved
	siteEnv        atomic.Pointer[SiteEnvFunc]
	wg             sync.WaitGroup
}

// Site is the engine's view of one folder.
type Site struct {
	Domain   string
	Dir      string
	Manifest *manifest.Manifest
	Err      string // manifest error, if any; the previous manifest stays in force
	Mode     manifest.Mode
	Spec     *manifest.Spec
	Instance runtime.Instance
	Workers  map[string]*worker

	health     healthState
	needsSince time.Time // first start waiting for its needs since
	// manifestSum is the last manifest kept in the history (history.go).
	manifestSum [32]byte
	handing     bool
	failures    int
	nextStart   time.Time
	lastErr     string
}

// Snapshot is what the proxy and the panel read: immutable, copied under
// the lock, so a handover in progress never shows a half state.
type Snapshot struct {
	Domain   string
	Dir      string
	Mode     manifest.Mode
	Manifest *manifest.Manifest
	Port     int
	Revision string
	Alive    bool
	Handing  bool
	Error    string
	Workers  map[string]WorkerSnapshot
	Health   Health
	Checks   []Check
	// Failures is how many starts in a row have failed.
	Failures int
	// Missing is what the site needs (manifest `needs`) that is not
	// available now, each with why.
	Missing []string
}

// WorkerSnapshot is a worker's state for readers.
type WorkerSnapshot struct {
	Port  int
	Alive bool
	Error string
}

// New builds an engine. It does not start anything: call Run.
func New(opts Options) *Engine {
	if opts.Tick == 0 {
		opts.Tick = 2 * time.Second
	}
	if opts.Runtimes == nil {
		opts.Runtimes = map[string]runtime.Runtime{"process": runtime.Process{}, "docker": runtime.Docker{House: opts.SitesDir}}
	}
	if opts.Ports == nil {
		opts.Ports = ports.New(0, 0)
	}
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	return &Engine{opts: opts, sites: map[string]*Site{}, aliases: map[string]string{}}
}

// Run adopts what is already running, then reconciles until ctx ends.
// Sites are left running on exit on purpose: they outlive the supervisor.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	e.runCtx = ctx
	e.mu.Unlock()
	e.adoptAll(ctx)
	e.reapContainers(ctx)
	e.tick(ctx)
	t := time.NewTicker(e.opts.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.wg.Wait()
			return
		case <-t.C:
			e.tick(ctx)
		}
	}
}

// Lookup returns the snapshot for a domain or one of its aliases.
func (e *Engine) Lookup(domain string) (Snapshot, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.sites[domain]
	if !ok {
		if owner, isAlias := e.aliases[domain]; isAlias {
			s, ok = e.sites[owner]
		}
	}
	if !ok {
		return Snapshot{}, false
	}
	return e.snapshot(s), true
}

// Sites returns every snapshot, sorted by domain.
func (e *Engine) Sites() []Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Snapshot, 0, len(e.sites))
	for _, s := range e.sites {
		out = append(out, e.snapshot(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// Restart forces a handover of a running site even if its revision has not
// changed: after a binary upgrade, or to pick up a changed .env that the
// site reads itself.
func (e *Engine) Restart(ctx context.Context, domain string) error {
	e.mu.Lock()
	s, ok := e.sites[domain]
	if !ok || s.Spec == nil {
		e.mu.Unlock()
		return fmt.Errorf("%s: not a running site", domain)
	}
	if s.handing {
		e.mu.Unlock()
		return fmt.Errorf("%s: a handover is already in progress", domain)
	}
	s.handing = true
	spec := s.Spec
	// The engine's own context, not the caller's: the restart was asked
	// by a request, which ends as soon as it answers, and a handover under
	// it was cancelled halfway ("docker run: context canceled", twice in a
	// row from `middleware site restart`).
	run := e.runCtx
	e.mu.Unlock()
	if run == nil {
		run = context.Background()
	}
	e.wg.Add(1)
	go e.handover(run, s, spec, "operator")
	return nil
}

func (e *Engine) snapshot(s *Site) Snapshot {
	sn := Snapshot{Domain: s.Domain, Dir: s.Dir, Mode: s.Mode, Manifest: s.Manifest, Handing: s.handing, Error: s.Err,
		Health: s.health.status, Checks: append([]Check(nil), s.health.checks...), Missing: e.missingLocked(s)}
	if sn.Error == "" {
		sn.Error = s.lastErr
		sn.Failures = s.failures
	}
	if s.Instance != nil {
		sn.Port = s.Instance.Port()
		sn.Revision = s.Instance.Revision()
		sn.Alive = true
	}
	if len(s.Workers) > 0 {
		sn.Workers = map[string]WorkerSnapshot{}
		for name, w := range s.Workers {
			sn.Workers[name] = WorkerSnapshot{Port: w.port, Alive: w.inst != nil, Error: w.lastErr}
		}
	}
	return sn
}

func (e *Engine) emit(typ, domain, level, msg string, data map[string]any) {
	e.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: "engine", Level: level, Message: msg, Data: data})
}

// SetSiteEnv installs Options.SiteEnv after the engine was built: the
// wiring that knows the tokens and the modules runs after the app made
// the engine. Read on every reconcile.
func (e *Engine) SetSiteEnv(f SiteEnvFunc) {
	e.siteEnv.Store(&f)
}

// tick is one reconcile pass.
func (e *Engine) tick(ctx context.Context) {
	entries, err := os.ReadDir(e.opts.SitesDir)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, ent := range entries {
		if !ent.IsDir() && ent.Type()&os.ModeSymlink == 0 {
			continue
		}
		domain := ent.Name()
		if !manifest.ValidName(domain) {
			continue
		}
		dir := filepath.Join(e.opts.SitesDir, domain)
		if !fsx.Exists(filepath.Join(dir, manifest.FileName)) {
			// Unmanaged: listed by the panel, never served. See docs/manifest.md.
			continue
		}
		seen[domain] = true
		e.reconcile(ctx, domain, dir)
	}
	// Folders that disappeared: stop what they ran.
	e.mu.Lock()
	for domain, s := range e.sites {
		if seen[domain] {
			e.healthDue(ctx, s)
		}
	}
	e.rebuildAliases()
	for domain, s := range e.sites {
		if seen[domain] {
			continue
		}
		delete(e.sites, domain)
		if s.Instance != nil {
			inst := s.Instance
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				_ = inst.Stop(ctx, 8*time.Second)
				e.opts.Ports.Release(inst.Port())
			}()
		}
		// Stopped outside the lock, as the instance is: under it, every
		// request to every site waited for these graces. A busy one finds
		// its site gone when its start ends and stops what it started.
		for _, w := range s.Workers {
			if !w.busy {
				e.retireWorkerLocked(ctx, domain, w, 8*time.Second)
			}
		}
		e.clearState(domain)
		e.emit("site.removed", domain, "", "folder removed, instance stopped", nil)
	}
	e.mu.Unlock()
}

func (e *Engine) reconcile(ctx context.Context, domain, dir string) {
	m, err := manifest.Load(dir)
	e.mu.Lock()
	s, ok := e.sites[domain]
	if !ok {
		s = &Site{Domain: domain, Dir: dir, Workers: map[string]*worker{}}
		e.sites[domain] = s
		e.emit("site.found", domain, "", "", nil)
	}
	if err != nil {
		if s.Err != err.Error() {
			s.Err = err.Error()
			e.emit("manifest.invalid", domain, "error", err.Error(), nil)
		}
		e.mu.Unlock()
		return // the previous manifest, if any, stays in force
	}
	if s.Err != "" {
		s.Err = ""
		e.emit("manifest.valid", domain, "", "manifest readable again", nil)
	}
	if data, err := os.ReadFile(filepath.Join(dir, manifest.FileName)); err == nil {
		e.recordManifest(s, data)
	}
	s.Manifest = m
	s.Mode = m.ModeFor(domain)
	e.mu.Unlock()

	e.reconcileWorkers(ctx, s, m)

	if s.Mode != manifest.ModeRun {
		e.mu.Lock()
		inst, spec := s.Instance, s.Spec
		s.Instance, s.Spec = nil, nil
		e.mu.Unlock()
		if inst != nil {
			e.emit("instance.stopping", domain, "", "manifest says "+string(s.Mode), nil)
			// The drain comes from the spec the instance was born from: the
			// new manifest may have no run section at all (a site turned
			// static or upstream), and reading m.Run here was a nil
			// dereference that took the supervisor down.
			drain := 8 * time.Second
			if spec != nil {
				drain = spec.Drain.Or(drain)
			}
			_ = inst.Stop(ctx, drain)
			e.opts.Ports.Release(inst.Port())
			e.clearState(domain)
		}
		return
	}

	spec, err := manifest.Resolve(domain, dir, m)
	if err != nil {
		e.mu.Lock()
		if s.lastErr != err.Error() {
			s.lastErr = err.Error()
			e.emit("manifest.invalid", domain, "error", err.Error(), nil)
		}
		e.mu.Unlock()
		return
	}
	e.addSiteEnv(domain, m, spec)
	if spec.Port != 0 {
		// Held for as long as it is declared, also while the site is down,
		// so the allocator never hands it to another site in between.
		e.opts.Ports.Reserve(spec.Port, domain)
	}

	e.mu.Lock()
	if s.handing {
		e.mu.Unlock()
		return
	}
	// A dead instance: forget it, with backoff so a crash loop does not
	// burn the port range and the log file.
	var exited runtime.Instance
	if s.Instance != nil {
		select {
		case err := <-s.Instance.Done():
			exited = s.Instance
			s.Instance = nil
			s.failures++
			// An adopted instance has no Spec until a reconcile gives it
			// one, and one that exits before that took the supervisor down
			// here with a nil dereference. The spec just resolved is the
			// policy the restart will run under anyway.
			policy := spec
			if s.Spec != nil {
				policy = s.Spec
			}
			wait := backoff(s.failures, policy.Restart.MaxBackoff.Or(60*time.Second))
			s.nextStart = time.Now().Add(wait)
			e.emit("instance.exited", domain, "error", fmt.Sprintf("%v; restart in %s", err, wait), map[string]any{"failures": s.failures})
			e.clearState(domain)
		default:
		}
	}
	if exited != nil {
		e.mu.Unlock()
		// The leader is gone, not necessarily its group: what `sh -c "a &
		// b"` left behind went on holding the port, and every restart the
		// backoff made added another set. Stopped before the port goes
		// back to the allocator, and outside the lock, since a container
		// takes a docker call or two. Nothing starts this tick anyway: the
		// backoff is a second at least.
		_ = exited.Stop(ctx, 2*time.Second)
		e.opts.Ports.Release(exited.Port())
		return
	}
	defer e.mu.Unlock()
	if s.Instance != nil && s.Instance.Revision() == spec.Revision {
		s.Spec = spec
		return
	}
	if time.Now().Before(s.nextStart) {
		return
	}
	if s.Instance == nil && e.waitForNeeds(s) {
		return
	}
	s.handing = true
	s.Spec = spec
	actor := "engine"
	e.wg.Add(1)
	go e.handover(ctx, s, spec, actor)
}

// handover starts the new generation, waits for it, swaps, and drains the
// old one. It runs outside the lock: a slow health check must not stall
// the reconcile of every other site.
func (e *Engine) handover(ctx context.Context, s *Site, spec *manifest.Spec, actor string) {
	defer e.wg.Done()
	domain := s.Domain
	rt, ok := e.opts.Runtimes[spec.Runtime]
	if !ok {
		e.fail(s, fmt.Errorf("runtime %q is not available", spec.Runtime))
		return
	}
	var port int
	if spec.Port != 0 {
		// A written port holds one generation at a time, so there is no
		// handover: the old one stops first and the site is down for as
		// long as the new one takes to start. That is the price of a port
		// someone outside the house has written down.
		port = spec.Port
		e.opts.Ports.Reserve(port, domain)
		e.mu.Lock()
		old := s.Instance
		s.Instance = nil
		e.mu.Unlock()
		if old != nil {
			e.emit("instance.stopping", domain, "", "written port: stopping before starting the new generation", nil)
			_ = old.Stop(ctx, 5*time.Second)
			e.clearState(domain)
		}
	} else {
		p, err := e.opts.Ports.Claim(domain)
		if err != nil {
			e.fail(s, err)
			return
		}
		port = p
	}
	logFile := filepath.Join(e.opts.DataDir, "logs", domain, "site.log")
	home := filepath.Join(e.opts.DataDir, "homes", domain)
	e.opts.Bus.Emit(events.Event{Type: "instance.starting", Subject: domain, Actor: actor, Data: map[string]any{"port": port, "revision": spec.Revision}})
	inst, err := rt.Start(ctx, spec, port, logFile, home)
	if err != nil {
		e.opts.Ports.Release(port)
		e.fail(s, err)
		return
	}
	timeout := spec.Health.Timeout.Or(60 * time.Second)
	if err := runtime.WaitReady(ctx, inst, spec.Health, timeout); err != nil {
		_ = inst.Stop(ctx, 2*time.Second)
		e.opts.Ports.Release(port)
		e.fail(s, fmt.Errorf("%w (last lines of the log: %s)", err, tail(logFile, 5)))
		return
	}
	e.mu.Lock()
	if e.sites[domain] != s || s.Mode != manifest.ModeRun {
		// The folder went, or stopped saying run, while this generation
		// started. The tick already stopped what the site ran and cleared
		// its state; installed on the detached site it kept running, on
		// its port, with nothing left that would ever stop it.
		s.handing = false
		e.mu.Unlock()
		_ = inst.Stop(ctx, 5*time.Second)
		e.opts.Ports.Release(port)
		e.emit("instance.discarded", domain, "", "the site was removed or stopped while its new generation started; stopped it", map[string]any{"port": port, "revision": spec.Revision})
		return
	}
	old := s.Instance
	s.Instance = inst
	s.failures = 0
	s.lastErr = ""
	s.handing = false
	// Under the lock, so a tick that removes the site in between cannot
	// clear the state before it is written and leave it behind.
	e.saveState(inst.State())
	e.mu.Unlock()
	if old == nil {
		e.opts.Bus.Emit(events.Event{Type: "instance.started", Subject: domain, Actor: actor, Data: map[string]any{"port": port, "revision": spec.Revision}})
		return
	}
	e.opts.Bus.Emit(events.Event{Type: "handover.switched", Subject: domain, Actor: actor, Data: map[string]any{"from": old.Port(), "to": port, "revision": spec.Revision}})
	drain := spec.Drain.Or(8 * time.Second)
	time.Sleep(drain)
	_ = old.Stop(ctx, 5*time.Second)
	e.opts.Ports.Release(old.Port())
	e.opts.Bus.Emit(events.Event{Type: "handover.done", Subject: domain, Actor: actor, Data: map[string]any{"drained": drain.String()}})
}

func (e *Engine) fail(s *Site, err error) {
	e.mu.Lock()
	s.handing = false
	s.failures++
	wait := backoff(s.failures, 60*time.Second)
	if s.Spec != nil {
		wait = backoff(s.failures, s.Spec.Restart.MaxBackoff.Or(60*time.Second))
	}
	s.nextStart = time.Now().Add(wait)
	s.lastErr = err.Error()
	e.mu.Unlock()
	e.emit("instance.failed", s.Domain, "error", fmt.Sprintf("%v; retry in %s", err, wait), map[string]any{"failures": s.failures})
}

// backoff doubles from one second up to max.
func backoff(failures int, max time.Duration) time.Duration {
	d := time.Second
	for i := 1; i < failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// statePath is one file per instance; a worker's key ("a.example/api")
// becomes "a.example@api.json" so every state lives in one flat folder.
func (e *Engine) statePath(key string) string {
	return runtime.StatePath(e.opts.DataDir, key)
}

func (e *Engine) saveState(st runtime.State) {
	_ = fsx.WriteJSON(e.statePath(st.Domain), st, 0o644)
}

func (e *Engine) clearState(domain string) {
	_ = os.Remove(e.statePath(domain))
}

// adoptAll re-attaches to instances a previous supervisor left running.
func (e *Engine) adoptAll(ctx context.Context) {
	entries, err := os.ReadDir(filepath.Join(e.opts.DataDir, "state"))
	if err != nil {
		return
	}
	for _, ent := range entries {
		var st runtime.State
		if err := fsx.ReadJSON(filepath.Join(e.opts.DataDir, "state", ent.Name()), &st); err != nil {
			continue
		}
		rt, ok := e.opts.Runtimes[st.Runtime]
		if !ok {
			continue
		}
		inst, err := rt.Adopt(ctx, st)
		if err != nil {
			e.clearState(st.Domain)
			e.emit("instance.gone", st.Domain, "warn", "recorded instance is no longer running", map[string]any{"pid": st.PID, "port": st.Port})
			continue
		}
		// Written again: the adopted state may know more than the file did
		// (a container's id, recorded since the guard measures containers
		// by it; a process's start tick and boot, recorded since a clock
		// step made every adoption fail), and a state left as it was would
		// never learn it.
		if fresh := inst.State(); fresh.ContainerID != st.ContainerID || fresh.StartTicks != st.StartTicks || fresh.BootID != st.BootID {
			e.saveState(fresh)
		}
		if strings.Contains(st.Domain, "/") {
			// A worker need not listen on anything (a queue consumer, a
			// watcher), so it is not dialled: the runtime has already
			// checked the pid is the one that was started.
			e.opts.Ports.Reserve(st.Port, st.Domain)
			e.adoptWorker(st, inst)
			e.emit("worker.adopted", st.Domain, "", "still running from a previous supervisor", map[string]any{"pid": st.PID, "port": st.Port})
			continue
		}
		if err := runtime.WaitReady(ctx, inst, manifest.Health{}, 3*time.Second); err != nil {
			// The runtime has confirmed it is the process that was started,
			// so alive but not listening is ours and broken (hung, or it
			// closed its socket). It used to be forgotten and left running:
			// the next tick started a second copy, and the first one held
			// whatever it held for good, outside every state file.
			_ = inst.Stop(ctx, 5*time.Second)
			e.clearState(st.Domain)
			e.emit("instance.stopped", st.Domain, "warn", "still running from a previous supervisor but not listening; stopped", map[string]any{"pid": st.PID, "port": st.Port})
			continue
		}
		e.opts.Ports.Reserve(st.Port, st.Domain)
		e.mu.Lock()
		s, ok := e.sites[st.Domain]
		if !ok {
			s = &Site{Domain: st.Domain, Dir: filepath.Join(e.opts.SitesDir, st.Domain), Workers: map[string]*worker{}}
			e.sites[st.Domain] = s
		}
		s.Instance, s.Mode = inst, manifest.ModeRun
		e.mu.Unlock()
		e.emit("instance.adopted", st.Domain, "", "still running from a previous supervisor; its environment is the one it was born with", map[string]any{"pid": st.PID, "port": st.Port, "revision": st.Revision})
	}
}

// tail returns the last n lines of a file for error messages.
func tail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := splitLines(string(data))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := ""
	for _, l := range lines {
		out += l + " | "
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Hosts maps every name the house answers to (see manifest.Hosts) to the
// site that owns it: what publishing, certificates and the doors ask. A
// name refused as an alias conflict is left out, like the router does.
func (e *Engine) Hosts() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string]string{}
	for domain, s := range e.sites {
		if s.Manifest == nil {
			continue
		}
		taken := func(h string) bool {
			if owner, ok := e.aliases[h]; ok && owner != domain {
				return true
			}
			if _, conflict := e.aliasConflicts[h]; conflict {
				return true
			}
			_, isSite := e.sites[h]
			return isSite
		}
		for _, h := range s.Manifest.Hosts(domain) {
			if h != domain {
				if taken(h) {
					continue
				}
				// The www an extra domain brings goes with that domain:
				// not this site's when the domain itself is not (another
				// site's name, or claimed twice).
				if base, ok := strings.CutPrefix(h, "www."); ok && base != domain && taken(base) {
					continue
				}
			}
			out[h] = domain
		}
	}
	return out
}

// rebuildAliases maps each alias to its site. A name that is already a
// site, or that two sites both claim, goes to nobody: serving it from
// whichever folder sorts first would be a choice nobody made, so it is
// refused and said, once, until the manifests stop disagreeing. Called
// under e.mu.
func (e *Engine) rebuildAliases() {
	claims := map[string][]string{}
	for domain, s := range e.sites {
		if s.Manifest == nil {
			continue
		}
		for _, a := range s.Manifest.Aliases {
			claims[a] = append(claims[a], domain)
		}
	}
	next := map[string]string{}
	for alias, owners := range claims {
		sort.Strings(owners)
		_, isSite := e.sites[alias]
		if len(owners) == 1 && !isSite {
			next[alias] = owners[0]
			continue
		}
		if _, was := e.aliases[alias]; was || e.aliasConflicts[alias] == "" {
			why := "claimed by " + strings.Join(owners, ", ")
			if isSite {
				why = "it is a site of its own"
			}
			e.emit("alias.conflict", owners[0], "error", alias+" is not served as an alias: "+why, nil)
		}
		if e.aliasConflicts == nil {
			e.aliasConflicts = map[string]string{}
		}
		e.aliasConflicts[alias] = strings.Join(owners, ",")
	}
	for alias := range e.aliasConflicts {
		if _, still := claims[alias]; !still {
			delete(e.aliasConflicts, alias)
		}
	}
	e.aliases = next
}

// reapContainers removes this house's containers that no state file
// claimed, once, after adoption: what a supervisor that died mid-start left.
func (e *Engine) reapContainers(ctx context.Context) {
	d, ok := e.opts.Runtimes["docker"].(runtime.Docker)
	if !ok || d.House == "" {
		return
	}
	keep := map[string]bool{}
	e.mu.RLock()
	for _, s := range e.sites {
		if s.Instance != nil {
			if c := s.Instance.State().Container; c != "" {
				keep[c] = true
			}
		}
		// A worker's container too: one reaped here is a database the
		// site needs, taken away by the supervisor that should keep it.
		for _, w := range s.Workers {
			if w.inst != nil {
				if c := w.inst.State().Container; c != "" {
					keep[c] = true
				}
			}
		}
	}
	e.mu.RUnlock()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, name := range d.Reap(rctx, keep) {
		e.emit("container.reaped", "", "warn", name+": left by an earlier supervisor and claimed by no site", nil)
	}
}

// addSiteEnv applies Options.SiteEnv to a spec and rehashes it.
func (e *Engine) addSiteEnv(domain string, m *manifest.Manifest, spec *manifest.Spec) {
	f := SiteEnvFunc(e.opts.SiteEnv)
	if p := e.siteEnv.Load(); p != nil {
		f = *p
	}
	if f == nil {
		return
	}
	env, mounts := f(domain, m)
	if len(env) == 0 && len(mounts) == 0 {
		return
	}
	for k, v := range env {
		spec.Env[k] = v
	}
	spec.Mounts = append(spec.Mounts, mounts...)
	spec.Rehash()
}

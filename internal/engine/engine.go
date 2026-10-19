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
	"sync"
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
}

// Engine supervises every site under SitesDir.
type Engine struct {
	opts  Options
	mu    sync.RWMutex
	sites map[string]*Site
	wg    sync.WaitGroup
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

	handing   bool
	failures  int
	nextStart time.Time
	lastErr   string
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
		opts.Runtimes = map[string]runtime.Runtime{"process": runtime.Process{}, "docker": runtime.Docker{}}
	}
	if opts.Ports == nil {
		opts.Ports = ports.New(0, 0)
	}
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	return &Engine{opts: opts, sites: map[string]*Site{}}
}

// Run adopts what is already running, then reconciles until ctx ends.
// Sites are left running on exit on purpose: they outlive the supervisor.
func (e *Engine) Run(ctx context.Context) {
	e.adoptAll(ctx)
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

// Lookup returns the snapshot for a domain.
func (e *Engine) Lookup(domain string) (Snapshot, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.sites[domain]
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
	e.mu.Unlock()
	e.wg.Add(1)
	go e.handover(ctx, s, spec, "operator")
	return nil
}

func (e *Engine) snapshot(s *Site) Snapshot {
	sn := Snapshot{Domain: s.Domain, Dir: s.Dir, Mode: s.Mode, Manifest: s.Manifest, Handing: s.handing, Error: s.Err}
	if sn.Error == "" {
		sn.Error = s.lastErr
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
		if !manifest.ValidDomain(domain) {
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
		for _, w := range s.Workers {
			if w.inst != nil {
				_ = w.inst.Stop(ctx, 8*time.Second)
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
	s.Manifest = m
	s.Mode = m.Mode()
	e.mu.Unlock()

	e.reconcileWorkers(ctx, s, m)

	if s.Mode != manifest.ModeRun {
		e.mu.Lock()
		inst := s.Instance
		s.Instance, s.Spec = nil, nil
		e.mu.Unlock()
		if inst != nil {
			e.emit("instance.stopping", domain, "", "manifest says "+string(s.Mode), nil)
			_ = inst.Stop(ctx, m.Run.Drain.Or(8*time.Second))
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

	e.mu.Lock()
	defer e.mu.Unlock()
	if s.handing {
		return
	}
	// A dead instance: forget it, with backoff so a crash loop does not
	// burn the port range and the log file.
	if s.Instance != nil {
		select {
		case err := <-s.Instance.Done():
			e.opts.Ports.Release(s.Instance.Port())
			s.Instance = nil
			s.failures++
			wait := backoff(s.failures, s.Spec.Restart.MaxBackoff.Or(60*time.Second))
			s.nextStart = time.Now().Add(wait)
			e.emit("instance.exited", domain, "error", fmt.Sprintf("%v; restart in %s", err, wait), map[string]any{"failures": s.failures})
			e.clearState(domain)
		default:
		}
	}
	if s.Instance != nil && s.Instance.Revision() == spec.Revision {
		s.Spec = spec
		return
	}
	if time.Now().Before(s.nextStart) {
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
	port, err := e.opts.Ports.Claim(domain)
	if err != nil {
		e.fail(s, err)
		return
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
	old := s.Instance
	s.Instance = inst
	s.failures = 0
	s.lastErr = ""
	s.handing = false
	e.mu.Unlock()
	e.saveState(inst.State())
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

func (e *Engine) statePath(domain string) string {
	return filepath.Join(e.opts.DataDir, "state", domain+".json")
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
		if err := runtime.WaitReady(ctx, inst, manifest.Health{}, 3*time.Second); err != nil {
			// Alive but not listening: a zombie or something else on that pid.
			e.clearState(st.Domain)
			continue
		}
		e.opts.Ports.Reserve(st.Port, st.Domain)
		e.mu.Lock()
		e.sites[st.Domain] = &Site{Domain: st.Domain, Dir: filepath.Join(e.opts.SitesDir, st.Domain), Instance: inst, Mode: manifest.ModeRun, Workers: map[string]*worker{}}
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

package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// worker is an always-on process that belongs to a site. It restarts; it is
// never handed over, because a written port cannot hold two generations,
// and an assigned one is only known to the site through $PORT at start.
//
// Workers are saved to data/state like instances and re-adopted the same
// way. Before that, every supervisor restart started a second copy of each
// worker next to the one still running, and a written port made the copy
// fail to bind forever.
type worker struct {
	name      string
	port      int
	assigned  bool // port came from the allocator, so it goes back to it
	revision  string
	inst      runtime.Instance
	failures  int
	nextStart time.Time
	lastErr   string
}

// workerKey is a worker's name in state files and the port allocator.
func workerKey(domain, name string) string { return domain + "/" + name }

func (e *Engine) stopWorker(ctx context.Context, domain string, w *worker) {
	if w.inst != nil {
		_ = w.inst.Stop(ctx, 5*time.Second)
		w.inst = nil
	}
	if w.assigned && w.port != 0 {
		e.opts.Ports.Release(w.port)
		w.port, w.assigned = 0, false
	}
	e.clearState(workerKey(domain, w.name))
}

func (e *Engine) reconcileWorkers(ctx context.Context, s *Site, m *manifest.Manifest) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stopAll := s.Mode == manifest.ModeDisabled
	for name, w := range s.Workers {
		def, still := m.Workers[name]
		if still && !stopAll && !def.Disabled {
			continue
		}
		e.stopWorker(ctx, s.Domain, w)
		delete(s.Workers, name)
		e.emit("worker.stopped", s.Domain, "", name, nil)
	}
	if stopAll {
		return
	}
	for name, def := range m.Workers {
		if def.Disabled {
			continue
		}
		spec := workerSpec(s.Domain, s.Dir, name, def)
		e.addSiteEnv(s.Domain, m, spec)
		w, ok := s.Workers[name]
		if !ok {
			w = &worker{name: name}
			s.Workers[name] = w
		}
		if def.Port != 0 {
			// A written port is held for as long as it is declared, also
			// while the worker is down, so the allocator never hands it to
			// a site in between.
			e.opts.Ports.Reserve(def.Port, workerKey(s.Domain, name))
			w.assigned = false
		}
		if w.inst != nil {
			select {
			case err := <-w.inst.Done():
				w.inst = nil
				w.failures++
				wait := backoff(w.failures, 60*time.Second)
				w.nextStart = time.Now().Add(wait)
				w.lastErr = fmt.Sprintf("%v; restart in %s", err, wait)
				e.clearState(workerKey(s.Domain, name))
				e.emit("worker.exited", s.Domain, "error", name+": "+w.lastErr, nil)
			default:
			}
		}
		if w.inst != nil && w.revision != spec.Revision {
			e.stopWorker(ctx, s.Domain, w)
			e.emit("worker.changed", s.Domain, "", name+": definition changed, restarting", nil)
		}
		if w.inst != nil || time.Now().Before(w.nextStart) {
			continue
		}
		port := def.Port
		if port == 0 {
			p, err := e.opts.Ports.Claim(workerKey(s.Domain, name))
			if err != nil {
				w.lastErr = err.Error()
				continue
			}
			port, w.assigned = p, true
		}
		w.port = port
		logFile := filepath.Join(e.opts.DataDir, "logs", s.Domain, "worker-"+name+".log")
		home := filepath.Join(e.opts.DataDir, "homes", s.Domain)
		inst, err := runtime.Process{}.Start(ctx, spec, port, logFile, home)
		if err != nil {
			if w.assigned {
				e.opts.Ports.Release(port)
				w.port, w.assigned = 0, false
			}
			w.failures++
			w.nextStart = time.Now().Add(backoff(w.failures, 60*time.Second))
			w.lastErr = err.Error()
			e.emit("worker.failed", s.Domain, "error", name+": "+err.Error(), nil)
			continue
		}
		w.inst = inst
		w.revision = spec.Revision
		w.failures = 0
		w.lastErr = ""
		e.saveState(inst.State())
		e.emit("worker.started", s.Domain, "", name, map[string]any{"port": port})
	}
}

// adoptWorker attaches a worker a previous supervisor left running.
func (e *Engine) adoptWorker(st runtime.State, inst runtime.Instance) {
	domain, name, _ := strings.Cut(st.Domain, "/")
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sites[domain]
	if !ok {
		s = &Site{Domain: domain, Dir: filepath.Join(e.opts.SitesDir, domain), Workers: map[string]*worker{}}
		e.sites[domain] = s
	}
	// Whether the port was assigned is not in the state: it is taken as
	// assigned here and corrected at the first reconcile, which knows from
	// the manifest whether the port is a written one.
	s.Workers[name] = &worker{name: name, port: st.Port, revision: st.Revision, inst: inst, assigned: true}
}

func workerSpec(domain, dir, name string, def *manifest.Worker) *manifest.Spec {
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{
		Command: def.Command, Cwd: def.Cwd, Env: def.Env, EnvFiles: def.EnvFiles,
		EnvFromFile: def.EnvFromFile, Port: def.Port,
	}}
	spec, err := manifest.Resolve(domain, dir, m)
	if err != nil {
		return &manifest.Spec{Domain: workerKey(domain, name), SiteDir: dir, Cwd: dir, Command: def.Command}
	}
	spec.Domain = workerKey(domain, name)
	return spec
}

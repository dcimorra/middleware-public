package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
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
	// restarting is set while RestartWorker stops the old process outside
	// the lock; without it a tick in between would start the new one
	// while the old still holds a written port, and the new one would
	// fail to bind and back off for a minute.
	restarting bool
	// busy is set while the engine starts or stops this worker outside the
	// lock. Both used to run under it, and every request asks the engine
	// for its site under the same lock: a docker build, an nvm install or
	// a five-second grace stalled the traffic of every site on the
	// machine. A tick that finds it set leaves the worker alone, so the
	// work is never done twice and a written port is never started on
	// while the old process still holds it.
	busy bool
}

// workerKey is a worker's name in state files and the port allocator.
func workerKey(domain, name string) string { return domain + "/" + name }

// retireWorkerLocked stops what w runs, outside the lock, and gives its
// port back and clears its state once it is gone. Called under e.mu; w is
// busy until then. The stop does not take the engine's cancellation: a
// shutdown that cut the grace short killed workers mid-write.
func (e *Engine) retireWorkerLocked(ctx context.Context, domain string, w *worker, grace time.Duration) {
	inst := w.inst
	w.inst, w.busy = nil, true
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if inst != nil {
			// Also for one that has exited: its group may not have.
			_ = inst.Stop(context.WithoutCancel(ctx), grace)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if w.assigned && w.port != 0 {
			e.opts.Ports.Release(w.port)
			w.port, w.assigned = 0, false
		}
		e.clearState(workerKey(domain, w.name))
		w.busy = false
	}()
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
		delete(s.Workers, name)
		e.emit("worker.stopped", s.Domain, "", name, nil)
		if !w.busy {
			e.retireWorkerLocked(ctx, s.Domain, w, 5*time.Second)
		}
		// A busy one is being started or stopped: a start finds it gone
		// from the site and stops what it started, a stop just ends.
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
		}
		if w.busy {
			continue
		}
		if def.Port != 0 {
			w.assigned = false
		}
		if w.inst != nil {
			select {
			case err := <-w.inst.Done():
				w.failures++
				wait := backoff(w.failures, 60*time.Second)
				w.nextStart = time.Now().Add(wait)
				w.lastErr = fmt.Sprintf("%v; restart in %s", err, wait)
				e.emit("worker.exited", s.Domain, "error", name+": "+w.lastErr, nil)
				e.retireWorkerLocked(ctx, s.Domain, w, 2*time.Second)
				continue
			default:
			}
		}
		if w.inst != nil && w.revision != spec.Revision {
			e.emit("worker.changed", s.Domain, "", name+": definition changed, restarting", nil)
			e.retireWorkerLocked(ctx, s.Domain, w, 5*time.Second)
			continue // the next tick starts it, once the old one is gone
		}
		if w.inst != nil || w.restarting || time.Now().Before(w.nextStart) {
			continue
		}
		// The runtime first: a port claimed for a worker that cannot start
		// was claimed again at every tick and never given back.
		rt, known := e.opts.Runtimes[spec.Runtime]
		if !known {
			w.lastErr = "runtime " + spec.Runtime + " is not available on this node"
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
		w.busy = true
		e.wg.Add(1)
		go e.startWorker(ctx, s, w, rt, spec, port, logFile, home)
	}
}

// startWorker runs a worker's start outside the lock (see worker.busy) and
// installs the result under it, if the worker is still the site's and the
// site still the engine's: one removed meanwhile is stopped again, or it
// would run with nothing left that knows of it.
func (e *Engine) startWorker(ctx context.Context, s *Site, w *worker, rt runtime.Runtime, spec *manifest.Spec, port int, logFile, home string) {
	defer e.wg.Done()
	inst, err := rt.Start(ctx, spec, port, logFile, home)
	e.mu.Lock()
	if err != nil {
		if w.assigned {
			e.opts.Ports.Release(port)
			w.port, w.assigned = 0, false
		}
		w.failures++
		w.nextStart = time.Now().Add(backoff(w.failures, 60*time.Second))
		w.lastErr = err.Error()
		w.busy = false
		e.mu.Unlock()
		e.emit("worker.failed", s.Domain, "error", w.name+": "+err.Error(), nil)
		return
	}
	if e.sites[s.Domain] != s || s.Workers[w.name] != w {
		assigned := w.assigned
		e.mu.Unlock()
		_ = inst.Stop(context.WithoutCancel(ctx), 5*time.Second)
		if assigned {
			e.opts.Ports.Release(port)
		}
		e.emit("worker.discarded", s.Domain, "", w.name+": removed while it started; stopped it", nil)
		return
	}
	w.inst = inst
	w.revision = spec.Revision
	w.failures = 0
	w.lastErr = ""
	w.busy = false
	e.saveState(inst.State())
	e.mu.Unlock()
	e.emit("worker.started", s.Domain, "", w.name, map[string]any{"port": port})
}

// RestartWorker stops a worker so the next tick starts it again, now and
// without the backoff a crash would earn. Stopping and starting for good
// is the manifest's `disabled`, because a decision that does not survive
// a restart is not one; a restart is not a decision, so it lives here.
func (e *Engine) RestartWorker(ctx context.Context, domain, name string) error {
	e.mu.Lock()
	s, ok := e.sites[domain]
	var w *worker
	if ok {
		w = s.Workers[name]
	}
	if w == nil {
		e.mu.Unlock()
		return fmt.Errorf("%s: no worker %q running", domain, name)
	}
	if w.restarting {
		e.mu.Unlock()
		return fmt.Errorf("%s: %s is already restarting", domain, name)
	}
	if w.busy {
		// What it is doing would finish after this restart and undo it:
		// a start would install its process after the port was given back.
		e.mu.Unlock()
		return fmt.Errorf("%s: %s is starting or stopping; try again in a moment", domain, name)
	}
	inst := w.inst
	w.inst, w.restarting = nil, true
	e.mu.Unlock()

	if inst != nil {
		_ = inst.Stop(ctx, 5*time.Second)
	}
	e.mu.Lock()
	if w.assigned && w.port != 0 {
		e.opts.Ports.Release(w.port)
		w.port, w.assigned = 0, false
	}
	w.restarting, w.failures, w.nextStart, w.lastErr = false, 0, time.Time{}, ""
	e.mu.Unlock()
	e.clearState(workerKey(domain, name))
	e.emit("worker.restarting", domain, "", name, nil)
	return nil
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
	return manifest.WorkerSpec(domain, dir, name, def)
}

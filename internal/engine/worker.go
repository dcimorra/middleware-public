package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// worker is an always-on process with a written port. It restarts; it is
// never handed over, because whoever calls it has its port written and two
// generations cannot share it.
type worker struct {
	name      string
	port      int
	revision  string
	inst      runtime.Instance
	failures  int
	nextStart time.Time
	lastErr   string
}

func (e *Engine) reconcileWorkers(ctx context.Context, s *Site, m *manifest.Manifest) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stopAll := s.Mode == manifest.ModeDisabled
	for name, w := range s.Workers {
		if _, still := m.Workers[name]; still && !stopAll {
			continue
		}
		if w.inst != nil {
			_ = w.inst.Stop(ctx, 5*time.Second)
		}
		delete(s.Workers, name)
		e.emit("worker.stopped", s.Domain, "", name, nil)
	}
	if stopAll {
		return
	}
	for name, def := range m.Workers {
		spec := workerSpec(s.Domain, s.Dir, name, def)
		w, ok := s.Workers[name]
		if !ok {
			w = &worker{name: name, port: def.Port}
			s.Workers[name] = w
		}
		if w.inst != nil {
			select {
			case err := <-w.inst.Done():
				w.inst = nil
				w.failures++
				wait := backoff(w.failures, 60*time.Second)
				w.nextStart = time.Now().Add(wait)
				w.lastErr = fmt.Sprintf("%v; restart in %s", err, wait)
				e.emit("worker.exited", s.Domain, "error", name+": "+w.lastErr, nil)
			default:
			}
		}
		if w.inst != nil && w.revision != spec.Revision {
			_ = w.inst.Stop(ctx, 5*time.Second)
			w.inst = nil
			e.emit("worker.changed", s.Domain, "", name+": definition changed, restarting", nil)
		}
		if w.inst != nil || time.Now().Before(w.nextStart) {
			continue
		}
		logFile := filepath.Join(e.opts.DataDir, "logs", s.Domain, "worker-"+name+".log")
		home := filepath.Join(e.opts.DataDir, "homes", s.Domain)
		inst, err := runtime.Process{}.Start(ctx, spec, def.Port, logFile, home)
		if err != nil {
			w.failures++
			w.nextStart = time.Now().Add(backoff(w.failures, 60*time.Second))
			w.lastErr = err.Error()
			e.emit("worker.failed", s.Domain, "error", name+": "+err.Error(), nil)
			continue
		}
		w.inst = inst
		w.revision = spec.Revision
		w.port = def.Port
		w.failures = 0
		w.lastErr = ""
		e.emit("worker.started", s.Domain, "", name, map[string]any{"port": def.Port})
	}
}

func workerSpec(domain, dir, name string, def *manifest.Worker) *manifest.Spec {
	m := &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{Command: def.Command, Cwd: def.Cwd, Env: def.Env, EnvFile: def.EnvFile}}
	spec, err := manifest.Resolve(domain, dir, m)
	if err != nil {
		return &manifest.Spec{Domain: domain, SiteDir: dir, Cwd: dir, Command: def.Command}
	}
	spec.Domain = domain + "/" + name
	return spec
}

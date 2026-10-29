package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// Trial starts a site as m describes it on a spare port, waits for it to
// answer and stops it, without touching the generation that serves. It is
// how a manifest the house writes for the operator (a container it worked
// out) is known to work before it is written: once written, a generation
// that never comes up leaves the engine retrying with backoff for ever,
// the old one serving and nobody told why but in the events.
//
// The two overlap for as long as the trial runs, as two generations do in
// a handover, with the same folder and HOME. What it returns on failure
// carries the last lines of its own log, which is apart from the site's.
func (e *Engine) Trial(ctx context.Context, domain, dir string, m *manifest.Manifest) error {
	if m == nil || m.Run == nil {
		return fmt.Errorf("%s runs no process", domain)
	}
	spec, err := manifest.Resolve(domain, dir, m)
	if err != nil {
		return err
	}
	e.addSiteEnv(domain, m, spec)
	rt, ok := e.opts.Runtimes[spec.Runtime]
	if !ok {
		return fmt.Errorf("runtime %q is not available", spec.Runtime)
	}
	if spec.Port != 0 {
		// The serving generation holds it: a trial on it would either fail
		// for the wrong reason or take the site down.
		return fmt.Errorf("%s has a written port (%d): it cannot be tried beside the one that serves", domain, spec.Port)
	}
	port, err := e.opts.Ports.Claim(domain)
	if err != nil {
		return err
	}
	defer e.opts.Ports.Release(port)
	logDir := filepath.Join(e.opts.DataDir, "logs", domain)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	logFile := filepath.Join(logDir, "trial.log")
	_ = os.Remove(logFile)
	home := filepath.Join(e.opts.DataDir, "homes", domain)
	e.emit("trial.starting", domain, "", fmt.Sprintf("trying %s on port %d", spec.Runtime, port), map[string]any{"port": port, "runtime": spec.Runtime})
	inst, err := rt.Start(ctx, spec, port, logFile, home)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = inst.Stop(sctx, 5*time.Second)
	}()
	if err := runtime.WaitReady(ctx, inst, spec.Health, spec.Health.Timeout.Or(60*time.Second)); err != nil {
		return fmt.Errorf("%w (last lines of its log: %s)", err, tail(logFile, 12))
	}
	return nil
}

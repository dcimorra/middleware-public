package sites

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// RestartWorker stops one of the site's workers so the engine starts it
// again at once.
func (m *Manager) RestartWorker(ctx context.Context, domain, name string) error {
	if _, err := m.managedDir(domain); err != nil {
		return err
	}
	if m.Engine == nil {
		return errors.New("no engine")
	}
	return m.Engine.RestartWorker(ctx, domain, name)
}

// SetWorkerDisabled stops or starts one worker by writing the decision to
// the manifest (`workers.<name>.disabled`), where it survives a restart,
// like a site's.
func (m *Manager) SetWorkerDisabled(domain, name string, disabled bool) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	w, ok := mf.Workers[name]
	if !ok || w == nil {
		return Site{}, fmt.Errorf("%w: %s has no worker %q", ErrNotFound, domain, name)
	}
	w.Disabled = disabled
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, err
	}
	// worker.enabled and worker.disabled, not started and stopped: those
	// are the engine's, when it acts on this a tick later.
	typ, msg := "worker.enabled", name+" switched on by the operator"
	if disabled {
		typ, msg = "worker.disabled", name+" switched off by the operator"
	}
	m.emit(typ, domain, "", msg, map[string]any{"worker": name})
	return m.describe(domain), nil
}

// History is the site's kept manifests, newest first (the engine keeps
// them, see engine/history.go).
func (m *Manager) History(domain string) ([]engine.Version, error) {
	if _, err := m.managedDir(domain); err != nil {
		return nil, err
	}
	if m.Engine == nil {
		return nil, errors.New("no engine")
	}
	out := m.Engine.History(domain)
	if out == nil {
		out = []engine.Version{}
	}
	return out, nil
}

// Version is one kept manifest's content.
func (m *Manager) Version(domain, id string) ([]byte, error) {
	if _, err := m.managedDir(domain); err != nil {
		return nil, err
	}
	if m.Engine == nil {
		return nil, errors.New("no engine")
	}
	data, err := m.Engine.ManifestVersion(domain, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return data, nil
}

// RestoreVersion writes a kept manifest back. It goes through Update, so
// it is validated against today's rules and lands in the history as the
// newest version: going back is itself a change that can be undone.
func (m *Manager) RestoreVersion(domain, id string) (Site, error) {
	data, err := m.Version(domain, id)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Parse(data)
	if err != nil {
		return Site{}, fmt.Errorf("%w: that version no longer validates: %v", ErrInvalid, err)
	}
	site, err := m.Update(domain, mf)
	if err == nil {
		m.emit("manifest.restored", domain, "", "manifest restored to the version of "+id, map[string]any{"version": id})
	}
	return site, err
}

// ExecResult is a one-off command's outcome.
type ExecResult struct {
	ExitCode  int     `json:"exit_code"`
	Output    string  `json:"output"`
	Truncated bool    `json:"truncated,omitempty"`
	Seconds   float64 `json:"seconds"`
	TimedOut  bool    `json:"timed_out,omitempty"`
}

// execOutputCap is how much of a command's output is kept: the head,
// which is where an install says what it is doing, and enough to read.
const execOutputCap = 1 << 20

// Exec runs command through sh where the site runs (runtime.Exec: its
// folder, environment and HOME, inside its container if it has one), for
// at most timeout. It is the panel's "run a command": an `npm install`,
// a migration, a look at what is on disk. Every run is an event with the
// command in it, because it is the one button that does anything at all.
func (m *Manager) Exec(ctx context.Context, domain, command string, timeout time.Duration) (ExecResult, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return ExecResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return ExecResult{}, fmt.Errorf("%w: an empty command", ErrInvalid)
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return ExecResult{}, fmt.Errorf("manifest: %w", err)
	}
	if mf.Run == nil {
		// A static site or a project still gets its folder and HOME.
		mf = &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{Command: []string{"true"}}}
	}
	spec, err := manifest.Resolve(domain, dir, mf)
	if err != nil {
		return ExecResult{}, err
	}
	m.emit("site.exec", domain, "operator", command, map[string]any{"command": command})
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &capped{max: execOutputCap}
	start := time.Now()
	code, err := runtime.Exec(ctx, spec, filepath.Join(m.Root, "data", "homes", domain), runtime.ExecOptions{
		Args: []string{"sh", "-c", command}, Stdin: strings.NewReader(""), Stdout: out, Stderr: out,
	})
	res := ExecResult{ExitCode: code, Output: out.b.String(), Truncated: out.cut, Seconds: time.Since(start).Seconds()}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	return res, err
}

// capped keeps the first max bytes and says whether more came.
type capped struct {
	mu  sync.Mutex
	b   bytes.Buffer
	max int
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.b.Len(); room < len(p) {
		c.cut = true
		if room > 0 {
			c.b.Write(p[:room])
		}
		return len(p), nil
	}
	return c.b.Write(p)
}

package sites

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// SetServiceDisabled stops or starts one of a site's services by writing
// the decision to the manifest (`services.<name>.disabled`), where it
// survives a restart. Its names answer the stopped page meanwhile; the
// other services are not touched.
func (m *Manager) SetServiceDisabled(domain, name string, disabled bool) (Site, error) {
	return m.changeService(domain, name, func(sv *manifest.Service) (string, string) {
		sv.Disabled = disabled
		if disabled {
			return "service.disabled", name + " switched off by the operator"
		}
		return "service.enabled", name + " switched on by the operator"
	})
}

// SetServiceHold puts one service's names behind the holding page, or
// takes them out (`services.<name>.hold`).
func (m *Manager) SetServiceHold(domain, name string, hold manifest.Hold) (Site, error) {
	return m.changeService(domain, name, func(sv *manifest.Service) (string, string) {
		if hold.Enabled {
			sv.Hold = &hold
		} else {
			sv.Hold = nil
		}
		return "service.hold", fmt.Sprintf("%s: hold %v", name, hold.Enabled)
	})
}

func (m *Manager) changeService(domain, name string, change func(*manifest.Service) (typ, msg string)) (Site, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return Site{}, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return Site{}, fmt.Errorf("manifest: %w", err)
	}
	sv, ok := mf.Services[name]
	if !ok || sv == nil {
		return Site{}, fmt.Errorf("%w: %s has no service %q", ErrNotFound, domain, name)
	}
	typ, msg := change(sv)
	if err := manifest.Save(dir, mf); err != nil {
		return Site{}, err
	}
	// The subject is the service's, as the engine's events about it.
	m.emit(typ, domain+"#"+name, "", msg, map[string]any{"service": name})
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
func (m *Manager) Exec(ctx context.Context, domain, service, command string, timeout time.Duration) (ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return ExecResult{}, fmt.Errorf("%w: an empty command", ErrInvalid)
	}
	out := &capped{max: execOutputCap}
	start := time.Now()
	code, timedOut, err := m.Run(ctx, domain, service, []string{"sh", "-c", command}, timeout, out, false)
	return ExecResult{ExitCode: code, Output: out.b.String(), Truncated: out.cut, Seconds: time.Since(start).Seconds(), TimedOut: timedOut}, err
}

// Run is Exec for an exact argv, its output written to out as it comes
// (the API streams it): what `middleware exec` sends when it cannot run
// the command itself, from a site's turn, whose jail has no docker. For
// a site with services, service says in which; empty is the one that
// answers the site's own name. containerOnly refuses a command that
// would run on the machine: a turn and a service door are let through
// the house into a container only, where the container's walls hold; on
// the machine the command would run as the supervisor, outside the jail
// the turn's own commands run in.
func (m *Manager) Run(ctx context.Context, domain, service string, argv []string, timeout time.Duration, out io.Writer, containerOnly bool) (code int, timedOut bool, err error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return 1, false, err
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return 1, false, fmt.Errorf("%w: an empty command", ErrInvalid)
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return 1, false, fmt.Errorf("manifest: %w", err)
	}
	spec, err := m.execSpec(domain, dir, service, mf)
	if err != nil {
		return 1, false, err
	}
	if containerOnly && (spec.Runtime != "docker" || spec.Docker == nil) {
		return 1, false, fmt.Errorf("%w: %s runs on the machine, not in a container: run the command yourself, in your own folder", ErrForbidden, spec.Domain)
	}
	command := strings.Join(argv, " ")
	if len(argv) == 3 && argv[0] == "sh" && argv[1] == "-c" {
		command = argv[2]
	}
	data := map[string]any{"command": command}
	subject := domain
	if spec.Service != "" {
		data["service"] = spec.Service
		subject = domain + "#" + spec.Service
	}
	m.emit("site.exec", subject, "operator", command, data)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, err = runtime.Exec(ctx, spec, filepath.Join(m.Root, "data", "homes", domain), runtime.ExecOptions{
		Args: argv, Stdin: strings.NewReader(""), Stdout: out, Stderr: out,
	})
	return code, ctx.Err() == context.DeadlineExceeded, err
}

// execSpec is where a one-off command runs: the site's run, or one of its
// services (the running generation's spec, so its image and network).
func (m *Manager) execSpec(domain, dir, service string, mf *manifest.Manifest) (*manifest.Spec, error) {
	if len(mf.Services) == 0 {
		if service != "" {
			return nil, fmt.Errorf("%w: %s has no services", ErrNotFound, domain)
		}
		if mf.Run == nil {
			// A static site or a project still gets its folder and HOME.
			mf = &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{Command: []string{"true"}}}
		}
		return manifest.Resolve(domain, dir, mf)
	}
	if service == "" {
		name, ok := mf.ServiceFor(domain, "/")
		if !ok {
			return nil, fmt.Errorf("%w: no service answers %s; name one (services: %s)", ErrInvalid, domain, strings.Join(mf.ServiceNames(), ", "))
		}
		service = name
	}
	if _, ok := mf.Services[service]; !ok {
		return nil, fmt.Errorf("%w: %s has no service %q (services: %s)", ErrNotFound, domain, service, strings.Join(mf.ServiceNames(), ", "))
	}
	if m.Engine == nil {
		return nil, errors.New("no engine")
	}
	return m.Engine.ServiceSpec(domain, service)
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

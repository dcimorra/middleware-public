package sites

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// DeployTimeout bounds one deployment. Half an hour covers a cold
// `npm ci` on a small box; a build that needs more is a task, not a
// button.
const DeployTimeout = 30 * time.Minute

// ErrDeploying says a deployment is already running for the site.
var ErrDeploying = errors.New("a deployment is already running")

type deployState struct {
	mu      sync.Mutex
	running map[string]bool
}

// Deploy runs the site's deploy.github.build commands in its folder and,
// when a pointer is configured, writes the build id into it, which is
// what makes the engine hand over. It returns as soon as the job has
// started; progress is on the bus as deploy.started, deploy.done and
// deploy.failed, and the output is in data/logs/<domain>/deploy.log.
//
// The build id is handed to every command as MIDDLEWARE_BUILD, so a build
// script can write into a folder named after it and the pointer then
// names that folder: `next build` with `distDir: process.env.MIDDLEWARE_BUILD`
// is the whole recipe. Without a pointer the build runs in place and the
// instance is restarted, which is a handover too, just one that both
// generations share files through.
func (m *Manager) Deploy(domain string) (buildID string, err error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return "", err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return "", fmt.Errorf("manifest: %w", err)
	}
	var build [][]string
	var pointer string
	if mf.Deploy != nil && mf.Deploy.GitHub != nil {
		build, pointer = mf.Deploy.GitHub.Build, mf.Deploy.GitHub.Pointer
	}
	if len(build) == 0 && pointer == "" && mf.Run == nil {
		return "", errors.New("nothing to deploy: no deploy.github.build, no pointer and no run section")
	}
	m.deploys.mu.Lock()
	if m.deploys.running == nil {
		m.deploys.running = map[string]bool{}
	}
	if m.deploys.running[domain] {
		m.deploys.mu.Unlock()
		return "", ErrDeploying
	}
	m.deploys.running[domain] = true
	m.deploys.mu.Unlock()

	buildID = time.Now().UTC().Format("20060102T150405Z")
	m.emit("deploy.started", domain, "", "build "+buildID, map[string]any{"build": buildID, "steps": len(build)})
	go func() {
		defer func() {
			m.deploys.mu.Lock()
			delete(m.deploys.running, domain)
			m.deploys.mu.Unlock()
		}()
		if err := m.runDeploy(domain, dir, mf, build, pointer, buildID); err != nil {
			m.emit("deploy.failed", domain, "", err.Error(), map[string]any{"build": buildID})
			return
		}
		m.emit("deploy.done", domain, "", "build "+buildID+" deployed", map[string]any{"build": buildID, "pointer": pointer})
	}()
	return buildID, nil
}

// Deploying reports whether a deployment is in progress for domain.
func (m *Manager) Deploying(domain string) bool {
	m.deploys.mu.Lock()
	defer m.deploys.mu.Unlock()
	return m.deploys.running[domain]
}

func (m *Manager) runDeploy(domain, dir string, mf *manifest.Manifest, build [][]string, pointer, buildID string) error {
	parent := m.Ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, DeployTimeout)
	defer cancel()
	logPath := filepath.Join(m.logsDir(domain), "deploy.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	fmt.Fprintf(out, "\n--- %s middleware: deploy %s build %s\n", time.Now().Format(time.RFC3339), domain, buildID)
	env := m.deployEnv(domain, dir, mf, buildID)
	// The site's HOME may not exist before its first start, and npm
	// refuses to run without one it can write to.
	if err := os.MkdirAll(filepath.Join(m.Root, "data", "homes", domain), 0o700); err != nil {
		return err
	}
	for i, argv := range build {
		if len(argv) == 0 {
			continue
		}
		fmt.Fprintf(out, "--- step %d/%d: %s\n", i+1, len(build), strings.Join(argv, " "))
		if err := runStep(ctx, dir, argv, env, out); err != nil {
			fmt.Fprintf(out, "--- failed: %v\n", err)
			return fmt.Errorf("step %d (%s): %w", i+1, argv[0], err)
		}
	}
	if pointer != "" {
		if err := fsx.WriteAtomic(filepath.Join(dir, pointer), []byte(buildID+"\n"), 0o644); err != nil {
			return fmt.Errorf("pointer: %w", err)
		}
		fmt.Fprintf(out, "--- pointer %s -> %s\n", pointer, buildID)
		return nil
	}
	if mf.Run != nil && m.Engine != nil {
		if err := m.Engine.Restart(ctx, domain); err != nil && !strings.Contains(err.Error(), "not a running site") {
			return err
		}
		fmt.Fprintln(out, "--- restarted")
	}
	return nil
}

// deployEnv is the site's own environment plus what a build needs. The
// supervisor's environment is not inherited: a build step must not see
// the panel's secrets any more than the site does.
func (m *Manager) deployEnv(domain, dir string, mf *manifest.Manifest, buildID string) []string {
	env := map[string]string{}
	for _, k := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	if mf.Run != nil {
		if spec, err := manifest.Resolve(domain, dir, mf); err == nil {
			for k, v := range spec.Env {
				env[k] = v
			}
		}
	}
	env["HOME"] = filepath.Join(m.Root, "data", "homes", domain)
	env["MIDDLEWARE_DOMAIN"] = domain
	env["MIDDLEWARE_SITE_DIR"] = dir
	env["MIDDLEWARE_BUILD"] = buildID
	env["CI"] = "true"
	delete(env, "PORT")
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// runStep runs one build command in its own process group so a timeout
// kills the whole tree, not just the shell npm spawned.
func runStep(ctx context.Context, dir string, argv []string, env []string, out *os.File) error {
	bin := argv[0]
	if strings.HasPrefix(bin, "./") || strings.HasPrefix(bin, "../") {
		bin = filepath.Join(dir, bin)
	}
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

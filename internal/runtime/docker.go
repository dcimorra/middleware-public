package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Docker runs an instance as a container through the docker CLI. The CLI
// rather than the engine API keeps the SDK (and its hundred transitive
// modules) out of the core; the few commands used here have been stable for
// a decade.
type Docker struct {
	// Bin is the docker binary; empty means "docker" from PATH.
	Bin string
}

// Name implements Runtime.
func (Docker) Name() string { return "docker" }

func (d Docker) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

type dockerInstance struct {
	d        Docker
	name     string
	port     int
	revision string
	domain   string
	logFile  string
	started  time.Time
	done     chan error
}

// Start implements Runtime. The site folder is mounted at /site, the port
// is published on loopback only, and the container is named after the
// domain and the port so two handover generations never collide.
func (d Docker) Start(ctx context.Context, spec *manifest.Spec, port int, logFile, home string) (Instance, error) {
	if spec.Docker == nil {
		return nil, errors.New("docker runtime needs a docker section")
	}
	image := spec.Docker.Image
	if spec.Docker.Build != "" {
		image = "middleware/" + strings.ReplaceAll(spec.Domain, ".", "-") + ":" + spec.Revision
		build := exec.CommandContext(ctx, d.bin(), "build", "-q", "-t", image, "-f", filepath.Join(spec.SiteDir, spec.Docker.Build), spec.SiteDir)
		if out, err := build.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("docker build: %w: %s", err, bytes.TrimSpace(out))
		}
	}
	argv, env := spec.WithPort(port)
	name := fmt.Sprintf("mw-%s-%d", strings.ReplaceAll(spec.Domain, ".", "-"), port)
	args := []string{"run", "-d", "--name", name, "--restart", "no",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"-v", spec.SiteDir + ":/site", "-w", "/site",
		"--label", "middleware.domain=" + spec.Domain,
		"--label", "middleware.revision=" + spec.Revision}
	if spec.Docker.Memory != "" {
		args = append(args, "--memory", spec.Docker.Memory)
	}
	for _, v := range spec.Docker.Volumes {
		args = append(args, "-v", v)
	}
	for _, kv := range buildEnv(env, port, spec.Domain, "/site", "/site") {
		if strings.HasPrefix(kv, "PATH=") {
			continue // the image's PATH, not the host's
		}
		args = append(args, "-e", kv)
	}
	args = append(args, image)
	args = append(args, argv...)
	run := exec.CommandContext(ctx, d.bin(), args...)
	if out, err := run.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker run: %w: %s", err, bytes.TrimSpace(out))
	}
	inst := &dockerInstance{d: d, name: name, port: port, revision: spec.Revision, domain: spec.Domain, logFile: logFile, started: time.Now(), done: make(chan error, 1)}
	go inst.watch()
	go inst.tailLogs()
	return inst, nil
}

// Adopt implements Runtime.
func (d Docker) Adopt(ctx context.Context, st State) (Instance, error) {
	if st.Container == "" {
		return nil, ErrGone
	}
	out, err := exec.CommandContext(ctx, d.bin(), "inspect", "-f", "{{.State.Running}}", st.Container).Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil, ErrGone
	}
	inst := &dockerInstance{d: d, name: st.Container, port: st.Port, revision: st.Revision, domain: st.Domain, logFile: st.LogFile, started: st.StartedAt, done: make(chan error, 1)}
	go inst.watch()
	return inst, nil
}

func (c *dockerInstance) Port() int          { return c.port }
func (c *dockerInstance) Revision() string   { return c.revision }
func (c *dockerInstance) Done() <-chan error { return c.done }

func (c *dockerInstance) Alive() bool {
	out, err := exec.Command(c.d.bin(), "inspect", "-f", "{{.State.Running}}", c.name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func (c *dockerInstance) State() State {
	return State{Domain: c.domain, Runtime: "docker", Port: c.port, Revision: c.revision, Container: c.name, LogFile: c.logFile, StartedAt: c.started}
}

// Stop does `docker stop` with the grace as its timeout, then removes the
// container so the name is free for the next generation.
func (c *dockerInstance) Stop(ctx context.Context, grace time.Duration) error {
	secs := int(grace.Seconds())
	if secs < 1 {
		secs = 1
	}
	_ = exec.CommandContext(ctx, c.d.bin(), "stop", "-t", fmt.Sprint(secs), c.name).Run()
	_ = exec.CommandContext(ctx, c.d.bin(), "rm", "-f", c.name).Run()
	return nil
}

// watch blocks on `docker wait`, which returns when the container exits.
func (c *dockerInstance) watch() {
	err := exec.Command(c.d.bin(), "wait", c.name).Run()
	if err == nil {
		err = errors.New("container exited")
	}
	c.done <- err
	close(c.done)
}

// tailLogs copies the container's output into the site's log file so the
// panel shows one log per site whatever the runtime.
func (c *dockerInstance) tailLogs() {
	if c.logFile == "" {
		return
	}
	f, err := os.OpenFile(c.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	cmd := exec.Command(c.d.bin(), "logs", "-f", c.name)
	cmd.Stdout = f
	cmd.Stderr = f
	_ = cmd.Run()
}

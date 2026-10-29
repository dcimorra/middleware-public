package cloudflare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// MetricsAddr is where the connector answers /ready; config.yml says so.
const MetricsAddr = "127.0.0.1:46101"

// Connector keeps cloudflared running while the module is enabled with a
// tunnel. The supervisor runs it itself instead of leaving it to a unit:
// an installation in a container has no systemd, and a unit nobody wrote
// was what V2's first draft had (the module downloaded the binary and
// nothing ever started it), which looked like a configured tunnel that
// served nothing.
//
// It outlives the supervisor like a site does (its own process group,
// KillMode=process in the unit) and is re-adopted from
// modules/cloudflared/connector.json, so restarting the supervisor never
// drops the tunnel. The pid is adopted only if its command line is still
// cloudflared with this installation's token file: a reused pid is not.
type Connector struct {
	Root string
	Bus  *events.Bus
	// Want says whether the connector should run: the module is enabled
	// and has a tunnel. Asked on every pass.
	Want func() bool
	Tick time.Duration

	mu       sync.Mutex
	pid      int
	started  time.Time
	exited   chan struct{}
	failures int
	next     time.Time
	lastErr  string
	ready    bool
}

type connectorState struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

// ConnectorStatus is what the panel shows.
type ConnectorStatus struct {
	Running bool      `json:"running"`
	Ready   bool      `json:"ready"`
	PID     int       `json:"pid,omitempty"`
	Since   time.Time `json:"since,omitempty"`
	Error   string    `json:"error,omitempty"`
	Log     string    `json:"log"`
}

func (c *Connector) dir() string       { return filepath.Join(c.Root, "modules", "cloudflared") }
func (c *Connector) bin() string       { return filepath.Join(c.Root, "modules", "bin", BinaryName) }
func (c *Connector) tokenFile() string { return filepath.Join(c.dir(), "token") }
func (c *Connector) statePath() string { return filepath.Join(c.dir(), "connector.json") }
func (c *Connector) logPath() string   { return filepath.Join(c.Root, "data", "logs", "cloudflared.log") }

func (c *Connector) emit(typ, level, msg string) {
	if c.Bus != nil {
		c.Bus.Emit(events.Event{Type: typ, Subject: "cloudflared", Actor: "engine", Level: level, Message: msg})
	}
}

// Status reports the connector's state.
func (c *Connector) Status() ConnectorStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ConnectorStatus{Running: c.pid != 0, Ready: c.ready, PID: c.pid, Since: c.started, Error: c.lastErr, Log: c.logPath()}
}

// Run adopts a connector left running, then keeps the running one equal to
// Want until ctx ends. It leaves the connector running on exit on purpose.
func (c *Connector) Run(ctx context.Context) {
	if c.Tick == 0 {
		c.Tick = 5 * time.Second
	}
	c.adopt()
	t := time.NewTicker(c.Tick)
	defer t.Stop()
	for {
		c.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Connector) reconcile(ctx context.Context) {
	want := c.Want != nil && c.Want() && fsx.Exists(c.bin()) && fsx.Exists(c.tokenFile())
	c.mu.Lock()
	pid, exited := c.pid, c.exited
	c.mu.Unlock()
	if pid != 0 {
		select {
		case <-exited:
			c.mu.Lock()
			c.pid, c.ready = 0, false
			c.failures++
			wait := time.Second << min(c.failures, 6)
			c.next = time.Now().Add(wait)
			c.lastErr = fmt.Sprintf("exited; restart in %s (last lines: %s)", wait, tailFile(c.logPath(), 3))
			msg := c.lastErr
			c.mu.Unlock()
			_ = os.Remove(c.statePath())
			c.emit("cloudflared.exited", "error", msg)
			return
		default:
		}
	}
	switch {
	case want && pid == 0:
		c.mu.Lock()
		wait := time.Now().Before(c.next)
		c.mu.Unlock()
		if !wait {
			c.start()
		}
	case !want && pid != 0:
		c.stop()
	case pid != 0:
		c.checkReady(ctx)
	}
}

func (c *Connector) start() {
	if err := os.MkdirAll(filepath.Dir(c.logPath()), 0o755); err != nil {
		c.fail(err)
		return
	}
	logf, err := os.OpenFile(c.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		c.fail(err)
		return
	}
	defer logf.Close()
	cmd := exec.Command(c.bin(), "--config", filepath.Join(c.dir(), "config.yml"), "tunnel", "--no-autoupdate", "run", "--token-file", c.tokenFile())
	// The file itself, never an io.Writer: a pipe would tie the
	// connector's life to this process and hang Wait on a grandchild.
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Dir = c.dir()
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + c.dir()}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		c.fail(err)
		return
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	now := time.Now()
	c.mu.Lock()
	c.pid, c.started, c.exited, c.lastErr = cmd.Process.Pid, now, exited, ""
	c.mu.Unlock()
	_ = fsx.WriteJSON(c.statePath(), connectorState{PID: cmd.Process.Pid, Started: now}, 0o600)
	c.emit("cloudflared.started", "", "pid "+strconv.Itoa(cmd.Process.Pid))
}

func (c *Connector) fail(err error) {
	c.mu.Lock()
	c.failures++
	c.next = time.Now().Add(time.Second << min(c.failures, 6))
	c.lastErr = err.Error()
	c.mu.Unlock()
	c.emit("cloudflared.failed", "error", err.Error())
}

// stop sends SIGTERM to the group, then SIGKILL after ten seconds.
func (c *Connector) stop() {
	c.mu.Lock()
	pid, exited := c.pid, c.exited
	c.mu.Unlock()
	if pid == 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
	}
	c.mu.Lock()
	c.pid, c.ready, c.failures, c.next = 0, false, 0, time.Time{}
	c.mu.Unlock()
	_ = os.Remove(c.statePath())
	c.emit("cloudflared.stopped", "", "the module is off or has no tunnel")
}

// adopt attaches to a connector a previous supervisor left running.
func (c *Connector) adopt() {
	var st connectorState
	if err := fsx.ReadJSON(c.statePath(), &st); err != nil || st.PID <= 0 {
		return
	}
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(st.PID) + "/cmdline")
	if err != nil || !bytes.Contains(cmdline, []byte(BinaryName)) || !bytes.Contains(cmdline, []byte(c.tokenFile())) {
		_ = os.Remove(c.statePath())
		return
	}
	exited := make(chan struct{})
	pid := st.PID
	go func() {
		for syscall.Kill(pid, 0) == nil && !zombie(pid) {
			time.Sleep(time.Second)
		}
		close(exited)
	}()
	c.mu.Lock()
	c.pid, c.started, c.exited = pid, st.Started, exited
	c.mu.Unlock()
	c.emit("cloudflared.adopted", "", "still running from a previous supervisor, pid "+strconv.Itoa(pid))
}

func zombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(data, ')')
	return i < 0 || i+2 >= len(data) || data[i+2] == 'Z'
}

// checkReady asks the connector's /ready, which answers 200 once it holds
// a connection to the edge. A connector that runs but never connects (a
// revoked token, a blocked egress) is what this tells apart.
func (c *Connector) checkReady(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+MetricsAddr+"/ready", nil)
	resp, err := http.DefaultClient.Do(req)
	ready := err == nil && resp.StatusCode == http.StatusOK
	if resp != nil {
		resp.Body.Close()
	}
	c.mu.Lock()
	changed := c.ready != ready
	c.ready = ready
	if ready {
		c.failures = 0
	}
	c.mu.Unlock()
	if changed {
		if ready {
			c.emit("cloudflared.ready", "", "connected to the edge")
		} else {
			c.emit("cloudflared.not_ready", "warn", "running but not connected to the edge; see "+c.logPath())
		}
	}
}

func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte(" | ")))
}

var errNoConnector = errors.New("cloudflare: the connector is not supervised by this process (the CLI does not run it)")

// Package runtime starts, watches and stops site instances. The engine
// decides *when*; a Runtime knows *how* for one kind of process (a plain
// child process, a Docker container). Both implementations honour the same
// contract so a site can switch between them with a manifest edit.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Instance is one running copy of a site.
type Instance interface {
	// Port the instance listens on, on loopback.
	Port() int
	// Revision of the spec it was born from.
	Revision() string
	// Alive reports whether the process or container still exists.
	Alive() bool
	// Stop asks politely, waits up to grace, then kills.
	Stop(ctx context.Context, grace time.Duration) error
	// Done is closed when the instance has exited, with the exit error.
	Done() <-chan error
	// State is what must be persisted so a later supervisor can re-adopt.
	State() State
}

// State is the on-disk record of a running instance.
type State struct {
	Domain    string `json:"domain"`
	Runtime   string `json:"runtime"`
	Port      int    `json:"port"`
	Revision  string `json:"revision"`
	PID       int    `json:"pid,omitempty"`
	Container string `json:"container,omitempty"`
	// ContainerID is the container's full id, which its processes' cgroup
	// names: how the guard tells which processes are this instance's when
	// it cannot read where they work (a rootful daemon's are root's).
	ContainerID string    `json:"container_id,omitempty"`
	LogFile     string    `json:"log_file,omitempty"`
	StartedAt   time.Time `json:"started_at"`
}

// StatePath is where the engine keeps an instance's state: key is a
// domain, or domain/worker, written domain@worker. One place, because the
// engine writes it and the guard and a move read it.
func StatePath(dataDir, key string) string {
	return filepath.Join(dataDir, "state", strings.ReplaceAll(key, "/", "@")+".json")
}

// Runtime starts instances and re-adopts the ones it finds.
type Runtime interface {
	Name() string
	// Start launches the spec on port and returns once the process exists
	// (not once it is ready: that is WaitReady's job).
	Start(ctx context.Context, spec *manifest.Spec, port int, logFile, home string) (Instance, error)
	// Adopt rebuilds an Instance from state if it is still alive.
	Adopt(ctx context.Context, st State) (Instance, error)
}

// ErrGone says the state described something that no longer exists.
var ErrGone = errors.New("instance is gone")

// WaitReady blocks until the port accepts connections and, if a health path
// is set, until it answers a status below 500. It fails as soon as the
// instance exits: waiting the full timeout on a process that died at startup
// is what made early deploys feel hung.
func WaitReady(ctx context.Context, inst Instance, health manifest.Health, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", inst.Port())
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-inst.Done():
			if err == nil {
				err = errors.New("exited")
			}
			return fmt.Errorf("instance %s before it was ready", err)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s", timeout)
		}
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			if health.Path == "" {
				return nil
			}
			resp, err := client.Get("http://" + addr + health.Path)
			if err == nil {
				resp.Body.Close()
				if health.Healthy(resp.StatusCode) {
					return nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

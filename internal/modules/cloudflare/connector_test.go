package cloudflare

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeConnector writes a cloudflared that only sleeps, so the supervision
// is exercised with real processes and no network. No exec: the shell
// keeps the command line with the token file, as cloudflared would.
func fakeConnector(t *testing.T) (root string) {
	root = t.TempDir()
	os.MkdirAll(filepath.Join(root, "modules", "bin"), 0o755)
	os.MkdirAll(filepath.Join(root, "modules", "cloudflared"), 0o755)
	os.WriteFile(filepath.Join(root, "modules", "bin", BinaryName), []byte("#!/bin/sh\necho connecting\nsleep 300\necho done\n"), 0o755)
	os.WriteFile(filepath.Join(root, "modules", "cloudflared", "token"), []byte("t\n"), 0o600)
	return root
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTheConnectorRunsWhileWantedOutlivesItsSupervisorAndStops(t *testing.T) {
	root := fakeConnector(t)
	var want atomic.Bool
	want.Store(true)
	c := &Connector{Root: root, Want: want.Load, Tick: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	waitUntil(t, "started", func() bool { return c.Status().Running })
	pid := c.Status().PID
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })

	// The supervisor goes away; the connector does not, and the next
	// supervisor adopts it instead of starting a second one.
	cancel()
	time.Sleep(150 * time.Millisecond)
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the connector died with its supervisor")
	}
	c2 := &Connector{Root: root, Want: want.Load, Tick: 50 * time.Millisecond}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go c2.Run(ctx2)
	waitUntil(t, "adopted", func() bool { return c2.Status().PID == pid })
	time.Sleep(200 * time.Millisecond)
	if c2.Status().PID != pid {
		t.Fatal("a second connector was started next to the adopted one")
	}

	// It dies: it is started again.
	syscall.Kill(-pid, syscall.SIGKILL)
	waitUntil(t, "restarted", func() bool { s := c2.Status(); return s.Running && s.PID != pid })
	pid2 := c2.Status().PID
	t.Cleanup(func() { syscall.Kill(-pid2, syscall.SIGKILL) })

	// The module goes off: it is stopped and not started again.
	want.Store(false)
	waitUntil(t, "stopped", func() bool { return !c2.Status().Running })
	time.Sleep(200 * time.Millisecond)
	if c2.Status().Running || syscall.Kill(pid2, 0) == nil && !zombie(pid2) {
		t.Fatal("the connector still runs with the module off")
	}
}

func TestAPidThatIsNoLongerTheConnectorIsNotAdopted(t *testing.T) {
	root := fakeConnector(t)
	// Our own pid: alive, but not cloudflared with this token file.
	os.WriteFile(filepath.Join(root, "modules", "cloudflared", "connector.json"), []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`}`), 0o600)
	c := &Connector{Root: root, Want: func() bool { return false }}
	c.adopt()
	if c.Status().Running {
		t.Fatal("a stranger's pid was adopted")
	}
}

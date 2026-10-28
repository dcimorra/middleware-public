package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// zombie reports whether pid has exited and waits to be reaped. In a
// container whose PID 1 does not reap, a killed orphan stays like that, and
// for these tests it is dead.
func zombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(data), ')')
	return i >= 0 && i+2 < len(data) && data[i+2] == 'Z'
}

func dead(pid int) bool {
	return syscall.Kill(pid, 0) != nil || zombie(pid)
}

func waitDead(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !dead(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !dead(pid) {
		t.Fatalf("%s (pid %d) is still running", what, pid)
	}
}

// A site whose shell exits while what it started in the background goes
// on: the engine used to forget the instance and release its port with the
// child still on it, one more child with every restart the backoff made.
func TestAnExitedInstanceTakesItsGroupWithIt(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "group.test")
	os.MkdirAll(dir, 0o755)
	pidFile := filepath.Join(dir, "child.pid")
	script := fmt.Sprintf(`"$MW_BIN" & echo $! > %s; sleep 1; exit 1`, pidFile)
	m := fmt.Sprintf(`{"version":2,"run":{"command":["/bin/sh","-c",%q],"env":{"MW_TEST_SERVER":"1","BUILD":"g","MW_BIN":%q},"health":{"timeout":"10s"}}}`, script, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "group.test", func(s Snapshot) bool { return s.Alive })
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var child int
	fmt.Sscan(strings.TrimSpace(string(data)), &child)
	if child <= 0 {
		t.Fatalf("no child pid in %q", data)
	}
	defer syscall.Kill(child, syscall.SIGKILL)
	waitFor(t, e, "group.test", func(s Snapshot) bool { return s.Failures >= 1 })
	waitDead(t, child, "the child of an instance whose shell exited")
}

// tickRecovering runs one tick and returns what it panicked with. A panic
// under the engine's lock leaves the lock held, and the cleanup that stops
// what the test started then waited for it for good: the test hung instead
// of failing. The lock is freed whoever holds it, since nobody does now.
func tickRecovering(ctx context.Context, e *Engine) (panicked any) {
	defer func() {
		if p := recover(); p != nil {
			panicked = p
			e.mu.TryLock()
			e.mu.Unlock()
		}
	}()
	e.tick(ctx)
	return nil
}

// startForAdoption starts the test binary as a site's process the way a
// previous supervisor would have, and leaves its state behind.
func startForAdoption(t *testing.T, e *Engine, sites, domain string) runtime.Instance {
	t.Helper()
	dir := writeSite(t, sites, domain, "x", "")
	spec := &manifest.Spec{Domain: domain, SiteDir: dir, Cwd: dir, Command: []string{os.Args[0]}, Env: map[string]string{"MW_TEST_SERVER": "1", "BUILD": "x"}, Revision: "previous"}
	inst, err := runtime.Process{}.Start(context.Background(), spec, freePort(t), filepath.Join(e.opts.DataDir, "logs", domain, "site.log"), filepath.Join(e.opts.DataDir, "homes", domain))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Stop(context.Background(), time.Second) })
	if err := runtime.WaitReady(context.Background(), inst, manifest.Health{}, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	e.saveState(inst.State())
	return inst
}

// An adopted instance has no spec until its first reconcile, and one that
// exited before it took the supervisor down with a nil dereference in the
// crash branch.
func TestAnAdoptedInstanceThatExitsBeforeTheFirstTickIsRestarted(t *testing.T) {
	e, sites := newTestEngine(t)
	inst := startForAdoption(t, e, sites, "early.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.adoptAll(ctx)
	e.mu.RLock()
	adopted := e.sites["early.test"].Instance
	e.mu.RUnlock()
	if adopted == nil {
		t.Fatal("not adopted")
	}
	_ = inst.Stop(context.Background(), time.Second)
	select {
	case <-adopted.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the adopted instance never noticed its process exited")
	}
	if p := tickRecovering(ctx, e); p != nil {
		t.Fatalf("the tick panicked: %v", p)
	}
	sn, _ := e.Lookup("early.test")
	if sn.Alive || sn.Failures != 1 {
		t.Fatalf("the exit was not counted: %+v", sn)
	}
}

// An adopted process that is the one recorded but no longer listens is
// stopped, not forgotten: forgotten, it kept running outside every state
// file while the next tick started a second copy next to it.
func TestAnAdoptedInstanceThatDoesNotListenIsStopped(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := filepath.Join(sites, "deaf.test")
	os.MkdirAll(dir, 0o755)
	spec := &manifest.Spec{Domain: "deaf.test", SiteDir: dir, Cwd: dir, Command: []string{"/bin/sh", "-c", "exec sleep 300"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := runtime.Process{}.Start(context.Background(), spec, freePort(t), filepath.Join(e.opts.DataDir, "logs", "deaf.test", "site.log"), filepath.Join(e.opts.DataDir, "homes", "deaf.test"))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop(context.Background(), time.Second)
	e.saveState(inst.State())
	e.adoptAll(context.Background())
	waitDead(t, inst.State().PID, "the adopted process that does not listen")
	if _, err := os.Stat(e.statePath("deaf.test")); !os.IsNotExist(err) {
		t.Fatalf("its state is still there: %v", err)
	}
}

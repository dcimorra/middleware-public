package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

func TestTheChildEnvironmentHasNoDuplicateKeysAndPortLast(t *testing.T) {
	t.Setenv("PATH", "/bin")
	env := buildEnv(map[string]string{"PORT": "1", "A": "x", "PATH": "/site/bin"}, 4321, "a.test", "/s", "/h")
	if hasDuplicateKeys(env) {
		t.Fatalf("duplicates: %v", env)
	}
	if env[len(env)-1] != "PORT=4321" {
		t.Fatalf("PORT is not last: %v", env)
	}
	for _, kv := range env {
		if kv == "PATH=/site/bin" {
			return
		}
	}
	t.Fatalf("the manifest's PATH did not win: %v", env)
}

func TestStartWritesOutputToTheLogFileAndStopReachesTheGroup(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	// A shell that spawns a grandchild: Stop must take both down.
	os.WriteFile(script, []byte("#!/bin/sh\necho started on $PORT\nsleep 300 &\nwait\n"), 0o755)
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"./run.sh"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := Process{}.Start(context.Background(), spec, 45999, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !inst.Alive() {
		t.Fatal("not alive")
	}
	if err := inst.Stop(context.Background(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if inst.Alive() {
		t.Fatal("still alive after Stop")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "out.log"))
	if !strings.Contains(string(data), "started on 45999") {
		t.Fatalf("log: %q", data)
	}
}

func TestAdoptRefusesADeadPid(t *testing.T) {
	_, err := Process{}.Adopt(context.Background(), State{PID: 999999})
	if err != ErrGone {
		t.Fatalf("got %v", err)
	}
}

func TestWaitReadyFailsFastWhenTheInstanceDies(t *testing.T) {
	dir := t.TempDir()
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"/bin/sh", "-c", "exit 3"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := Process{}.Start(context.Background(), spec, 45998, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = WaitReady(context.Background(), inst, manifest.Health{}, 30*time.Second)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestAReusedPidIsNotAdopted(t *testing.T) {
	began, ok := processStart(os.Getpid())
	if !ok {
		t.Skip("no /proc")
	}
	if d := time.Since(began); d < 0 || d > 5*time.Minute {
		t.Fatalf("this process started %s ago, which is not plausible", d)
	}
	st := State{Domain: "a.test", Runtime: "process", PID: os.Getpid(), StartedAt: began}
	if _, err := (Process{}).Adopt(context.Background(), st); err != nil {
		t.Fatalf("the process that was started was not adopted: %v", err)
	}
	st.StartedAt = began.Add(-time.Hour)
	if _, err := (Process{}).Adopt(context.Background(), st); err != ErrGone {
		t.Fatalf("a pid started an hour after the recorded start was adopted: %v", err)
	}
}

// A site whose command is `node` starts with the Node it was given, found
// by the child's PATH and not the supervisor's.
func TestASiteThatSaysNodeRunsWithTheNodeItWasGiven(t *testing.T) {
	dir := t.TempDir()
	nodeDir := filepath.Join(dir, "nvm", "bin")
	os.MkdirAll(nodeDir, 0o755)
	os.WriteFile(filepath.Join(nodeDir, "node-made-up"), []byte("#!/bin/sh\necho \"this node, PATH starts $PATH\" | cut -c1-200\nsleep 300\n"), 0o755)
	NodeDir = func(_ context.Context, siteDir string, argv []string) string { return nodeDir }
	defer func() { NodeDir = nil }()
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir, Cwd: dir, Command: []string{"node-made-up", "server.js"}, Env: map[string]string{}, Revision: "r1"}
	inst, err := Process{}.Start(context.Background(), spec, 45998, filepath.Join(dir, "out.log"), filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop(context.Background(), time.Second)
	time.Sleep(300 * time.Millisecond)
	data, _ := os.ReadFile(filepath.Join(dir, "out.log"))
	if !strings.Contains(string(data), "this node, PATH starts "+nodeDir) {
		t.Fatalf("log: %q", data)
	}
}

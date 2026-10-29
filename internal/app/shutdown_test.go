package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/runtime"
)

// TestMain lets the test binary double as a supervised site: with
// MW_TEST_SERVER set it answers its BUILD on every path.
func TestMain(m *testing.M) {
	if os.Getenv("MW_TEST_SERVER") != "" {
		l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
		if err != nil {
			os.Exit(1)
		}
		build := os.Getenv("BUILD")
		_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, build) }))
		return
	}
	os.Exit(m.Run())
}

// shutdownHook serves a slow request and an endless stream, and records
// when the app told it to write what it keeps.
type shutdownHook struct {
	slowStarted chan struct{}
	slowDone    atomic.Bool
	stoppedLate atomic.Bool // Stop ran after the slow request had answered
	stopped     atomic.Bool
}

func (h *shutdownHook) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			close(h.slowStarted)
			time.Sleep(time.Second)
			fmt.Fprint(w, "done")
			h.slowDone.Store(true)
		case "/stream":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (h *shutdownHook) Run(context.Context, *App) error { return nil }

func (h *shutdownHook) Stop() {
	h.stopped.Store(true)
	h.stoppedLate.Store(h.slowDone.Load())
}

func pidDead(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	// An orphan killed where PID 1 does not reap stays a zombie: dead.
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	i := strings.LastIndexByte(string(data), ')')
	return i >= 0 && i+2 < len(data) && data[i+2] == 'Z'
}

func siteState(t *testing.T, root, domain string) runtime.State {
	t.Helper()
	var st runtime.State
	data, err := os.ReadFile(runtime.StatePath(filepath.Join(root, "data"), domain))
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A SIGTERM used to make Run return the moment Shutdown was called: no
// request drained, the hooks wrote what they keep while requests were still
// running, and a handover in its drain was abandoned with the old
// generation running outside every state file. Run now returns after the
// server has drained (an endless stream cut, not waited out) and after the
// engine has finished the handover, and only then stops the hooks.
func TestAShutdownDrainsRequestsAndFinishesTheHandover(t *testing.T) {
	defer func(a, b time.Duration) { ShutdownTimeout, StreamsCutAfter = a, b }(ShutdownTimeout, StreamsCutAfter)
	ShutdownTimeout, StreamsCutAfter = 8*time.Second, 300*time.Millisecond
	t.Setenv("MIDDLEWARE_LISTEN", "127.0.0.1:0")
	root := t.TempDir()
	a, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sites", "drain.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".current"), []byte("one\n"), 0o644)
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":".current"},"drain":"60s","health":{"timeout":"10s"}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	hook := &shutdownHook{slowStarted: make(chan struct{})}
	a.Hooks = []Hook{hook}
	events, unsub := a.Bus.Subscribe()
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()

	var addr string
	for addr == "" {
		select {
		case ev := <-events:
			if ev.Type == "middleware.started" {
				addr, _ = ev.Data["listen"].(string)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the app never started listening")
		}
	}
	waitSite := func(cond func(port int) bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if sn, ok := a.Engine.Lookup("drain.test"); ok && sn.Alive && cond(sn.Port) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("the site never got there")
	}
	waitSite(func(int) bool { return true })
	old := siteState(t, root, "drain.test")
	os.WriteFile(filepath.Join(dir, ".current"), []byte("two\n"), 0o644)
	waitSite(func(port int) bool { return port != old.Port })
	next := siteState(t, root, "drain.test")
	t.Cleanup(func() {
		for _, pid := range []int{old.PID, next.PID} {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})

	// A stream that never ends by itself, and a request that takes a
	// second, both in flight when the signal comes.
	stream, err := http.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	slow := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			slow <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		slow <- string(b)
	}()
	<-hook.slowStarted
	began := time.Now()
	cancel()
	select {
	case err := <-ran:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(ShutdownTimeout + 5*time.Second):
		t.Fatal("Run did not return")
	}
	took := time.Since(began)
	if got := <-slow; got != "done" {
		t.Fatalf("the request in flight was cut: %q", got)
	}
	if !hook.stopped.Load() || !hook.stoppedLate.Load() {
		t.Fatal("the hooks were stopped while a request was still running")
	}
	if took > ShutdownTimeout-time.Second {
		t.Fatalf("Run took %s: the stream held the shutdown", took)
	}
	if !pidDead(old.PID) {
		t.Fatal("Run returned with the old generation still running, outside every state file")
	}
	if pidDead(next.PID) {
		t.Fatal("the new generation did not outlive the supervisor")
	}
	if st := siteState(t, root, "drain.test"); st.PID != next.PID {
		t.Fatalf("the state is not the new generation's: %+v", st)
	}
}

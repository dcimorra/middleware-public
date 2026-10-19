package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/ports"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// TestMain lets the test binary double as the site under supervision: with
// MW_TEST_SERVER set it is a small HTTP server that answers its revision on
// every path, so the handover test needs no other binary on the machine.
func TestMain(m *testing.M) {
	if os.Getenv("MW_TEST_SERVER") != "" {
		serveAsSite()
		return
	}
	os.Exit(m.Run())
}

func serveAsSite() {
	rev := os.Getenv("BUILD")
	if os.Getenv("MW_TEST_CRASH") != "" {
		os.Exit(7)
	}
	if d := os.Getenv("MW_TEST_SLOW_START"); d != "" {
		dur, _ := time.ParseDuration(d)
		time.Sleep(dur)
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
	if err != nil {
		os.Exit(1)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond) // long enough for a drain to matter
		fmt.Fprint(w, rev)
	})}
	_ = srv.Serve(l)
}

func newTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	sites := filepath.Join(root, "sites")
	data := filepath.Join(root, "data")
	os.MkdirAll(sites, 0o755)
	e := New(Options{SitesDir: sites, DataDir: data, Ports: ports.New(41000, 41100), Bus: events.New(""), Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
	return e, sites
}

func writeSite(t *testing.T, sites, domain, build string, extraEnv string) string {
	t.Helper()
	dir := filepath.Join(sites, domain)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, ".current"), []byte(build+"\n"), 0o644)
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"%s},"env_from_file":{"BUILD":".current"},"drain":"300ms","health":{"timeout":"10s"}}}`, os.Args[0], extraEnv)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	return dir
}

func waitFor(t *testing.T, e *Engine, domain string, cond func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if sn, ok := e.Lookup(domain); ok && cond(sn) {
			return sn
		}
		time.Sleep(50 * time.Millisecond)
	}
	sn, _ := e.Lookup(domain)
	t.Fatalf("condition not met for %s: %+v", domain, sn)
	return sn
}

func get(port int) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return string(b[:n]), nil
}

func TestHandoverDropsNoRequests(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "a.test", "build-1", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "a.test", func(s Snapshot) bool { return s.Alive })

	var failed, total atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sn, _ := e.Lookup("a.test")
				total.Add(1)
				if _, err := get(sn.Port); err != nil {
					failed.Add(1)
				}
			}
		}()
	}
	// Two deployments by pointer while the load runs.
	for _, b := range []string{"build-2", "build-3"} {
		time.Sleep(500 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, ".current"), []byte(b+"\n"), 0o644)
		waitFor(t, e, "a.test", func(s Snapshot) bool {
			if !s.Alive || s.Handing {
				return false
			}
			body, err := get(s.Port)
			return err == nil && body == b
		})
	}
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	if total.Load() < 50 {
		t.Fatalf("the load generator barely ran: %d requests", total.Load())
	}
	if f := failed.Load(); f != 0 {
		t.Fatalf("%d of %d requests failed across two handovers", f, total.Load())
	}
}

func TestACrashingSiteIsRestartedWithBackoff(t *testing.T) {
	e, sites := newTestEngine(t)
	writeSite(t, sites, "b.test", "x", `,"MW_TEST_CRASH":"1"`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	waitFor(t, e, "b.test", func(s Snapshot) bool { return s.Error != "" })
	time.Sleep(2 * time.Second)
	e.mu.RLock()
	failures := e.sites["b.test"].failures
	e.mu.RUnlock()
	if failures < 2 || failures > 4 {
		t.Fatalf("expected a backed-off retry or two in 2s, got %d failures", failures)
	}
}

func TestAHoldStopsTheInstanceAndRemovingItStartsAgain(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "c.test", "x", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	first := waitFor(t, e, "c.test", func(s Snapshot) bool { return s.Alive })
	m := fmt.Sprintf(`{"version":2,"hold":{"enabled":true},"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1"},"env_from_file":{"BUILD":".current"}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	waitFor(t, e, "c.test", func(s Snapshot) bool { return !s.Alive && s.Mode == "hold" })
	if _, err := get(first.Port); err == nil {
		t.Fatal("the old instance still answers")
	}
	writeSite(t, sites, "c.test", "x", "")
	waitFor(t, e, "c.test", func(s Snapshot) bool { return s.Alive })
}

func TestAFailedStartKeepsTheOldInstanceServing(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeSite(t, sites, "d.test", "x", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	first := waitFor(t, e, "d.test", func(s Snapshot) bool { return s.Alive })
	m := fmt.Sprintf(`{"version":2,"run":{"command":[%q],"env":{"MW_TEST_SERVER":"1","MW_TEST_CRASH":"1"},"env_from_file":{"BUILD":".current"}}}`, os.Args[0])
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	waitFor(t, e, "d.test", func(s Snapshot) bool { return s.Error != "" && s.Alive })
	if body, err := get(first.Port); err != nil || body != "x" {
		t.Fatalf("old instance not serving: %q %v", body, err)
	}
}

func TestInstancesAreReAdoptedByANewEngine(t *testing.T) {
	e, sites := newTestEngine(t)
	writeSite(t, sites, "e.test", "x", "")
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	first := waitFor(t, e, "e.test", func(s Snapshot) bool { return s.Alive })
	cancel()
	time.Sleep(300 * time.Millisecond)

	e2 := New(Options{SitesDir: e.opts.SitesDir, DataDir: e.opts.DataDir, Ports: ports.New(41000, 41100), Bus: events.New(""), Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go e2.Run(ctx2)
	sn := waitFor(t, e2, "e.test", func(s Snapshot) bool { return s.Alive })
	if sn.Port != first.Port {
		t.Fatalf("adopted on port %d, was %d: it was restarted instead of adopted", sn.Port, first.Port)
	}
	e.mu.RLock()
	inst := e.sites["e.test"].Instance
	e.mu.RUnlock()
	inst.Stop(context.Background(), time.Second)
}

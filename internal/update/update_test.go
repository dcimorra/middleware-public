package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/release"
)

func fakeGitHub(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	old := release.APIBase
	release.APIBase = srv.URL
	t.Cleanup(func() { release.APIBase = old })
}

const releases = `[
 {"tag_name":"v2.0.0-alpha.10","assets":[{"name":"middleware-linux-amd64"}]},
 {"tag_name":"v2.0.0-alpha.11","draft":true,"assets":[{"name":"middleware-linux-amd64"}]},
 {"tag_name":"v2.0.0-alpha.12","assets":[{"name":"middleware-linux-arm64"}]},
 {"tag_name":"v2.0.0-alpha.9","assets":[{"name":"middleware-linux-amd64"}]},
 {"tag_name":"v1.9.0","assets":[{"name":"middleware-linux-amd64"}]}
]`

func TestTheNewestPublishedReleaseForThisMachineIsOffered(t *testing.T) {
	fakeGitHub(t, releases)
	u := New(Options{Current: "v2.0.0-alpha.5", Arch: "amd64", Install: func(context.Context, string) error { return nil }})
	st := u.Check(context.Background(), true)
	// Not the draft, not the one without an amd64 binary; alpha.10 is
	// after alpha.9 although it sorts before it as text.
	if st.Error != "" || st.Latest != "v2.0.0-alpha.10" || !st.Available || !st.CanInstall {
		t.Fatalf("%+v", st)
	}
	// Who runs a stable release is offered stable ones only.
	stable := New(Options{Current: "v1.8.0", Arch: "amd64"})
	if st := stable.Check(context.Background(), true); st.Latest != "v1.9.0" {
		t.Fatalf("stable: %+v", st)
	}
}

func TestInstallAsksTheHelperForTheNewestAndRestarts(t *testing.T) {
	fakeGitHub(t, releases)
	var asked string
	restarted := make(chan struct{}, 1)
	u := New(Options{Current: "v2.0.0-alpha.5", Arch: "amd64",
		Install: func(_ context.Context, v string) error { asked = v; return nil },
		Restart: func() { restarted <- struct{}{} }})
	v, err := u.Install(context.Background(), "operator")
	if err != nil || v != "v2.0.0-alpha.10" || asked != v {
		t.Fatalf("%q %v asked %q", v, err, asked)
	}
	<-restarted
	// Nothing newer: refused, the helper is not asked.
	current := New(Options{Current: "v2.0.0-alpha.10", Arch: "amd64", Install: func(context.Context, string) error { t.Fatal("asked"); return nil }})
	if _, err := current.Install(context.Background(), "operator"); err != ErrNothingNewer {
		t.Fatalf("%v", err)
	}
}

func TestADevBuildOrNoHelperCannotInstall(t *testing.T) {
	fakeGitHub(t, releases)
	if st := New(Options{Current: "dev", Install: func(context.Context, string) error { return nil }}).Status(); st.CanInstall {
		t.Fatal("dev can install")
	}
	if st := New(Options{Current: "v2.0.0-alpha.5", Install: func(context.Context, string) error { return nil }, Helper: func() bool { return false }}).Status(); st.CanInstall || st.Why == "" {
		t.Fatalf("no helper: %+v", st)
	}
	if st := New(Options{Current: "v2.0.0-alpha.5", Auto: func() bool { return false }}).Status(); st.Auto {
		t.Fatal("auto on by default")
	}
}

// A check that finds a release installs it with auto on; with auto off it
// says so once, however many checks find the same release.
func TestACheckInstallsOnlyWithAutoOnAndOtherwiseAnnouncesOnce(t *testing.T) {
	fakeGitHub(t, releases)
	bus := events.New("")
	ch, stop := bus.Subscribe()
	defer stop()
	auto := false
	var installed string
	u := New(Options{Current: "v2.0.0-alpha.5", Arch: "amd64", Bus: bus, Auto: func() bool { return auto },
		Every:   func() time.Duration { return 5 * time.Minute },
		Install: func(_ context.Context, v string) error { installed = v; return nil }})
	if st := u.Status(); st.EveryMinutes != 5 {
		t.Fatalf("interval %+v", st)
	}
	u.tick(context.Background())
	u.tick(context.Background())
	if installed != "" {
		t.Fatal("installed with auto off")
	}
	announced := 0
	for len(ch) > 0 {
		if e := <-ch; e.Type == "update.available" {
			announced++
		}
	}
	if announced != 1 {
		t.Fatalf("announced %d times", announced)
	}
	auto = true
	u.tick(context.Background())
	if installed != "v2.0.0-alpha.10" {
		t.Fatalf("auto on did not install: %q", installed)
	}
}

// clock is a time the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// An automatic install waits while a move runs, is looked at again a
// minute later and not an hour, and goes ahead once the move is over.
// The operator's button does not wait.
func TestAnAutomaticInstallWaitsForTheMovesAndTheButtonDoesNot(t *testing.T) {
	fakeGitHub(t, releases)
	bus := events.New("")
	ch, stop := bus.Subscribe()
	defer stop()
	var busy atomic.Bool
	busy.Store(true)
	installed := make(chan string, 4)
	clk := &clock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	opts := Options{Current: "v2.0.0-alpha.5", Arch: "amd64", Bus: bus, Now: clk.Now,
		Auto:    func() bool { return true },
		Every:   func() time.Duration { return time.Hour },
		Busy:    busy.Load,
		Install: func(_ context.Context, v string) error { installed <- v; return nil }}
	u := New(opts)
	u.poll = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); u.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// The first look, two minutes after the start, finds a move running.
	deadline := time.Now().Add(5 * time.Second)
	for u.Status().NextCheck.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("Run did not start")
		}
		time.Sleep(time.Millisecond)
	}
	clk.add(2 * time.Minute)
	waitFor(t, ch, "update.deferred")
	for u.Status().NextCheck != clk.Now().Add(DeferRetry) {
		if time.Now().After(deadline) {
			t.Fatalf("next check %v, want %v", u.Status().NextCheck, clk.Now().Add(DeferRetry))
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case v := <-installed:
		t.Fatalf("installed %s during a move", v)
	default:
	}
	// Still moving a minute later: deferred again, announced only once.
	clk.add(DeferRetry)
	for u.Status().NextCheck != clk.Now().Add(DeferRetry) {
		if time.Now().After(deadline) {
			t.Fatal("the second look did not happen")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case v := <-installed:
		t.Fatalf("installed %s during a move", v)
	default:
	}
	for len(ch) > 0 {
		if e := <-ch; e.Type == "update.deferred" {
			t.Fatal("announced twice")
		}
	}
	// The operator's button installs regardless.
	if v, err := New(opts).Install(context.Background(), "operator"); err != nil || v != "v2.0.0-alpha.10" {
		t.Fatalf("%q %v", v, err)
	}
	if v := <-installed; v != "v2.0.0-alpha.10" {
		t.Fatalf("installed %q", v)
	}
	// The move ends: the next look, a minute on, installs.
	busy.Store(false)
	clk.add(DeferRetry)
	select {
	case v := <-installed:
		if v != "v2.0.0-alpha.10" {
			t.Fatalf("installed %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not installed once the move ended")
	}
}

// waitFor reads events until one of typ arrives.
func waitFor(t *testing.T, ch <-chan events.Event, typ string) events.Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Type == typ {
				return e
			}
		case <-timeout:
			t.Fatalf("no %s", typ)
		}
	}
}

// The confirmation is what keeps a release installed, so it waits for
// the proof: the listener open, and a request answered through it.
func TestTheSupervisorConfirmsOnlyOnceItAnswersThroughItsListener(t *testing.T) {
	bus := events.New("")
	ch, stop := bus.Subscribe()
	defer stop()
	confirmed := make(chan string, 1)
	u := New(Options{Current: "v2.0.0-alpha.10", Bus: bus, Settle: 10 * time.Millisecond,
		Confirm: func(_ context.Context, v string) (Confirmation, error) {
			confirmed <- v
			return Confirmation{Confirmed: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.ConfirmWhenServing(ctx)
	select {
	case <-confirmed:
		t.Fatal("confirmed before the listener was open")
	case <-time.After(100 * time.Millisecond):
	}
	// Any answer is one: an unknown name's 404 crossed the whole chain.
	served := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- struct{}{}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	bus.Emit(events.Event{Type: "middleware.started", Data: map[string]any{"listen": srv.Listener.Addr().String()}})
	if v := <-confirmed; v != "v2.0.0-alpha.10" {
		t.Fatalf("confirmed %q", v)
	}
	<-served
	waitFor(t, ch, "update.confirmed")
}

func TestASupervisorThatDoesNotAnswerDoesNotConfirm(t *testing.T) {
	bus := events.New("")
	// A port nobody listens on.
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.Listener.Addr().String()
	srv.Close()
	bus.Emit(events.Event{Type: "middleware.started", Data: map[string]any{"listen": dead}})
	confirmed := make(chan string, 2)
	u := New(Options{Current: "v2.0.0-alpha.10", Bus: bus, Settle: time.Millisecond,
		Confirm: func(_ context.Context, v string) (Confirmation, error) {
			confirmed <- v
			return Confirmation{}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.ConfirmWhenServing(ctx)
	select {
	case <-confirmed:
		t.Fatal("confirmed without an answer")
	case <-time.After(300 * time.Millisecond):
	}
	// A dev build is never on trial and asks nothing.
	New(Options{Current: "dev", Bus: bus, Settle: time.Millisecond, Probe: func(context.Context, string) error { return nil },
		Confirm: func(_ context.Context, v string) (Confirmation, error) {
			confirmed <- v
			return Confirmation{}, nil
		}}).ConfirmWhenServing(ctx)
	select {
	case v := <-confirmed:
		t.Fatalf("a dev build confirmed %q", v)
	case <-time.After(100 * time.Millisecond):
	}
}

// After a rollback the release rolled back from is not offered: automatic
// updates would otherwise install it again at the next check.
func TestAReleaseRolledBackFromIsNotOfferedUntilANewerOne(t *testing.T) {
	fakeGitHub(t, releases)
	bus := events.New("")
	ch, stop := bus.Subscribe()
	defer stop()
	// Already started when the updater looks: the event is in Recent.
	bus.Emit(events.Event{Type: "middleware.started", Data: map[string]any{"listen": "127.0.0.1:1"}})
	installed := ""
	u := New(Options{Current: "v2.0.0-alpha.9", Arch: "amd64", Bus: bus, Settle: time.Millisecond,
		Auto:    func() bool { return true },
		Probe:   func(context.Context, string) error { return nil },
		Install: func(_ context.Context, v string) error { installed = v; return nil },
		Confirm: func(context.Context, string) (Confirmation, error) {
			return Confirmation{Skipped: "v2.0.0-alpha.10", RolledBack: "v2.0.0-alpha.10", Why: "it did not confirm"}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.ConfirmWhenServing(ctx)
	if e := waitFor(t, ch, "update.rolled_back"); e.Level != "error" {
		t.Fatalf("%+v", e)
	}
	u.tick(context.Background())
	if st := u.Status(); st.Available || st.Skipped != "v2.0.0-alpha.10" || installed != "" {
		t.Fatalf("%+v installed %q", st, installed)
	}
	if _, err := u.Install(context.Background(), "operator"); err != ErrNothingNewer {
		t.Fatalf("the skipped release was installed by hand: %v", err)
	}
	// A newer release is out: offered and installed.
	fakeGitHub(t, `[{"tag_name":"v2.0.0-alpha.11","assets":[{"name":"middleware-linux-amd64"}]}]`)
	u.tick(context.Background())
	if installed != "v2.0.0-alpha.11" {
		t.Fatalf("installed %q", installed)
	}
}

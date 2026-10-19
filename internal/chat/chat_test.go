package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/events"
)

// fakeProvider replays canned events and records the requests it got. A
// run blocks before its last event when hold is set, so a test can see two
// turns collide.
type fakeProvider struct {
	key    string
	script []agent.Event
	hold   chan struct{}
	mu     sync.Mutex
	reqs   []agent.Request
}

func (f *fakeProvider) Key() string               { return f.key }
func (f *fakeProvider) Available() (bool, string) { return true, "fake" }
func (f *fakeProvider) Models() []string          { return []string{"fake-1"} }

func (f *fakeProvider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	r := &fakeRun{ch: make(chan agent.Event), stop: make(chan struct{})}
	go func() {
		defer close(r.ch)
		for i, ev := range f.script {
			if i == len(f.script)-1 && f.hold != nil {
				select {
				case <-f.hold:
				case <-r.stop:
					r.ch <- agent.Event{Kind: agent.KindError, Text: "stopped"}
					return
				case <-ctx.Done():
					return
				}
			}
			select {
			case r.ch <- ev:
			case <-r.stop:
				return
			}
		}
	}()
	return r, nil
}

func (f *fakeProvider) last() agent.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

type fakeRun struct {
	ch   chan agent.Event
	stop chan struct{}
	once sync.Once
}

func (r *fakeRun) Events() <-chan agent.Event { return r.ch }
func (r *fakeRun) Stop()                      { r.once.Do(func() { close(r.stop) }) }

func newManager(t *testing.T, p *fakeProvider) (*Manager, string, *events.Bus) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"sites/a.test", "sites/b.test", "data", "tasks"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "sites", "a.test", "site.json"), []byte(`{"version":2,"agent":{"provider":"fake","model":"fake-1","monthly_cap_usd":1}}`), 0o644)
	bus := events.New("")
	reg := agent.NewRegistry()
	reg.Register(p)
	m := New(Options{Root: root, Bus: bus, Registry: reg, DefaultProvider: "fake"})
	return m, root, bus
}

func waitFor(t *testing.T, ch <-chan events.Event, typ string) events.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == typ {
				return ev
			}
			if ev.Type == "agent.error" && typ != "agent.error" {
				t.Fatalf("agent.error while waiting for %s: %s", typ, ev.Message)
			}
		case <-deadline:
			t.Fatalf("no %s event", typ)
		}
	}
}

func TestATurnStreamsEventsAndPersistsBothSidesWithTheSession(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindText, Text: "Reading.", SessionID: "s1"},
		{Kind: agent.KindTool, Tool: "Read", Input: map[string]any{"file_path": "README.md"}},
		{Kind: agent.KindText, Text: "Done."},
		{Kind: agent.KindCost, Cost: 0.25},
		{Kind: agent.KindResult, Text: "Done.", SessionID: "s1"},
	}}
	m, root, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	id, err := m.Send("a.test", "look around", "")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.text")
	waitFor(t, ch, "agent.tool")
	done := waitFor(t, ch, "agent.done")
	if done.Subject != "a.test" || done.Data["turn_id"] != id || done.Data["cost"] != 0.25 {
		t.Fatalf("done = %+v", done)
	}
	req := p.last()
	if req.Workdir != filepath.Join(root, "sites", "a.test") || req.Model != "fake-1" || req.SessionID != "" {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(req.System, "a.test") || !strings.Contains(req.System, filepath.Join(root, "sites", "b.test")) {
		t.Fatalf("preamble does not name the site and the denied sibling:\n%s", req.System)
	}
	if req.Jail.Home != filepath.Join(root, "data", "homes", "a.test") {
		t.Fatalf("home = %s", req.Jail.Home)
	}
	c, err := m.History("a.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Turns) != 2 || c.Turns[0].Role != "user" || c.Turns[1].Role != "assistant" || c.Turns[1].Text != "Reading.\n\nDone." || c.Turns[1].Cost != 0.25 || c.SessionID != "s1" || c.Running {
		t.Fatalf("conversation = %+v", c)
	}
	if len(c.Turns[1].Tools) != 1 || c.Turns[1].Tools[0] != "Read" {
		t.Fatalf("tools = %v", c.Turns[1].Tools)
	}
	// The second turn resumes the session.
	if _, err := m.Send("a.test", "again", "other-model"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if req := p.last(); req.SessionID != "s1" || req.Model != "other-model" {
		t.Fatalf("second request = %+v", req)
	}
	if got := m.opts.Ledger.Month("a.test", time.Now()); got < 0.49 || got > 0.51 {
		t.Fatalf("ledger month = %v", got)
	}
}

func TestOnlyOneTurnRunsPerSiteAtATimeAndStopEndsIt(t *testing.T) {
	hold := make(chan struct{})
	p := &fakeProvider{key: "fake", hold: hold, script: []agent.Event{
		{Kind: agent.KindText, Text: "working"},
		{Kind: agent.KindResult, Text: "ok"},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "one", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.text")
	if _, err := m.Send("a.test", "two", ""); err != ErrBusy {
		t.Fatalf("second send: %v", err)
	}
	if err := m.Reset("a.test"); err != ErrBusy {
		t.Fatalf("reset while running: %v", err)
	}
	// Another site is not blocked by this one.
	if _, err := m.Send("b.test", "hello", ""); err != nil {
		t.Fatalf("b.test blocked: %v", err)
	}
	if err := m.Stop("a.test"); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.error")
	if ev.Subject != "a.test" {
		t.Fatalf("error on %s", ev.Subject)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Running("a.test") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Running("a.test") {
		t.Fatal("still running after Stop")
	}
	c, _ := m.History("a.test")
	if last := c.Turns[len(c.Turns)-1]; last.Role != "error" {
		t.Fatalf("last turn = %+v", last)
	}
	close(hold)
	waitFor(t, ch, "agent.done")
}

func TestTheMonthlyCapRefusesANewTurn(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Cost: 1.5}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "spend", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	_, err := m.Send("a.test", "more", "")
	if _, ok := err.(*agent.ErrOverCap); !ok {
		t.Fatalf("want ErrOverCap, got %v", err)
	}
	waitFor(t, ch, "agent.cost_cap")
}

func TestAJailViolationIsReportedAfterTheTurn(t *testing.T) {
	m, root, bus := newManager(t, &fakeProvider{key: "fake"})
	other := filepath.Join(root, "sites", "b.test", ".env")
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindTool, Tool: "Read", Input: map[string]any{"file_path": other}},
		{Kind: agent.KindResult, Text: "peeked"},
	}}
	m.opts.Registry.Register(p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "peek", ""); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.jail_violation")
	paths, _ := ev.Data["paths"].([]string)
	if ev.Level != "error" || len(paths) != 1 || paths[0] != filepath.Join(root, "sites", "b.test") {
		t.Fatalf("violation = %+v", ev)
	}
}

func TestTheMiddlewareConversationRunsInTheRootAndDeniesTheSites(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send(Middleware, "what is running?", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	req := p.last()
	if req.Workdir != root || len(req.Jail.Deny) != 1 || req.Jail.Deny[0] != filepath.Join(root, "sites") {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(req.System, "installation itself") {
		t.Fatalf("preamble:\n%s", req.System)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "chat", "middleware.json")); err != nil {
		t.Fatal(err)
	}
}

func TestResetForgetsTheSessionAndUnknownDomainsAreRefused(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok", SessionID: "s9"}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "hi", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if c, _ := m.History("a.test"); c.SessionID != "s9" {
		t.Fatalf("session = %q", c.SessionID)
	}
	if err := m.Reset("a.test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "chat.reset")
	c, err := m.History("a.test")
	if err != nil || len(c.Turns) != 0 || c.SessionID != "" || c.Provider != "fake" {
		t.Fatalf("after reset: %+v err=%v", c, err)
	}
	for _, bad := range []string{"nope.test", "../etc", "A.TEST", ""} {
		if _, err := m.Send(bad, "hi", ""); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, err := m.Send("a.test", "   ", ""); err == nil {
		t.Fatal("empty message accepted")
	}
}

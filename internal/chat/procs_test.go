package chat

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// What a turn leaves behind with setsid escapes the group kill. The mark in
// its environment is how the house still lists it and ends it, and a pid
// with another conversation's mark (or none) is refused.
func TestWhatATurnLeftBehindIsListedAndReaped(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "hi", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if p.last().Env[MarkVar] != "a.test" {
		t.Fatalf("the turn's environment has no mark: %v", p.last().Env)
	}
	start := func(mark string) *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		cmd.Env = append(os.Environ(), MarkVar+"="+mark)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go cmd.Wait()
		t.Cleanup(func() { cmd.Process.Kill() })
		return cmd
	}
	ours := start("a.test")
	theirs := start("b.test")
	// a.test.example would match a prefix search; the mark is whole.
	near := start("a.test.example")
	time.Sleep(100 * time.Millisecond)
	procs := m.Processes("a.test")
	if len(procs) != 1 || procs[0].PID != ours.Process.Pid {
		t.Fatalf("listed %+v", procs)
	}
	if m.Kill("a.test", theirs.Process.Pid) {
		t.Fatal("killed another conversation's process")
	}
	if n := m.Reap("a.test"); n != 1 {
		t.Fatalf("reaped %d", n)
	}
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(ours.Process.Pid, 0) == nil && !zombie(ours.Process.Pid) {
		t.Fatal("still alive after the reap")
	}
	for _, c := range []*exec.Cmd{theirs, near} {
		if syscall.Kill(c.Process.Pid, 0) != nil {
			t.Fatal("a process that was not this conversation's was killed")
		}
	}
}

func TestATurnIsCutAtItsLimit(t *testing.T) {
	defer func(d time.Duration) { TurnLimit = d }(TurnLimit)
	TurnLimit = 300 * time.Millisecond
	hold := make(chan struct{})
	defer close(hold)
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindText, Text: "working"}, {Kind: agent.KindResult}}, hold: hold}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "loop forever", ""); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, "agent.turn_limit")
	if ev.Subject != "a.test" {
		t.Fatalf("%+v", ev)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Running("a.test") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Running("a.test") {
		t.Fatal("the turn still holds its slot")
	}
}

// A lent folder is readable and not writable: it leaves the deny list and
// the adapter closes it to every writing tool.
func TestALentFolderIsReadNotWritten(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	os.WriteFile(root+"/sites/a.test/site.json", []byte(`{"version":2,"chat":{"reads":["b.test","missing.test"]}}`), 0o644)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "read b", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	j := p.last().Jail
	lent := root + "/sites/b.test"
	if len(j.Lent) != 1 || j.Lent[0] != lent {
		t.Fatalf("lent: %v", j.Lent)
	}
	for _, d := range j.Deny {
		if d == lent {
			t.Fatal("the lent folder is still denied")
		}
	}
}

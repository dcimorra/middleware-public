package chat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/agent/claude"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// A turn in flight when the supervisor restarts is picked up by the next
// one: still running, it is followed to its end; finished meanwhile, its
// output is read to its result. Both end as written turns.
func TestTurnsInFlightSurviveARestart(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"sites/a.test", "sites/b.test", "data"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	lines := func(text string) string {
		return fmt.Sprintf(`{"type":"system","subtype":"init","session_id":"s-%[1]s"}
{"type":"assistant","message":{"content":[{"type":"text","text":%[2]q}]}}
{"type":"result","subtype":"success","result":%[2]q,"total_cost_usd":0.01,"session_id":"s-%[1]s"}
`, text[:1], text)
	}
	// The "CLI" a dead supervisor started: it writes half, waits, writes
	// the rest. Started by us, as it would have been by the old process.
	streamA := filepath.Join(root, "data", "chat", "streams", "a.test-t1.out")
	os.MkdirAll(filepath.Dir(streamA), 0o700)
	full := lines("alive and answered")
	first, rest, _ := strings.Cut(full, "\n")
	script := filepath.Join(root, "cli.sh")
	os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s'\nsleep 1\nprintf '%%s' '%s'\n", first, rest)), 0o755)
	out, _ := os.Create(streamA)
	cmd := exec.Command(script)
	cmd.Stdout = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	out.Close()
	go cmd.Wait()
	// And one that already finished while nobody listened.
	streamB := filepath.Join(root, "data", "chat", "streams", "b.test-t2.out")
	os.WriteFile(streamB, []byte(lines("finished while away")), 0o600)
	now := time.Now()
	fsx.WriteJSON(filepath.Join(root, "data", "chat", "running", "a.test.json"), runningFile{Name: "a.test", TurnID: "t1", PID: cmd.Process.Pid, Stream: streamA, Provider: "claude", Started: now}, 0o600)
	fsx.WriteJSON(filepath.Join(root, "data", "chat", "running", "b.test.json"), runningFile{Name: "b.test", TurnID: "t2", PID: 999999, Stream: streamB, Provider: "claude", Started: now}, 0o600)

	reg := agent.NewRegistry()
	reg.Register(claude.New(claude.Config{Bin: "true"}))
	bus := events.New("")
	m := New(Options{Root: root, Bus: bus, Registry: reg})
	ch, unsub := bus.Subscribe()
	defer unsub()
	m.Recover(context.Background())
	done := map[string]bool{}
	deadline := time.After(15 * time.Second)
	for len(done) < 2 {
		select {
		case ev := <-ch:
			if ev.Type == "agent.done" {
				done[ev.Subject] = true
			}
			if ev.Type == "agent.error" {
				t.Fatalf("%s: %s", ev.Subject, ev.Message)
			}
		case <-deadline:
			t.Fatalf("recovered only %v", done)
		}
	}
	for name, want := range map[string]string{"a.test": "alive and answered", "b.test": "finished while away"} {
		c, _ := m.History(name)
		last := c.Turns[len(c.Turns)-1]
		if last.Role != "assistant" || last.Text != want || c.SessionID == "" {
			t.Fatalf("%s: %+v", name, c)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "data", "chat", "running")); len(ents) != 0 {
		t.Fatal("running files were left behind")
	}
}

package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
)

func TestEachAgentResumesItsOwnSessionAndHearsWhatTheOtherSaid(t *testing.T) {
	claude := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "soy claude", SessionID: "claude-1"}}}
	codex := &fakeProvider{key: "other", script: []agent.Event{{Kind: agent.KindResult, Text: "soy codex", SessionID: "codex-1"}}}
	m, root, bus := newManager(t, claude)
	m.opts.Registry.Register(codex)
	ch, unsub := bus.Subscribe()
	defer unsub()
	manifest := func(provider string) {
		os.WriteFile(filepath.Join(root, "sites", "a.test", "site.json"), []byte(`{"version":2,"agent":{"provider":"`+provider+`"}}`), 0o644)
	}
	sendAndWait := func(text string) {
		t.Helper()
		send(t, m, "a.test", text, ch)
	}

	sendAndWait("uno")
	manifest("other")
	sendAndWait("dos")
	if r := codex.last(); r.SessionID != "" || !strings.Contains(r.Prompt, "soy claude") || !strings.HasSuffix(r.Prompt, "dos") {
		t.Fatalf("codex resumed %q with %q", r.SessionID, r.Prompt)
	}
	if r := codex.last(); r.Model != "" {
		t.Fatalf("codex was handed claude's model %q", r.Model)
	}
	manifest("fake")
	sendAndWait("tres")
	if r := claude.last(); r.SessionID != "claude-1" || !strings.Contains(r.Prompt, "soy codex") || strings.Contains(r.Prompt, "<Agent (fake)>") {
		t.Fatalf("claude resumed %q with %q", r.SessionID, r.Prompt)
	}
	// Back with the same agent: its own session, nothing to catch up on.
	sendAndWait("cuatro")
	if r := claude.last(); r.SessionID != "claude-1" || r.Prompt != "cuatro" {
		t.Fatalf("claude resumed %q with %q", r.SessionID, r.Prompt)
	}
}

func TestAMessageCanNameItsAgent(t *testing.T) {
	claude := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "soy claude", SessionID: "claude-1"}}}
	codex := &fakeProvider{key: "other", script: []agent.Event{{Kind: agent.KindResult, Text: "soy codex", SessionID: "codex-1"}}}
	m, _, bus := newManager(t, claude)
	m.opts.Registry.Register(codex)
	ch, unsub := bus.Subscribe()
	defer unsub()
	send(t, m, "a.test", "uno", ch)
	if _, err := m.SendWith("a.test", "dos", SendOptions{Provider: "other"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if r := codex.last(); r.Model != "" || !strings.HasSuffix(r.Prompt, "dos") {
		t.Fatalf("%+v", r)
	}
	if c, _ := m.Conversation("a.test"); c.Provider != "other" || c.Sessions["fake"] != "claude-1" || c.Sessions["other"] != "codex-1" {
		t.Fatalf("%+v", c)
	}
}

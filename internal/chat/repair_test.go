package chat

import (
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// The automatic repairs run in a conversation of their own, the same one
// every time, and without the push settings the panel's gets: their
// prompt quotes the site's log.
func TestRepairsRunApartWithoutThePushSettings(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "Done: x", SessionID: "s1"}}}
	m, _, bus := newManager(t, p)
	m.SetTurnEnv(func(domain string) map[string]string { return map[string]string{"GIT_CONFIG_COUNT": "1"} })
	ch, unsub := bus.Subscribe()
	defer unsub()
	name, err := m.RepairConversation("a.test")
	if err != nil {
		t.Fatal(err)
	}
	if d, id := split(name); d != "a.test" || id == "" {
		t.Fatalf("the repairs run in %q", name)
	}
	if again, _ := m.RepairConversation("a.test"); again != name {
		t.Fatalf("a second repair opened another conversation: %q, %q", name, again)
	}
	if _, err := m.Send(name, "repair it", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	req := p.last()
	if _, ok := req.Env["GIT_CONFIG_COUNT"]; ok {
		t.Fatalf("a repair got the push settings: %v", req.Env)
	}
	if !strings.Contains(req.System, "`git push` is not available") {
		t.Fatalf("the repair is not told it cannot push:\n%s", req.System)
	}
	if c, _ := m.History("a.test"); len(c.Turns) != 0 {
		t.Fatal("the repair went into the operator's conversation")
	}
	if again, _ := m.RepairConversation("a.test"); again != name {
		t.Fatalf("after a turn, another conversation: %q", again)
	}
	if _, err := m.RepairConversation("missing.test"); err == nil {
		t.Fatal("a repair conversation was opened for a site that does not exist")
	}
}

package chat

import (
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
)

func TestWhatATurnSaysOfItsLimitsGoesToTheBookAndNotToTheConversation(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindLimits, Limits: []agent.Window{{Name: "5h", Used: 42, ResetsAt: reset, Status: "allowed"}}},
		{Kind: agent.KindResult, Text: "ok"},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "hi", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	u, ok := m.Usage().Get("fake")
	if !ok || len(u.Windows) != 1 || u.Windows[0].Used != 42 || !u.Windows[0].ResetsAt.Equal(reset) || u.Source != "turn" {
		t.Fatalf("%+v", u)
	}
}

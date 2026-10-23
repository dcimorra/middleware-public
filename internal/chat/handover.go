package chat

import (
	"strings"
)

// A conversation can change agent between two messages: Claude, then
// Codex, then Claude again. Each keeps its own session (Conversation.
// Sessions), and the one that comes in is told what was said while it was
// away, since its own session never saw it.

// catchUpTurns and catchUpChars bound what is handed over: the last turns,
// clipped, enough to pick up the thread without filling the context.
const (
	catchUpTurns = 12
	catchUpChars = 12000
)

// sessionFor is the session provider resumes in c, and what it has to be
// told first: the turns since it last answered, or the whole conversation
// when it never did.
func sessionFor(c Conversation, provider string) (session, catchUp string) {
	session = c.Sessions[provider]
	if session == "" && c.Sessions == nil && c.Provider == provider {
		// Written before Sessions: its one session is its last agent's.
		session = c.SessionID
	}
	// The turns since this agent last answered.
	from := 0
	for i := len(c.Turns) - 1; i >= 0; i-- {
		t := c.Turns[i]
		if t.Role != "user" && agentOf(c, t) == provider {
			from = i + 1
			break
		}
	}
	missed := c.Turns[from:]
	if session == "" && from == 0 && len(c.Sessions) == 0 && c.SessionID == "" {
		// A new conversation: nothing happened without it.
		return "", ""
	}
	var others bool
	for _, t := range missed {
		if t.Role != "user" && agentOf(c, t) != provider {
			others = true
		}
	}
	if !others {
		return session, ""
	}
	if len(missed) > catchUpTurns {
		missed = missed[len(missed)-catchUpTurns:]
	}
	var b strings.Builder
	b.WriteString("This conversation went on with another agent while you were not in it. What was said since you last answered, for context (the operator's next message comes after it):\n\n")
	for _, t := range missed {
		who := "Operator"
		if t.Role != "user" {
			who = "Agent (" + agentOf(c, t) + ")"
		}
		text := strings.TrimSpace(t.Text)
		if r := []rune(text); len(r) > 2000 {
			text = string(r[:2000]) + " […]"
		}
		b.WriteString("<" + who + ">\n" + text + "\n\n")
	}
	out := b.String()
	if r := []rune(out); len(r) > catchUpChars {
		out = "[…]\n" + string(r[len(r)-catchUpChars:])
	}
	return session, out + "---\n\n"
}

// agentOf is the agent that wrote a turn; turns written before turns
// carried it are the conversation's last agent's.
func agentOf(c Conversation, t Turn) string {
	if t.Provider != "" {
		return t.Provider
	}
	return c.Provider
}

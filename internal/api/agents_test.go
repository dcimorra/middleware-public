package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/chat"
)

// fakeProvider is a provider that is never available, which is all the
// listing needs; starting a run is the chat package's business.
type fakeProvider struct{ key string }

func (p fakeProvider) Key() string               { return p.key }
func (p fakeProvider) Available() (bool, string) { return false, p.key + " not installed" }
func (p fakeProvider) Models() []string          { return []string{"m1", "m2"} }
func (p fakeProvider) Start(context.Context, agent.Request) (agent.Run, error) {
	return nil, agent.ErrUnknownProvider
}

func TestAgentsAreListedAndTheConversationsAnswer(t *testing.T) {
	f := newFixture(t).withToken()
	reg := agent.NewRegistry()
	reg.Register(fakeProvider{key: "claude"})
	os.MkdirAll(filepath.Join(f.root, "sites", "x.test"), 0o755)
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg})
	f.srv.RegisterAgents(AgentsOptions{Registry: reg, Chat: m})

	resp, body := f.call("GET", "/api/v1/agents", nil)
	if resp.StatusCode != 200 || text(body) == "" {
		t.Fatalf("agents: %d %s", resp.StatusCode, text(body))
	}
	resp, body = f.call("GET", "/api/v1/chat/middleware", nil)
	if resp.StatusCode != 200 || body["domain"] != chat.Middleware || body["provider"] != "claude" {
		t.Fatalf("middleware conversation: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/sites/x.test/chat", nil)
	if resp.StatusCode != 200 || body["domain"] != "x.test" {
		t.Fatalf("site conversation: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("GET", "/api/v1/sites/nope.test/chat", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown site: %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/sites/x.test/chat", map[string]string{"text": ""})
	if resp.StatusCode != 400 {
		t.Fatalf("empty message: %d", resp.StatusCode)
	}
	// The provider cannot run, so the turn is refused before it starts.
	resp, body = f.call("POST", "/api/v1/sites/x.test/chat", map[string]string{"text": "hello"})
	if resp.StatusCode == 200 {
		t.Fatalf("a turn without a runnable provider must not start: %v", body)
	}
	resp, _ = f.call("POST", "/api/v1/sites/x.test/chat/stop", nil)
	if resp.StatusCode != 409 {
		t.Fatalf("stop with nothing running: %d", resp.StatusCode)
	}
	resp, _ = f.call("DELETE", "/api/v1/chat/middleware", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
}

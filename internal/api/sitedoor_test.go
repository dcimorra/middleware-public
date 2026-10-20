package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/chat"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/sitetoken"
)

// answeringProvider answers every turn with a fixed text and cost, and
// records what it was asked to run with.
type answeringProvider struct {
	answer string
	mu     sync.Mutex
	reqs   []agent.Request
}

func (p *answeringProvider) Key() string               { return "claude" }
func (p *answeringProvider) Available() (bool, string) { return true, "fake" }
func (p *answeringProvider) Models() []string          { return []string{"sonnet"} }
func (p *answeringProvider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	ch := make(chan agent.Event, 2)
	ch <- agent.Event{Kind: agent.KindText, Text: p.answer, SessionID: "s1"}
	ch <- agent.Event{Kind: agent.KindResult, Cost: 0.25}
	close(ch)
	return chanRun(ch), nil
}
func (p *answeringProvider) last() agent.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reqs[len(p.reqs)-1]
}

type chanRun chan agent.Event

func (r chanRun) Events() <-chan agent.Event { return r }
func (r chanRun) Stop()                      {}

func doorFixture(t *testing.T, manifests map[string]string) (*fixture, *sitetoken.Keys, *answeringProvider, *chat.Manager) {
	f := newFixture(t).withToken()
	for d, m := range manifests {
		os.MkdirAll(filepath.Join(f.root, "sites", d), 0o755)
		os.WriteFile(filepath.Join(f.root, "sites", d, "site.json"), []byte(m), 0o644)
	}
	p := &answeringProvider{answer: `{"ok": true}`}
	reg := agent.NewRegistry()
	reg.Register(p)
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg})
	keys := sitetoken.New(filepath.Join(f.root, "data"))
	f.srv.RegisterSiteDoor(SiteDoorOptions{
		Chat: m, Tokens: keys,
		Lookup: func(d string) (*manifest.Manifest, bool) {
			mf, err := manifest.Load(filepath.Join(f.root, "sites", d))
			return mf, err == nil
		},
		HouseDaily: func() float64 { return 1 },
		Spent:      m.Ledger().Day,
	})
	return f, keys, p, m
}

func (f *fixture) asSite(keys *sitetoken.Keys, domain, method, path string, body any) (int, map[string]any) {
	tok, _ := keys.Token(domain)
	saved := f.token
	f.token = tok
	defer func() { f.token = saved }()
	resp, out := f.call(method, path, body)
	return resp.StatusCode, out
}

func TestTheSiteDoorAnswersItsSiteWithTheLevelItsManifestSays(t *testing.T) {
	f, keys, p, _ := doorFixture(t, map[string]string{
		"open.test":   `{"version":2,"claude_api":{"tools":"read","model":"haiku"}}`,
		"closed.test": `{"version":2}`,
	})
	if code, _ := f.asSite(keys, "closed.test", "GET", "/api/v1/site", nil); code != 403 {
		t.Fatalf("a site without the door got %d", code)
	}
	code, me := f.asSite(keys, "open.test", "GET", "/api/v1/site", nil)
	if code != 200 || me["domain"] != "open.test" {
		t.Fatalf("me: %d %v", code, me)
	}
	code, out := f.asSite(keys, "open.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "is it ok?", "format": "json"})
	if code != 200 || out["text"] != `{"ok": true}` || out["json"].(map[string]any)["ok"] != true {
		t.Fatalf("ask: %d %v", code, out)
	}
	req := p.last()
	if req.Model != "haiku" || strings.Join(req.Tools, ",") != "Read,Glob,Grep" || !strings.Contains(strings.Join(req.Jail.Secrets, ","), ".env") {
		t.Fatalf("the turn ran with model %q tools %v secrets %v", req.Model, req.Tools, req.Jail.Secrets)
	}
	if !strings.Contains(req.System, "program, not a person") || strings.Contains(req.System, "question") {
		t.Fatalf("the site's turn got the panel's preamble:\n%s", req.System)
	}
	// Its conversation is listed and continued; the panel's is never
	// reachable from here, nor another site's.
	id := out["id"].(string)
	code, list := f.callList(keys, "open.test", "/api/v1/site/claude/chats")
	if code != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}
	if code, _ := f.asSite(keys, "open.test", "POST", "/api/v1/site/claude/chats/"+id+"/messages", map[string]any{"prompt": "and now?"}); code != 200 {
		t.Fatalf("continue: %d", code)
	}
	if p.last().SessionID != "s1" {
		t.Fatal("the second turn did not resume the first")
	}
	if code, _ := f.asSite(keys, "closed.test", "GET", "/api/v1/site/claude/chats/"+id, nil); code != 403 {
		t.Fatalf("another site's token read the conversation: %d", code)
	}
	if code, _ := f.asSite(keys, "open.test", "GET", "/api/v1/site/claude/chats/ffff", nil); code != 404 {
		t.Fatalf("an unknown conversation: %d", code)
	}
	// A forged token is refused, slowly.
	start := time.Now()
	f.token = "open.test.forged"
	if resp, _ := f.call("GET", "/api/v1/site", nil); resp.StatusCode != 401 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("forged token: %d after %s", resp.StatusCode, time.Since(start))
	}
}

func TestTheDailyCapIsCheckedBeforeTheTurn(t *testing.T) {
	f, keys, _, _ := doorFixture(t, map[string]string{"a.test": `{"version":2,"claude_api":{"daily_usd":0.3}}`})
	if code, _ := f.asSite(keys, "a.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "one"}); code != 200 {
		t.Fatalf("first: %d", code)
	}
	if code, _ := f.asSite(keys, "a.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "two"}); code != 200 {
		t.Fatalf("second: %d", code)
	}
	code, out := f.asSite(keys, "a.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "three"})
	if code != 429 || !strings.Contains(text(out), "daily_usd") {
		t.Fatalf("over the cap: %d %v", code, out)
	}
}

func TestNoneMeansNoToolsAndWriteMeansTheJail(t *testing.T) {
	f, keys, p, _ := doorFixture(t, map[string]string{
		"n.test": `{"version":2,"claude_api":{}}`,
		"w.test": `{"version":2,"claude_api":{"tools":"write"}}`,
	})
	f.asSite(keys, "n.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x"})
	if r := p.last(); r.Tools == nil || len(r.Tools) != 0 || strings.Join(r.Disallowed, ",") != "Task" {
		t.Fatalf("none: tools %#v disallowed %v", r.Tools, r.Disallowed)
	}
	f.asSite(keys, "w.test", "POST", "/api/v1/site/claude/ask", map[string]any{"prompt": "x"})
	if r := p.last(); r.Tools != nil || len(r.Jail.Secrets) != 0 {
		t.Fatalf("write: tools %#v secrets %v", r.Tools, r.Jail.Secrets)
	}
}

func (f *fixture) callList(keys *sitetoken.Keys, domain, path string) (int, []any) {
	tok, _ := keys.Token(domain)
	saved := f.token
	f.token = tok
	defer func() { f.token = saved }()
	resp, out := f.call("GET", path, nil)
	var list []any
	_ = json.Unmarshal([]byte(text(out)), &list)
	return resp.StatusCode, list
}

package kimi

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
)

func TestTheConfigNamesTheKeyVariableNeverTheKeyAndCarriesTheJail(t *testing.T) {
	p := New(Config{APIKey: "sk-secret", Models: []string{"kimi-k3", "kimi-k2.7-code"}})
	cfg := p.Config(agent.Request{Jail: agent.Jail{Commands: []string{"sudo"}, Deny: []string{"/srv/other"}}, Tools: []string{"Read", "Grep", "Glob"}})
	if strings.Contains(cfg, "sk-secret") {
		t.Fatal("the key is in the file")
	}
	for _, want := range []string{`api_key_env = "KIMI_API_KEY"`, `default_model = "kimi-k3"`, `[models."kimi-k2.7-code"]`, `pattern = "Bash(sudo*)"`, `pattern = "Read(/srv/other/**)"`, `pattern = "Write"`, `pattern = "Bash"`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("missing %s in\n%s", want, cfg)
		}
	}
}

func TestMessagesBecomeToolsResultsAndAnAnswerThatContinues(t *testing.T) {
	p := &parser{}
	var evs []agent.Event
	for _, l := range []string{
		`{"role":"meta","type":"system.version","version":"2.3.0"}`,
		`{"role":"assistant","tool_calls":[{"type":"function","id":"c1","function":{"name":"Read","arguments":"{\"path\":\"a.txt\"}"}}]}`,
		`{"role":"tool","tool_call_id":"c1","content":"hola"}`,
		`{"role":"assistant","content":"Pone hola."}`,
	} {
		e, _ := p.Line(l)
		evs = append(evs, e...)
	}
	evs = append(evs, p.Flush()...)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "tool,tool_result,text,cost,result" {
		t.Fatalf("%v", kinds)
	}
	if evs[0].Input["path"] != "a.txt" || evs[1].Output != "hola" || evs[4].SessionID != Resumed {
		t.Fatalf("%+v", evs)
	}
	if !strings.Contains(strings.Join(New(Config{}).Args(agent.Request{Prompt: "x", SessionID: Resumed}), " "), "--continue") {
		t.Fatal("a resumed turn does not continue")
	}
}

// With KIMI_BIN, the real CLI reads the config this writes and reaches
// Moonshot, which refuses the fake key: what comes back is that refusal.
func TestTheRealCLIAcceptsTheConfig(t *testing.T) {
	bin := os.Getenv("KIMI_BIN")
	if bin == "" {
		t.Skip("KIMI_BIN runs it")
	}
	dir := t.TempDir()
	p := New(Config{Bin: bin, APIKey: "sk-bogus", Models: []string{"kimi-k3"}})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	run, err := p.Start(ctx, agent.Request{Workdir: dir, Prompt: "hola", Name: "t", Stream: dir + "/stream", Jail: agent.Jail{SiteDir: dir, Home: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(ctx, run)
	last := evs[len(evs)-1]
	if last.Kind != agent.KindError || !strings.Contains(last.Text, "401") {
		t.Fatalf("%+v", evs)
	}
}

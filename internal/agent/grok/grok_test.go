package grok

import (
	"os"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

func parse(text string) []agent.Event {
	p := &parser{}
	var out []agent.Event
	for _, l := range strings.Split(text, "\n") {
		evs, done := p.Line(l)
		out = append(out, evs...)
		if done {
			return out
		}
	}
	return append(out, p.Flush()...)
}

// What Grok Build 1.0.67 printed for a wrong key.
func TestAWrongKeyIsGroksOwnError(t *testing.T) {
	raw, err := os.ReadFile("testdata/bad_key.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	evs := parse(string(raw))
	if len(evs) != 1 || evs[0].Kind != agent.KindError || !strings.Contains(evs[0].Text, "XAI_API_KEY") {
		t.Fatalf("%+v", evs)
	}
}

// The documented stream: chunks become one message per stretch of text,
// and end carries the session and the cost.
func TestChunksToolsAndTheEnd(t *testing.T) {
	evs := parse(`{"type":"thought","data":"Analyzing..."}
{"type":"text","data":"Voy a "}
{"type":"text","data":"leerlo."}
{"type":"tool_call","toolCallId":"call_1","title":"Read","kind":"read","status":"in_progress","toolName":"read_file","rawInput":{"path":"a.txt"},"content":[],"locations":[]}
{"type":"tool_call_update","toolCallId":"call_1","status":"completed","content":[],"rawOutput":{"lines":1},"locations":[]}
{"type":"text","data":"Pone hola."}
{"type":"usage","messageId":"resp_1","stopReason":"end_turn","usage":{"input_tokens":812}}
{"type":"end","stopReason":"end_turn","sessionId":"abc123","total_cost_usd":0.0126}`)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "text,tool,tool_result,text,cost,result" {
		t.Fatalf("%v", kinds)
	}
	if evs[0].Text != "Voy a leerlo." || evs[1].Tool != "Read" || evs[2].Output != `{"lines":1}` {
		t.Fatalf("%+v", evs[:3])
	}
	if last := evs[len(evs)-1]; last.Text != "Pone hola." || last.SessionID != "abc123" || last.Cost != 0.0126 {
		t.Fatalf("%+v", last)
	}
}

func TestTheJailIsGroksDenyRules(t *testing.T) {
	args := strings.Join(New(Config{}).Args(agent.Request{Prompt: "x", Tools: []string{"Read", "Grep"}, Jail: agent.Jail{Commands: []string{"sudo"}, Deny: []string{"/srv/b"}}}), " ")
	for _, want := range []string{"--deny Bash(sudo*)", "--deny Read(/srv/b/**)", "--deny Write(/srv/b/**)", "run_terminal_cmd", "--always-approve"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
}

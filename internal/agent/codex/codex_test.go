package codex

import (
	"os"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
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

// What Codex 0.163 printed for a wrong key: retries, then turn.failed.
func TestAWrongKeyIsOneErrorNotFiveRetries(t *testing.T) {
	raw, err := os.ReadFile("testdata/bad_key.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	evs := parse(string(raw))
	if len(evs) != 1 || evs[0].Kind != agent.KindError || !strings.Contains(evs[0].Text, "401") || evs[0].SessionID == "" {
		t.Fatalf("%+v", evs)
	}
}

// The shape OpenAI documents for a turn that ran a command and answered.
func TestACommandAndAnAnswer(t *testing.T) {
	evs := parse(`{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"docs\nsdk\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/s/a.txt","kind":"update"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Repo contains docs and sdk."}}
{"type":"turn.completed","usage":{"input_tokens":2476,"output_tokens":122}}`)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "tool,tool_result,tool,tool_result,text,cost,result" {
		t.Fatalf("%v", kinds)
	}
	if evs[0].Tool != "Bash" || evs[1].Output != "docs\nsdk\n" || evs[2].Input["file_path"] != "/s/a.txt" {
		t.Fatalf("%+v", evs[:3])
	}
	if last := evs[len(evs)-1]; last.Text != "Repo contains docs and sdk." || last.SessionID != "t1" {
		t.Fatalf("%+v", last)
	}
}

func TestAReadOnlyLevelRunsReadOnlyAndAResumeGoesByThread(t *testing.T) {
	p := New(Config{Models: []string{"gpt-5.5"}})
	args := strings.Join(p.Args(agent.Request{Prompt: "hola", Tools: []string{"Read"}, SessionID: "t1"}), " ")
	for _, want := range []string{"exec resume --json", `sandbox_mode="read-only"`, "--model gpt-5.5", "t1 hola"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	if !strings.Contains(strings.Join(p.Args(agent.Request{Prompt: "x"}), " "), `sandbox_mode="workspace-write"`) {
		t.Fatal("a full turn is not workspace-write")
	}
}

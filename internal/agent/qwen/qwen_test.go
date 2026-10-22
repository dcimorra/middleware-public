package qwen

import (
	"os"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/agent/claude"
)

// What Qwen Code 0.26 printed for a turn with a wrong key.
func TestAWrongKeyEndsTheTurnWithQwensReason(t *testing.T) {
	raw, err := os.ReadFile("testdata/bad_key.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	p := claude.NewParser()
	var evs []agent.Event
	for _, l := range strings.Split(string(raw), "\n") {
		e, _ := p.Line(l)
		evs = append(evs, e...)
	}
	last := evs[len(evs)-1]
	if last.Kind != agent.KindError || !strings.Contains(last.Text, "401") || last.SessionID == "" {
		t.Fatalf("%+v", last)
	}
}

func TestALevelBecomesExcludedToolsAndTheModelGoesOnTheLine(t *testing.T) {
	p := New(Config{Models: []string{"qwen3.8-max"}})
	args := strings.Join(p.Args(agent.Request{Prompt: "hola", Tools: []string{"Read", "Glob", "Grep"}, SessionID: "s1"}), " ")
	for _, want := range []string{"--model qwen3.8-max", "--resume s1", "run_shell_command", "write_file", "--approval-mode yolo"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	if strings.Contains(args, "read_file,") && strings.Contains(strings.SplitN(args, "--exclude-tools ", 2)[1], "read_file") {
		t.Fatalf("read excluded: %s", args)
	}
}

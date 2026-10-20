package chat

import (
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
)

func TestQuestionBlocksAreParsedAndMalformedOnesSkipped(t *testing.T) {
	text := "Before I continue:\n\n```question\n{\"question\": \"Which database?\", \"options\": [\"SQLite\", \"Postgres\"], \"recommended\": \"SQLite\"}\n```\n\n" +
		"```question\nnot json\n```\n" +
		"```question\n{\"question\": \"\", \"options\": [\"a\"]}\n```\n" +
		"```\n```question\n{\"question\": \"inside another block\"}\n```\n" +
		"```question\n{\"question\": \"Anything else?\"}\n```\n" +
		"```question\n{\"question\": \"never closed\"}\n"
	qs := ParseQuestions(text)
	if len(qs) != 3 {
		t.Fatalf("questions = %+v", qs)
	}
	if qs[0].Question != "Which database?" || len(qs[0].Options) != 2 || qs[0].Recommended != "SQLite" {
		t.Fatalf("first = %+v", qs[0])
	}
	// A block whose fence line follows an unrelated closing fence is read
	// once that outer block is closed, like any other; the one without a
	// closing fence is not.
	if qs[1].Question != "inside another block" || qs[2].Question != "Anything else?" || qs[2].Options == nil || len(qs[2].Options) != 0 {
		t.Fatalf("rest = %+v", qs[1:])
	}
	if ParseQuestions("no questions here\n```sh\necho hi\n```\n") != nil {
		t.Fatal("found a question where there is none")
	}
}

func TestAQuestionInTheAgentsTextIsEmittedAsItArrivesAndStoredOnTheTurn(t *testing.T) {
	block := "```question\n{\"question\": \"Which database?\", \"options\": [\"SQLite\", \"Postgres\"], \"recommended\": \"SQLite\"}\n```"
	p := &fakeProvider{key: "fake", script: []agent.Event{
		{Kind: agent.KindText, Text: "I need a decision.\n\n" + block},
		{Kind: agent.KindTool, Tool: "Read", Input: map[string]any{"file_path": "README.md"}},
		{Kind: agent.KindResult, Text: "waiting"},
	}}
	m, _, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	id, err := m.Send("a.test", "set it up", "")
	if err != nil {
		t.Fatal(err)
	}
	q := waitFor(t, ch, "agent.question")
	opts, _ := q.Data["options"].([]string)
	if q.Subject != "a.test" || q.Data["turn_id"] != id || q.Data["question"] != "Which database?" || len(opts) != 2 || q.Data["recommended"] != "SQLite" {
		t.Fatalf("question event = %+v", q)
	}
	// The tool call comes after the question: it was emitted as the text
	// arrived, not when the turn ended.
	waitFor(t, ch, "agent.tool")
	waitFor(t, ch, "agent.done")
	c, _ := m.History("a.test")
	last := c.Turns[len(c.Turns)-1]
	if len(last.Questions) != 1 || last.Questions[0].Question != "Which database?" || last.Questions[0].Recommended != "SQLite" {
		t.Fatalf("turn = %+v", last)
	}
	if !strings.Contains(last.Text, block) {
		t.Fatal("the block was stripped from the stored text; the panel decides how to show it")
	}
	// A provider that only reports the final result still has its
	// question read from it.
	p2 := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "```question\n{\"question\": \"Deploy now?\", \"options\": [\"Yes\", \"No\"]}\n```"}}}
	m.opts.Registry.Register(p2)
	if _, err := m.Send("b.test", "go", ""); err != nil {
		t.Fatal(err)
	}
	if q := waitFor(t, ch, "agent.question"); q.Data["question"] != "Deploy now?" {
		t.Fatalf("question from result = %+v", q)
	}
	waitFor(t, ch, "agent.done")
}

func TestThePreambleCarriesTheQuestionAndLongWorkRules(t *testing.T) {
	s := Preamble("a.test", "/srv/middleware/sites/a.test", agent.Jail{})
	for _, want := range []string{"```question", "\"recommended\"", "setsid nohup", ">/tmp/<name>.log 2>&1 </dev/null &", "log's path"} {
		if !strings.Contains(s, want) {
			t.Errorf("preamble lacks %q:\n%s", want, s)
		}
	}
}

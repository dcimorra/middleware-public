package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
)

func fakeBin(t *testing.T, dir, output string) (bin, argsFile string) {
	t.Helper()
	argsFile = filepath.Join(dir, "args")
	os.WriteFile(filepath.Join(dir, "out.txt"), []byte(output), 0o644)
	bin = filepath.Join(dir, "gemini")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat " + filepath.Join(dir, "out.txt") + "\n"
	os.WriteFile(bin, []byte(script), 0o755)
	return bin, argsFile
}

func TestASingleJSONDocumentBecomesOneTextEventAndAResult(t *testing.T) {
	dir := t.TempDir()
	out := "Loaded cached credentials.\n{\n  \"response\": \"The file has three lines.\",\n  \"session_id\": \"g-1\",\n  \"stats\": {\"cost\": 0.002, \"models\": {}}\n}\n"
	bin, argsFile := fakeBin(t, dir, out)
	site := filepath.Join(dir, "site")
	os.MkdirAll(site, 0o755)
	p := New(Config{Bin: bin, ResumeFlag: "--resume", SandboxArgs: []string{"--sandbox"}})
	run, err := p.Start(context.Background(), agent.Request{Workdir: site, Prompt: "count", Model: "gemini-2.5-flash", SessionID: "old", System: "Stay here.", Jail: agent.Jail{SiteDir: site, Home: filepath.Join(dir, "home"), Deny: []string{"/x"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(context.Background(), run)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, " "); got != "text text cost result" {
		t.Fatalf("kinds = %q\n%+v", got, evs)
	}
	if evs[0].Text != "Loaded cached credentials." || evs[1].Text != "The file has three lines." {
		t.Fatalf("texts = %+v", evs[:2])
	}
	last := evs[len(evs)-1]
	if last.SessionID != "g-1" || last.Cost != 0.002 {
		t.Fatalf("result = %+v", last)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"-p\nStay here.\n\n---\n\ncount\n", "--output-format\njson\n", "--model\ngemini-2.5-flash\n", "--resume\nold\n", "--sandbox\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
}

func TestAStreamJSONTranscriptBecomesEvents(t *testing.T) {
	dir := t.TempDir()
	out := `{"type":"init","session_id":"g-2","model":"gemini-2.5-pro"}
{"type":"message","role":"user","content":"hi"}
{"type":"message","role":"assistant","content":"Looking."}
{"type":"tool_use","tool_name":"read_file","parameters":{"path":"README.md"}}
{"type":"tool_result","tool_name":"read_file","output":"x"}
{"type":"message","role":"assistant","content":[{"text":"Done."}]}
{"type":"result","status":"success","stats":{"cost":0.01}}
`
	bin, _ := fakeBin(t, dir, out)
	site := filepath.Join(dir, "site")
	os.MkdirAll(site, 0o755)
	p := New(Config{Bin: bin, OutputFormat: "stream-json"})
	run, err := p.Start(context.Background(), agent.Request{Workdir: site, Prompt: "x", Jail: agent.Jail{SiteDir: site, Home: filepath.Join(dir, "home")}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(context.Background(), run)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, " "); got != "text tool text cost result" {
		t.Fatalf("kinds = %q\n%+v", got, evs)
	}
	if evs[1].Tool != "read_file" || evs[1].Input["path"] != "README.md" || evs[1].SessionID != "g-2" {
		t.Fatalf("tool = %+v", evs[1])
	}
	if evs[4].Cost != 0.01 {
		t.Fatalf("cost = %+v", evs[4])
	}
}

func TestAnErrorDocumentBecomesAnErrorEvent(t *testing.T) {
	p := &parser{}
	evs, done := p.Line(`{"error":{"type":"AuthError","message":"not logged in"}}`)
	if !done || len(evs) != 1 || evs[0].Kind != agent.KindError || evs[0].Text != "not logged in" {
		t.Fatalf("events = %+v done=%v", evs, done)
	}
}

func TestAnUnfinishedDocumentIsFlushedAsText(t *testing.T) {
	p := &parser{}
	p.Line(`{`)
	p.Line(`  "response": "cut`)
	evs := p.Flush()
	if len(evs) != 2 || evs[0].Kind != agent.KindText || !strings.Contains(evs[0].Text, "cut") || evs[1].Kind != agent.KindResult {
		t.Fatalf("events = %+v", evs)
	}
}

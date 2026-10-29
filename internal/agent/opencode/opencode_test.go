package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// What `opencode run --format json` printed for a turn that read a file
// and answered, with a first run's migration noise in front.
const turn = `Performing one time database migration, may take a few minutes...
sqlite-migration:done
{"type":"step_start","timestamp":1,"sessionID":"ses_1","part":{"type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_1","part":{"type":"tool","tool":"read","callID":"call_1","state":{"status":"completed","input":{"filePath":"/s/a.txt"},"output":"hola","title":"a.txt"}}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_1","part":{"type":"step-finish","cost":0.002,"reason":"tool-calls"}}
{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"type":"text","text":"El fichero dice hola."}}
{"type":"step_finish","timestamp":5,"sessionID":"ses_1","part":{"type":"step-finish","cost":0.001,"reason":"stop"}}
`

func parse(text string) []agent.Event {
	p := &parser{}
	var out []agent.Event
	for _, l := range strings.Split(text, "\n") {
		evs, _ := p.Line(l)
		out = append(out, evs...)
	}
	return append(out, p.Flush()...)
}

func TestATurnBecomesToolTextCostAndResult(t *testing.T) {
	evs := parse(turn)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "tool,tool_result,text,cost,result" {
		t.Fatalf("%v", kinds)
	}
	if evs[0].Tool != "Read" || evs[0].Input["file_path"] != "/s/a.txt" || evs[0].ToolID != "call_1" || evs[1].Output != "hola" {
		t.Fatalf("tool %+v %+v", evs[0], evs[1])
	}
	last := evs[len(evs)-1]
	if last.Text != "El fichero dice hola." || last.SessionID != "ses_1" || last.Cost < 0.0029 || last.Cost > 0.0031 {
		t.Fatalf("result %+v", last)
	}
}

func TestAnErrorOrNothingAtAllEndsTheTurnSayingWhy(t *testing.T) {
	evs := parse(`{"type":"error","sessionID":"ses_2","error":{"name":"APIError","data":{"message":"invalid api key"}}}`)
	if len(evs) != 2 || evs[1].Kind != agent.KindError || evs[1].Text != "invalid api key" {
		t.Fatalf("%+v", evs)
	}
	evs = parse("Error: something crashed\n    at foo (x.js:1)")
	if len(evs) != 1 || evs[0].Kind != agent.KindError || !strings.Contains(evs[0].Text, "something crashed") {
		t.Fatalf("%+v", evs)
	}
}

// The jail, as OpenCode reads it: outside the folder denied, the jail's
// commands denied after a general allow (the last matching rule wins),
// and a read-only level denies every tool it does not list.
func TestTheJailBecomesPermissionsInTheOrderOpenCodeReadsThem(t *testing.T) {
	req := agent.Request{Jail: agent.Jail{Commands: []string{"sudo"}, Lent: []string{"/srv/lent"}}}
	cfg := orderedJSON(map[string]any{"permission": Permissions(req)})
	for _, want := range []string{`"external_directory":{"*":"deny","/srv/lent/**":"allow"}`, `"bash":{"*":"allow","sudo":"deny","sudo *":"deny"}`, `"edit":{"*":"allow","/srv/lent/**":"deny"}`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("missing %s in %s", want, cfg)
		}
	}
	read := Permissions(agent.Request{Tools: []string{"Read", "Glob", "Grep"}})
	if read["bash"] != "deny" || read["edit"] != "deny" || read["webfetch"] != "deny" || read["read"] != nil {
		t.Fatalf("%v", read)
	}
	var v any
	if err := json.Unmarshal([]byte(cfg), &v); err != nil {
		t.Fatal(err)
	}
}

func TestTheModelAndTheSessionGoOnTheCommandLine(t *testing.T) {
	p := New(Config{Models: []string{"openai/gpt-4.1"}})
	args := strings.Join(p.Args(agent.Request{Prompt: "-hola", SessionID: "ses_1"}), " ")
	if args != "run --format json --model openai/gpt-4.1 --session ses_1  -hola" {
		t.Fatalf("%q", args)
	}
}

func TestTheKeysAreWrittenWhereTheCLIReadsThem(t *testing.T) {
	home := t.TempDir()
	if err := WriteAuth(home, map[string]string{"openai": "sk-1"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".local/share/opencode/auth.json"))
	if !strings.Contains(string(raw), `"type": "api"`) || !strings.Contains(string(raw), "sk-1") {
		t.Fatalf("%s", raw)
	}
	st, _ := os.Stat(AuthPath(home))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("%v", st.Mode())
	}
}

package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// transcript is what a real `claude -p --output-format stream-json
// --verbose` printed for a short turn, trimmed to the fields that matter.
const transcript = `{"type":"system","subtype":"init","cwd":"/x","session_id":"sess-123","tools":["Read","Edit"],"model":"claude-sonnet"}
{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"Let me read the file."},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x/README.md"}}]},"session_id":"sess-123"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"hello"}]},"session_id":"sess-123"}
a stray non-json warning
{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"Done."}]},"session_id":"sess-123"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"num_turns":2,"result":"Done.","session_id":"sess-123","total_cost_usd":0.0421}
`

// fakeBin writes a script that records its arguments and environment and
// prints the canned transcript, so the adapter is tested end to end
// without the real CLI, a login or a network.
func fakeBin(t *testing.T, dir string) (bin, argsFile, envFile string) {
	t.Helper()
	argsFile = filepath.Join(dir, "args")
	envFile = filepath.Join(dir, "env")
	os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(transcript), 0o644)
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nenv > " + envFile + "\ncat " + filepath.Join(dir, "transcript.jsonl") + "\n"
	os.WriteFile(bin, []byte(script), 0o755)
	return bin, argsFile, envFile
}

func TestAFakeClaudeTranscriptBecomesEventsWithSessionAndCost(t *testing.T) {
	dir := t.TempDir()
	bin, argsFile, envFile := fakeBin(t, dir)
	site := filepath.Join(dir, "site")
	home := filepath.Join(dir, "home")
	os.MkdirAll(site, 0o755)
	p := New(Config{Bin: bin, APIKey: "sk-test", ExtraArgs: []string{"--extra"}})
	req := agent.Request{
		Workdir: site, Prompt: "hello", Model: "sonnet", SessionID: "prev", System: "You are in a jail.",
		Jail: agent.Jail{SiteDir: site, Home: home, Deny: []string{"/other"}},
	}
	run, err := p.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(context.Background(), run)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := "text tool tool_result text text cost result"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("kinds = %q, want %q\n%+v", got, want, evs)
	}
	if evs[3].Text != "a stray non-json warning" {
		t.Fatalf("the non-json line was not passed as text: %+v", evs[2])
	}
	last := evs[len(evs)-1]
	if last.SessionID != "sess-123" || last.Cost != 0.0421 || last.Text != "Done." {
		t.Fatalf("result = %+v", last)
	}
	if evs[1].Tool != "Read" || evs[1].Input["file_path"] != "/x/README.md" || evs[1].SessionID != "sess-123" {
		t.Fatalf("tool = %+v", evs[1])
	}

	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"-p\nhello\n", "--output-format\nstream-json\n", "--verbose\n", "--permission-mode\nbypassPermissions\n", "--model\nsonnet\n", "--resume\nprev\n", "--add-dir\n" + site + "\n", "--append-system-prompt\nYou are in a jail.\n", "--settings\n", "--extra\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "ANTHROPIC_API_KEY=sk-test\n") || !strings.Contains(string(env), "HOME="+home+"\n") {
		t.Fatalf("env:\n%s", env)
	}

	// The settings file carries a deny rule for the jailed path.
	var settings map[string]any
	data, err := os.ReadFile(filepath.Join(home, ".middleware", "claude-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"Read(//other/**)"`) || !strings.Contains(string(data), `"Bash(*/other*)"`) {
		t.Fatalf("settings:\n%s", data)
	}
}

func TestTheAPIKeyIsNotPassedUnlessConfigured(t *testing.T) {
	dir := t.TempDir()
	bin, _, envFile := fakeBin(t, dir)
	t.Setenv("ANTHROPIC_API_KEY", "from-the-supervisor")
	site := filepath.Join(dir, "site")
	os.MkdirAll(site, 0o755)
	p := New(Config{Bin: bin})
	run, err := p.Start(context.Background(), agent.Request{Workdir: site, Prompt: "hi", Jail: agent.Jail{SiteDir: site, Home: filepath.Join(dir, "home")}})
	if err != nil {
		t.Fatal(err)
	}
	agent.Collect(context.Background(), run)
	env, _ := os.ReadFile(envFile)
	if strings.Contains(string(env), "ANTHROPIC_API_KEY") {
		t.Fatalf("the supervisor's key leaked:\n%s", env)
	}
}

func TestAnErrorResultBecomesAnErrorEvent(t *testing.T) {
	p := &parser{}
	evs, done := p.Line(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"too many turns","session_id":"s","total_cost_usd":0.5}`)
	if !done || len(evs) != 2 || evs[1].Kind != agent.KindError || evs[1].Text != "too many turns" || evs[0].Cost != 0.5 {
		t.Fatalf("events = %+v done=%v", evs, done)
	}
}

func TestAMissingBinaryIsReportedAsUnavailable(t *testing.T) {
	p := New(Config{Bin: "claude-does-not-exist-" + t.Name()})
	if ok, why := p.Available(); ok || !strings.Contains(why, "not found") {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
	if _, err := p.Start(context.Background(), agent.Request{Workdir: t.TempDir(), Prompt: "x"}); err == nil {
		t.Fatal("start succeeded without a binary")
	}
}

func TestTheHouseJailDeniesEachSiteInTheSettingsAndNotTheSitesFolder(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"sites/old.test", "data/homes/middleware", "modules"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	j := agent.NewJail(root, root, filepath.Join(root, "data", "homes", "middleware"))
	deny, _ := json.Marshal(Settings(j)["permissions"].(map[string]any)["deny"])
	rules := string(deny)
	if !strings.Contains(rules, "Read(/"+filepath.Join(root, "sites", "old.test")+"/**)") || !strings.Contains(rules, "Bash(*"+filepath.Join(root, "modules")+"*)") {
		t.Fatalf("rules do not name the existing site and the modules:\n%s", rules)
	}
	if strings.Contains(rules, filepath.Join(root, "sites")+"/**") || strings.Contains(rules, filepath.Join(root, "sites")+")") {
		t.Fatalf("rules deny the sites folder itself, so a new site could not be created:\n%s", rules)
	}
}

func TestALentFolderIsClosedToWritingAndAdded(t *testing.T) {
	j := agent.Jail{SiteDir: "/s/a", Lent: []string{"/s/b"}}
	rules, _ := json.Marshal(Settings(j))
	for _, want := range []string{"Edit(//s/b/**)", "Write(//s/b/**)", "Bash(*/s/b*)"} {
		if !strings.Contains(string(rules), want) {
			t.Errorf("missing %s in %s", want, rules)
		}
	}
	if strings.Contains(string(rules), "Read(//s/b") {
		t.Fatal("the lent folder cannot be read")
	}
	args := strings.Join(New(Config{}).Args(agent.Request{Prompt: "x", Jail: j}, ""), " ")
	if !strings.Contains(args, "--add-dir /s/b") {
		t.Fatalf("args: %s", args)
	}
}

func TestASubagentsResultAndStateAreRead(t *testing.T) {
	p := &parser{}
	evs, _ := p.Line(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tA","name":"Task","input":{"description":"look","run_in_background":true}}]}}`)
	if len(evs) != 1 || evs[0].ToolID != "tA" || evs[0].Tool != "Task" {
		t.Fatalf("%+v", evs)
	}
	evs, _ = p.Line(`{"type":"user","parent_tool_use_id":"tA","message":{"content":[{"type":"tool_result","tool_use_id":"t9","content":[{"type":"text","text":"line 1"},{"type":"text","text":"line 2"}],"is_error":true}]}}`)
	if len(evs) != 1 || evs[0].Kind != agent.KindToolResult || evs[0].Output != "line 1\nline 2" || !evs[0].IsError || evs[0].Thread != "tA" {
		t.Fatalf("%+v", evs)
	}
	evs, _ = p.Line(`{"type":"system","subtype":"task_notification","tool_use_id":"tA","status":"completed"}`)
	if len(evs) != 1 || evs[0].Kind != agent.KindAgentStatus || evs[0].Status != "completed" || evs[0].ToolID != "tA" {
		t.Fatalf("%+v", evs)
	}
}

func TestARateLimitEventBecomesTheWindowItNames(t *testing.T) {
	p := &parser{}
	evs, _ := p.Line(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1790000000,"rateLimitType":"seven_day","utilization":0.81},"uuid":"u","session_id":"s1"}`)
	if len(evs) != 1 || evs[0].Kind != agent.KindLimits {
		t.Fatalf("%+v", evs)
	}
	w := evs[0].Limits[0]
	if w.Name != "7d" || w.Used < 80.99 || w.Used > 81.01 || w.Status != "allowed_warning" || w.ResetsAt.Unix() != 1790000000 {
		t.Fatalf("%+v", w)
	}
	evs, _ = p.Line(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour"}}`)
	if len(evs) != 1 || evs[0].Limits[0].Used != -1 {
		t.Fatalf("a window without utilization is not unknown: %+v", evs)
	}
	// The week binds, and the five hours come only in unifiedWindows.
	evs, _ = p.Line(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1790000000,"rateLimitType":"seven_day","unifiedWindows":{"five_hour":{"utilization":0.12,"resetsAt":1789000000},"seven_day":{"utilization":0.4,"resetsAt":1790000000}}}}`)
	ws := evs[0].Limits
	if len(ws) != 2 || ws[0].Name != "5h" || ws[0].Used < 11.99 || ws[0].Used > 12.01 || ws[0].ResetsAt.Unix() != 1789000000 || ws[1].Name != "7d" || ws[1].Used < 39.99 || ws[1].Status != "allowed" {
		t.Fatalf("%+v", ws)
	}
}

func TestClaudesAliasesSayWhatTheyRunAndAVendorsDoNot(t *testing.T) {
	l := New(Config{}).ModelLabels()
	if l["sonnet"] != "claude-sonnet-5-5" || l["opus"] != "claude-opus-5-5" || l["haiku"] == "" {
		t.Fatalf("labels %v", l)
	}
	// DeepSeek through Claude Code: its names are its own, and "sonnet"
	// is not one of them.
	if l := New(Config{Key: "deepseek"}).ModelLabels(); l != nil {
		t.Fatalf("a vendor got Claude's labels: %v", l)
	}
}

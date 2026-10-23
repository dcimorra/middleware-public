package agy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// fakeBin prints a recorded stream and keeps its arguments and HOME.
func fakeBin(t *testing.T, dir, fixture string) (bin, argsFile string) {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	argsFile = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "agy")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho \"AGY_CLI_DISABLE_AUTO_UPDATE=$AGY_CLI_DISABLE_AUTO_UPDATE\" >> " + argsFile + "\ncat " + src + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

const signIn = `{"token":{"access_token":"ya29.a","token_type":"Bearer","refresh_token":"1//r","expiry":"2026-10-23T17:12:11Z"},"auth_method":"consumer"}`

func TestARecordedTurnBecomesToolsTextAndAResult(t *testing.T) {
	dir := t.TempDir()
	bin, argsFile := fakeBin(t, dir, "stream-1.4.6.jsonl")
	site, home := filepath.Join(dir, "site"), filepath.Join(dir, "home")
	os.MkdirAll(site, 0o755)
	p := New(Config{Bin: bin, Token: signIn})
	run, err := p.Start(context.Background(), agent.Request{Workdir: site, Prompt: "/read it", Model: "gemini-3.8-flash-high", SessionID: "old", System: "Stay here.",
		Jail: agent.Jail{SiteDir: site, Home: home, Deny: []string{"/other"}, Commands: []string{"sudo"}, Secrets: []string{".env"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(context.Background(), run)
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, " "); got != "tool tool_result tool tool_result text result" {
		t.Fatalf("kinds = %q\n%+v", got, evs)
	}
	if evs[0].Tool != "Read" || evs[0].Input["file_path"] != "/tmp/agyprobe2/a.txt" || evs[2].Tool != "Bash" || evs[2].Input["command"] != "ls" {
		t.Fatalf("tools = %+v %+v", evs[0], evs[2])
	}
	if evs[1].ToolID != evs[0].ToolID || !strings.Contains(evs[3].Output, "a.txt") {
		t.Fatalf("results = %+v %+v", evs[1], evs[3])
	}
	// The deltas of one answer are one block, not one per delta.
	if !strings.Contains(evs[4].Text, "hello") || strings.Count(evs[4].Text, "a.txt") < 1 {
		t.Fatalf("text = %q", evs[4].Text)
	}
	last := evs[len(evs)-1]
	if last.SessionID != "1390e007-59d3-44e1-a11b-357243d6c781" {
		t.Fatalf("result = %+v", last)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"-p\nStay here.\n\n---\n\n/read it\n", "--output-format\nstream-json\n", "--dangerously-skip-permissions\n", "--model\ngemini-3.8-flash-high\n", "--conversation\nold\n", "AGY_CLI_DISABLE_AUTO_UPDATE=1"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	tok, _ := os.ReadFile(filepath.Join(Dir(home), TokenFile))
	if string(tok) != signIn {
		t.Fatalf("sign-in not seeded: %q", tok)
	}
	var st struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	raw, _ := os.ReadFile(filepath.Join(Dir(home), "settings.json"))
	json.Unmarshal(raw, &st)
	got := strings.Join(st.Permissions.Deny, " ")
	for _, want := range []string{"read_file(/other)", "write_file(/other)", "command(sudo)", "read_file(" + filepath.Join(site, ".env") + ")"} {
		if !strings.Contains(got, want) {
			t.Errorf("deny missing %q: %s", want, got)
		}
	}
}

func TestARefreshedSignInIsNotOverwrittenByTheStoredOne(t *testing.T) {
	dir := t.TempDir()
	refreshed := strings.Replace(signIn, "ya29.a", "ya29.newer", 1)
	os.WriteFile(filepath.Join(dir, TokenFile), []byte(refreshed), 0o600)
	if err := SeedToken(dir, signIn); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, TokenFile)); string(got) != refreshed {
		t.Fatalf("the CLI's refreshed token was replaced: %s", got)
	}
	// A new sign-in (another refresh token) does replace it.
	other := strings.Replace(signIn, "1//r", "1//other", 1)
	SeedToken(dir, other)
	if got, _ := os.ReadFile(filepath.Join(dir, TokenFile)); string(got) != other {
		t.Fatalf("a new sign-in was not written: %s", got)
	}
}

func TestAnErrorResultEndsTheTurnAsAnError(t *testing.T) {
	p := &parser{}
	evs, done := p.Line(`{"event":"result","result":{"conversation_id":"","status":"ERROR","response":"","error":"invalid model selection"}}`)
	if !done || len(evs) != 1 || evs[0].Kind != agent.KindError || evs[0].Text != "invalid model selection" {
		t.Fatalf("evs = %+v done=%v", evs, done)
	}
}

func TestAStreamCutShortIsAnErrorWithWhatWasSaid(t *testing.T) {
	p := &parser{}
	p.Line(`{"event":"step_update","step_update":{"conversation_id":"c","step_index":3,"state":"ACTIVE","step_type":"agent_response","text_delta":"half "}}`)
	p.Line(`{"event":"step_update","step_update":{"conversation_id":"c","step_index":3,"state":"ACTIVE","step_type":"agent_response","text_delta":"said"}}`)
	evs := p.Flush()
	if len(evs) != 2 || evs[0].Text != "half said" || evs[1].Kind != agent.KindError {
		t.Fatalf("evs = %+v", evs)
	}
}

func TestUsageIsReadFromTheRecordedAnswer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "usage-1.4.6.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, ws, err := ParseUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].Name != "gemini-weekly" || ws[0].Label != "Gemini Models" || ws[1].Name != "3p-weekly" {
		t.Fatalf("windows = %+v", ws)
	}
	if ws[0].Used < 0 || ws[0].Used > 100 || ws[0].ResetsAt.IsZero() {
		t.Fatalf("window = %+v", ws[0])
	}
}

package agy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
)

// With AGY_BIN and AGY_TOKEN (a signed-in antigravity-oauth-token file),
// the real CLI runs a turn as the house starts one: a file read through
// its tool, a command its jail refuses, and the answer. Then its /usage.
// It spends a few thousand tokens of the account's weekly limit.
func TestTheRealCLIRunsATurnInsideItsJailAndSaysItsUsage(t *testing.T) {
	bin, tokFile := os.Getenv("AGY_BIN"), os.Getenv("AGY_TOKEN")
	if bin == "" || tokFile == "" {
		t.Skip("AGY_BIN and AGY_TOKEN run it")
	}
	token, err := os.ReadFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	site, home := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(site, "a.txt"), []byte("the word is heliotrope\n"), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	run, err := New(Config{Bin: bin, Token: string(token)}).Start(ctx, agent.Request{Workdir: site, Stream: filepath.Join(t.TempDir(), "stream"),
		Prompt: "Read a.txt with your file tool, then try to run `sudo true` in the shell, then answer with the word in a.txt and whether sudo ran.",
		System: "Be brief.", Jail: agent.Jail{SiteDir: site, Home: home, Commands: []string{"sudo"}}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(ctx, run)
	var read, refused bool
	for _, e := range evs {
		if e.Kind == agent.KindTool && e.Tool == "Read" {
			read = true
		}
		if e.Kind == agent.KindToolResult && e.IsError && strings.Contains(e.Output, "deny rule") {
			refused = true
		}
	}
	last := evs[len(evs)-1]
	if last.Kind != agent.KindResult || !strings.Contains(strings.ToLower(last.Text), "heliotrope") || last.SessionID == "" {
		t.Fatalf("last = %+v\n%+v", last, evs)
	}
	if !read || !refused {
		t.Fatalf("read=%v refused=%v\n%+v", read, refused, evs)
	}
	_, ws, err := Usage(ctx, bin, "", home)
	if err != nil || len(ws) == 0 {
		t.Fatalf("usage: %v %+v", err, ws)
	}
	for _, w := range ws {
		t.Logf("%s (%s): %.1f%% used, resets %s", w.Label, w.Name, w.Used, w.ResetsAt)
	}
}

// A site door that may only read: the real CLI reads, and its own rules
// refuse the edit and the command.
func TestTheRealCLIOnlyReadsWhenTheDoorOnlyLetsItRead(t *testing.T) {
	bin, tokFile := os.Getenv("AGY_BIN"), os.Getenv("AGY_TOKEN")
	if bin == "" || tokFile == "" {
		t.Skip("AGY_BIN and AGY_TOKEN run it")
	}
	token, err := os.ReadFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	site, home := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(site, "a.txt"), []byte("the word is heliotrope\n"), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	run, err := New(Config{Bin: bin, Token: string(token)}).Start(ctx, agent.Request{Workdir: site, Stream: filepath.Join(t.TempDir(), "stream"),
		Prompt: "Read a.txt with your file tool. Then create b.txt with the content x using your file writing tool. Then run `echo hi` in the shell. Report the word and what worked.",
		Tools:  []string{"Read", "Glob", "Grep"}, Disallowed: []string{"Task"},
		Jail: agent.Jail{SiteDir: site, Home: home}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(ctx, run)
	var read, ran bool
	for i, e := range evs {
		if e.Kind != agent.KindTool {
			continue
		}
		var res agent.Event
		for _, r := range evs[i:] {
			if r.Kind == agent.KindToolResult && r.ToolID == e.ToolID {
				res = r
				break
			}
		}
		switch e.Tool {
		case "Read":
			read = read || !res.IsError
		case "Bash":
			ran = ran || !res.IsError
		}
	}
	if _, err := os.Stat(filepath.Join(site, "b.txt")); err == nil {
		t.Fatal("a door that may only read wrote b.txt")
	}
	if !read || ran {
		t.Fatalf("read=%v ran=%v\n%+v", read, ran, evs)
	}
}

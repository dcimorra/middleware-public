package vibe

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// An entry printed while it is generated counts once, when finished; an
// effect is a tool call, then its result.
func TestEntriesCountOnceWhenFinished(t *testing.T) {
	p := &parser{}
	var evs []agent.Event
	for _, l := range []string{
		`{"id":"u1","sessionId":"s1","type":"message","role":"user","generationStatus":"completed","content":[{"type":"text","text":"lee a.txt"}]}`,
		`{"id":"e1","sessionId":"s1","type":"effect","title":"Read a.txt","generationStatus":"generating","detail":{"toolName":"read_file","kind":"file_read","input":{"path":"a.txt"}},"state":{"status":"running"}}`,
		`{"id":"e1","sessionId":"s1","type":"effect","title":"Read a.txt","generationStatus":"completed","detail":{"toolName":"read_file","kind":"file_read","input":{"path":"a.txt"}},"state":{"status":"completed","outputText":"hola"}}`,
		`{"id":"a1","sessionId":"s1","type":"message","role":"assistant","generationStatus":"generating","content":[{"type":"text","text":"Pone"}]}`,
		`{"id":"a1","sessionId":"s1","type":"message","role":"assistant","generationStatus":"completed","content":[{"type":"text","text":"Pone hola."}]}`,
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
	if evs[0].Tool != "read_file" || evs[1].Output != "hola" || evs[4].Text != "Pone hola." || evs[4].SessionID != "s1" {
		t.Fatalf("%+v", evs)
	}
}

func TestALevelRunsWithOnlyItsTools(t *testing.T) {
	args := strings.Join(New(Config{}).Args(agent.Request{Prompt: "x", Tools: []string{"Read", "Grep"}, SessionID: "s1"}), " ")
	for _, want := range []string{"--enabled-tools read_file", "--enabled-tools grep", "--resume s1", "--output streaming"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	if !strings.Contains(strings.Join(New(Config{}).Args(agent.Request{Prompt: "x", Tools: []string{}}), " "), "--disabled-tools *") {
		t.Fatal("no tools is not no tools")
	}
}

// With VIBE_BIN, the real CLI takes the flags this builds and reaches
// Mistral, which refuses the fake key.
func TestTheRealCLITakesTheFlags(t *testing.T) {
	bin := os.Getenv("VIBE_BIN")
	if bin == "" {
		t.Skip("VIBE_BIN runs it")
	}
	dir := t.TempDir()
	// Vibe leaves read-only files under its HOME, which t.TempDir cannot
	// remove: this one is made writable first.
	home, err := os.MkdirTemp("", "vibe-home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			_ = os.Chmod(p, 0o700)
			return nil
		})
		_ = os.RemoveAll(home)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	run, err := New(Config{Bin: bin, APIKey: "bogus"}).Start(ctx, agent.Request{Workdir: dir, Prompt: "hola", Tools: []string{"Read"}, Stream: dir + "/stream", Jail: agent.Jail{SiteDir: dir, Home: home}})
	if err != nil {
		t.Fatal(err)
	}
	evs := agent.Collect(ctx, run)
	last := evs[len(evs)-1]
	if last.Kind != agent.KindError || !strings.Contains(strings.ToLower(last.Text), "api key") {
		t.Fatalf("%+v", evs)
	}
}

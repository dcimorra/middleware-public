package chat

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
)

func TestTheEmbeddedSkillIsInstalledInTheHomeBeforeATurn(t *testing.T) {
	p := &fakeProvider{key: "fake", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("a.test", "hi", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	got, err := os.ReadFile(filepath.Join(root, "data", "homes", "a.test", ".claude", "skills", "middleware", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, SkillContent()) || !strings.HasPrefix(string(got), "---\nname: middleware") {
		t.Fatalf("installed skill differs from the embedded one:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "homes", "a.test", ".gemini")); err == nil {
		t.Fatal("a claude-style provider got Antigravity's copy of the skill")
	}
	if strings.HasPrefix(p.last().System, SkillHint) {
		t.Fatal("the hint is for providers that do not load skills")
	}
}

func TestAgyGetsTheSkillWhereItLoadsSkillsAndAHintInThePreamble(t *testing.T) {
	p := &fakeProvider{key: "agy", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	os.WriteFile(filepath.Join(root, "sites", "b.test", "site.json"), []byte(`{"version":2,"agent":{"provider":"agy"}}`), 0o644)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("b.test", "hi", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	got, err := os.ReadFile(filepath.Join(root, "data", "homes", "b.test", ".gemini", "config", "skills", "middleware", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, SkillContent()) {
		t.Fatalf("Antigravity's SKILL.md differs from the skill:\n%s", got)
	}
	if !strings.HasPrefix(p.last().System, SkillHint) {
		t.Fatalf("preamble lacks the hint:\n%s", p.last().System)
	}
	// Told to read a file outside the folder and also never to leave it,
	// Codex chose the second: the rule has to name the exception.
	if !strings.Contains(p.last().System, "The one exception is the house's guide, ~/.claude/skills/middleware/SKILL.md") {
		t.Fatalf("the rule about the folder does not name the guide:\n%s", p.last().System)
	}
}

func TestInstallSkillWritesOnceAndLeavesAnUnchangedFileAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude", "skills", "middleware")
	if err := InstallSkill(dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	// An unchanged file is not rewritten: the mtime stays, which is what
	// stops every turn from churning the home.
	time.Sleep(20 * time.Millisecond)
	if err := InstallSkill(dir); err != nil {
		t.Fatal(err)
	}
	if st2, _ := os.Stat(file); !st2.ModTime().Equal(st.ModTime()) {
		t.Fatal("an unchanged skill was rewritten")
	}
	os.WriteFile(file, []byte("stale"), 0o644)
	if err := InstallSkill(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, SkillContent()) {
		t.Fatalf("a stale skill was not replaced:\n%s", got)
	}
}

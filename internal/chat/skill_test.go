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
	// And in the CLI's own folder, which is where Claude Code now loads
	// skills from (its CLAUDE_CONFIG_DIR).
	if got, err := os.ReadFile(filepath.Join(root, "data", "agents", "config", "a.test", "fake", "skills", "middleware", "SKILL.md")); err != nil || !bytes.Equal(got, SkillContent()) {
		t.Fatalf("the skill is not in the CLI's folder: %v", err)
	}
	if p.last().Jail.Config != filepath.Join(root, "data", "agents", "config", "a.test") {
		t.Fatalf("the jail's config folder: %q", p.last().Jail.Config)
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
	// By its full path: told "~/.claude/…", an agent on a V2 node went for
	// the root's .claude, which is denied.
	if !strings.Contains(p.last().System, "The house's guide, "+filepath.Join(root, "data", "homes", "b.test", ".claude", "skills", "middleware", "SKILL.md")+", which you may always read") {
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

// A site written with Claude keeps its rules for the next agent: one that
// does not read CLAUDE.md is told to, unless the project has an AGENTS.md
// (which it reads by itself).
func TestAnAgentThatDoesNotReadClaudeMdIsToldToReadTheProjects(t *testing.T) {
	p := &fakeProvider{key: "agy", script: []agent.Event{{Kind: agent.KindResult, Text: "ok"}}}
	m, root, bus := newManager(t, p)
	site := filepath.Join(root, "sites", "b.test")
	os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"version":2,"agent":{"provider":"agy"}}`), 0o644)
	os.WriteFile(filepath.Join(site, "CLAUDE.md"), []byte("# rules\n"), 0o644)
	ch, unsub := bus.Subscribe()
	defer unsub()
	if _, err := m.Send("b.test", "hi", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if !strings.Contains(p.last().System, "instructions are in CLAUDE.md") {
		t.Fatalf("no hint:\n%s", p.last().System)
	}
	os.WriteFile(filepath.Join(site, "AGENTS.md"), []byte("# rules\n"), 0o644)
	if _, err := m.Send("b.test", "again", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "agent.done")
	if strings.Contains(p.last().System, "instructions are in CLAUDE.md") {
		t.Fatal("told to read CLAUDE.md with an AGENTS.md there")
	}
}

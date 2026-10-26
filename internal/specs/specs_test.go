package specs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAProjectWithoutTheFolderIsNotSetUpAndThatIsNoError(t *testing.T) {
	o, err := Read(t.TempDir())
	if err != nil || o.Initialized || o.Changes == nil || o.Specs == nil {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestTheFolderIsReadAsTheAgentWroteIt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "openspec/config.yaml", "schema: spec-driven\n")
	write(t, dir, "openspec/changes/add-dark-mode/proposal.md", "# Proposal: Dark mode\n\n## Why\n\nVisitors read at night\nand the page glares.\n\n## What Changes\n\n- a toggle\n")
	write(t, dir, "openspec/changes/add-dark-mode/tasks.md", "## 1. Theme\n\n- [x] 1.1 Colours\n- [ X ] 1.2 Toggle\n- [ ] 1.3 Remember it\n- [~] 1.4 Half done is not done\n- a plain bullet is no task\n")
	write(t, dir, "openspec/changes/add-dark-mode/specs/ui/theme/spec.md", "## ADDED Requirements\n")
	write(t, dir, "openspec/changes/archive/2026-10-01-first-page/proposal.md", "# First page\n")
	write(t, dir, "openspec/specs/ui/theme/spec.md", "# Theme\n\n### Requirement: Light\n\n### Requirement: Contrast\n")
	o, err := Read(dir)
	if err != nil || !o.Initialized {
		t.Fatal(err)
	}
	if len(o.Changes) != 1 {
		t.Fatalf("changes: %+v", o.Changes)
	}
	c := o.Changes[0]
	if c.ID != "add-dark-mode" || c.Title != "Dark mode" || c.Why != "Visitors read at night and the page glares." || c.Done != 2 || c.Total != 4 {
		t.Fatalf("the change: %+v", c)
	}
	if strings.Join(c.Files, ",") != "openspec/changes/add-dark-mode/proposal.md,openspec/changes/add-dark-mode/tasks.md,openspec/changes/add-dark-mode/specs/ui/theme/spec.md" {
		t.Fatalf("files: %v", c.Files)
	}
	if len(o.Archived) != 1 || o.Archived[0].ID != "first-page" || o.Archived[0].Date != "2026-10-01" {
		t.Fatalf("archived: %+v", o.Archived)
	}
	if len(o.Specs) != 1 || o.Specs[0].Capability != "ui/theme" || o.Specs[0].Title != "Theme" || o.Specs[0].Requirements != 2 {
		t.Fatalf("specs: %+v", o.Specs)
	}
}

// fakeCLI is an openspec that writes down how it was run and makes the
// folder, as the real one does.
func fakeCLI(t *testing.T) (bin, log string) {
	t.Helper()
	d := t.TempDir()
	log = filepath.Join(d, "ran")
	bin = filepath.Join(d, "openspec")
	script := "#!/bin/sh\necho \"$@\" > " + log + "\nenv >> " + log + "\nmkdir -p openspec/specs\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestInitWritesTheSkillsForEveryAgentTheHouseRunsAndReportsNothing(t *testing.T) {
	bin, log := fakeCLI(t)
	dir := t.TempDir()
	if _, err := Init(context.Background(), dir, InitOptions{Bin: bin, Home: t.TempDir(), Path: "/usr/bin:/bin", Agents: []string{"openai", "claude", "agy", "nobody"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(log)
	got := string(raw)
	if !strings.HasPrefix(got, "init --tools claude,codex,antigravity --no-animation .") {
		t.Fatalf("ran: %s", got)
	}
	if !strings.Contains(got, "OPENSPEC_TELEMETRY=0") || !strings.Contains(got, "DO_NOT_TRACK=1") {
		t.Fatalf("telemetry was not turned off:\n%s", got)
	}
	// A project already set up is left alone: init over it would rewrite
	// OpenSpec's skills, which is update's job.
	os.Remove(log)
	if _, err := Init(context.Background(), dir, InitOptions{Bin: bin, Home: t.TempDir(), Path: "/usr/bin:/bin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("init ran over an existing openspec/")
	}
}

// TestTheRealCLISetsAProjectUpThatThisPackageReads runs OpenSpec itself
// (OPENSPEC=1, with openspec on PATH): the house reads the folder by
// hand, and only the real CLI can say the layout is still the one read.
func TestTheRealCLISetsAProjectUpThatThisPackageReads(t *testing.T) {
	if os.Getenv("OPENSPEC") != "1" {
		t.Skip("OPENSPEC=1 runs the real openspec")
	}
	bin, err := exec.LookPath("openspec")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if out, err := Init(context.Background(), dir, InitOptions{Bin: bin, Home: t.TempDir(), Path: os.Getenv("PATH")}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "skills", "openspec-propose", "SKILL.md")); err != nil {
		t.Fatalf("no skill for Claude Code: %v", err)
	}
	cmd := exec.Command(bin, "new", "change", "add-thing")
	cmd.Dir, cmd.Env = dir, append(os.Environ(), "OPENSPEC_TELEMETRY=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	write(t, dir, "openspec/changes/add-thing/tasks.md", "- [x] 1.1 One\n- [ ] 1.2 Two\n")
	o, err := Read(dir)
	if err != nil || !o.Initialized || len(o.Changes) != 1 || o.Changes[0].ID != "add-thing" || o.Changes[0].Done != 1 || o.Changes[0].Total != 2 {
		t.Fatalf("%+v %v", o, err)
	}
}

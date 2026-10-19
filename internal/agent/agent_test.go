package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{
		"sites/a.test", "sites/b.test", "data/homes/a.test", "data/homes/b.test",
		"data/state", "data/events", "tasks", "modules",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "data", "settings.json"), []byte("{}"), 0o644)
	return root
}

func TestASiteJailDeniesSiblingsDataAndTheRootButNotItself(t *testing.T) {
	root := makeRoot(t)
	site := filepath.Join(root, "sites", "a.test")
	home := filepath.Join(root, "data", "homes", "a.test")
	j := NewJail(root, site, home)
	denied := strings.Join(j.Deny, "\n")
	for _, want := range []string{
		filepath.Join(root, "sites", "b.test"),
		filepath.Join(root, "data", "homes", "b.test"),
		filepath.Join(root, "data", "state"),
		filepath.Join(root, "data", "settings.json"),
		filepath.Join(root, "tasks"),
		filepath.Join(root, "modules"),
	} {
		if !strings.Contains(denied, want) {
			t.Errorf("missing %s in deny list:\n%s", want, denied)
		}
	}
	for _, d := range j.Deny {
		if d == site || d == home || strings.HasPrefix(d, site+"/") || d == root {
			t.Errorf("the jail denies its own ground: %s", d)
		}
	}
}

func TestTheMiddlewareJailDeniesOnlyTheSitesFolder(t *testing.T) {
	root := makeRoot(t)
	j := NewJail(root, root, filepath.Join(root, "data", "homes", "middleware"))
	if len(j.Deny) != 1 || j.Deny[0] != filepath.Join(root, "sites") {
		t.Fatalf("deny = %v", j.Deny)
	}
}

func TestTheAuditFindsADeniedPathInATranscript(t *testing.T) {
	root := makeRoot(t)
	site := filepath.Join(root, "sites", "a.test")
	j := NewJail(root, site, filepath.Join(root, "data", "homes", "a.test"))
	evs := []Event{
		{Kind: KindText, Text: "Let me look around."},
		{Kind: KindTool, Tool: "Read", Input: map[string]any{"file_path": filepath.Join(root, "sites", "b.test", ".env")}},
		{Kind: KindTool, Tool: "Read", Input: map[string]any{"file_path": filepath.Join(site, "README.md")}},
	}
	v := j.Violations(Transcript(evs))
	if len(v) != 1 || v[0] != filepath.Join(root, "sites", "b.test") {
		t.Fatalf("violations = %v", v)
	}
}

func TestTheJailEnvironmentHasHomeInsideAndNoDuplicates(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	j := Jail{SiteDir: "/s", Home: "/h"}
	env := j.Env(map[string]string{"HOME": "/elsewhere", "FOO": "bar"})
	seen := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if seen[k] {
			t.Fatalf("duplicate %s in %v", k, env)
		}
		seen[k] = true
	}
	if !contains(env, "HOME=/h") || !contains(env, "FOO=bar") || !contains(env, "PATH=/usr/bin") {
		t.Fatalf("env = %v", env)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestTheLedgerSumsByDayAndEnforcesTheMonthlyCap(t *testing.T) {
	l := NewLedger(t.TempDir())
	day1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	lastMonth := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if err := l.Add("a.test", 0.4, day1); err != nil {
		t.Fatal(err)
	}
	if err := l.Add("a.test", 0.7, day2); err != nil {
		t.Fatal(err)
	}
	if err := l.Add("a.test", 5, lastMonth); err != nil {
		t.Fatal(err)
	}
	_ = l.Add("a.test", 0, day2) // ignored
	if got := l.Month("a.test", day2); got < 1.09 || got > 1.11 {
		t.Fatalf("month = %v", got)
	}
	if got := l.Total("a.test"); got < 6.09 || got > 6.11 {
		t.Fatalf("total = %v", got)
	}
	if err := l.Allow("a.test", 2, day2); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	err := l.Allow("a.test", 1, day2)
	if _, ok := err.(*ErrOverCap); !ok {
		t.Fatalf("want ErrOverCap, got %v", err)
	}
	if err := l.Allow("a.test", 0, day2); err != nil {
		t.Fatalf("no cap means allowed: %v", err)
	}
}

func TestTheMonthlyCapIsReadFromTheRawManifest(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"agent":{"provider":"claude","monthly_cap_usd":12.5}}`), 0o644)
	if got := MonthlyCap(dir); got != 12.5 {
		t.Fatalf("cap = %v", got)
	}
	if got := MonthlyCap(t.TempDir()); got != 0 {
		t.Fatalf("missing manifest: cap = %v", got)
	}
}

type echoParser struct{ lines []string }

func (p *echoParser) Line(line string) ([]Event, bool) {
	p.lines = append(p.lines, line)
	if line == "END" {
		return []Event{{Kind: KindResult, Text: "ok"}}, true
	}
	return []Event{{Kind: KindText, Text: line}}, false
}

func (p *echoParser) Flush() []Event { return nil }

func TestACLIRunEndsOnTheResultLineEvenIfAGrandchildKeepsStdout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	// The grandchild inherits stdout and sleeps far longer than the test:
	// the run must still end when the parser sees END.
	os.WriteFile(script, []byte("#!/bin/sh\necho hello\nsleep 300 &\necho END\nsleep 300\n"), 0o755)
	start := time.Now()
	run, err := StartCLI(context.Background(), &echoParser{}, dir, Jail{Home: dir}.Env(nil), script)
	if err != nil {
		t.Fatal(err)
	}
	evs := Collect(context.Background(), run)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if len(evs) != 2 || evs[0].Text != "hello" || evs[1].Kind != KindResult {
		t.Fatalf("events = %+v", evs)
	}
}

func TestACLIRunThatExitsWithoutAResultReportsAnErrorWithStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho partial\necho 'not logged in' >&2\nexit 1\n"), 0o755)
	run, err := StartCLI(context.Background(), &echoParser{}, dir, Jail{Home: dir}.Env(nil), script)
	if err != nil {
		t.Fatal(err)
	}
	evs := Collect(context.Background(), run)
	last := evs[len(evs)-1]
	if last.Kind != KindError || !strings.Contains(last.Text, "not logged in") || !strings.Contains(last.Text, "exit status 1") {
		t.Fatalf("events = %+v", evs)
	}
}

func TestStopEndsACLIRunAndItsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho started\nsleep 300\n"), 0o755)
	run, err := StartCLI(context.Background(), &echoParser{}, dir, Jail{Home: dir}.Env(nil), script)
	if err != nil {
		t.Fatal(err)
	}
	<-run.Events()
	run.Stop()
	run.Stop() // twice is fine
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-run.Events():
			if !ok {
				return
			}
			if ev.Kind == KindError && ev.Text != "stopped" {
				t.Fatalf("unexpected %+v", ev)
			}
		case <-deadline:
			t.Fatal("the run did not end after Stop")
		}
	}
}

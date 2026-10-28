package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/events"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// world is a bare remote, a site whose folder is a clone of it, and
// another clone standing for whoever pushes from elsewhere.
func world(t *testing.T) (m *Manager, site, other string, bus *events.Bus) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	remote := filepath.Join(root, "remote.git")
	run(t, root, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(root, "seed")
	run(t, root, "clone", remote, seed)
	os.WriteFile(filepath.Join(seed, "index.html"), []byte("one\n"), 0o644)
	run(t, seed, "add", ".")
	run(t, seed, "commit", "-m", "one")
	run(t, seed, "push", "origin", "HEAD:main")
	site = filepath.Join(root, "sites", "s.example")
	os.MkdirAll(filepath.Dir(site), 0o755)
	run(t, root, "clone", remote, site)
	other = seed
	bus = events.New("")
	m = New(Options{DataDir: filepath.Join(root, "data"), Bus: bus, Sites: func() []Site { return []Site{{Domain: "s.example", Dir: site}} }})
	return
}

func push(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644)
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-m", msg)
	run(t, dir, "push", "origin", "HEAD:main")
}

func TestAPullBringsWhatWasPushedElsewhere(t *testing.T) {
	m, site, other, _ := world(t)
	ctx := context.Background()
	list := m.List(ctx)
	if len(list) != 1 || list[0].Path != "." || list[0].Branch != "main" || list[0].Upstream != "origin/main" || list[0].Head.Subject != "one" {
		t.Fatalf("list: %+v", list)
	}
	push(t, other, "index.html", "two\n", "two")
	r, err := m.SetAuto(ctx, list[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Auto || r.Last == nil || !r.Last.OK || r.Last.Pulled != 1 || r.Head.Subject != "two" {
		t.Fatalf("after marking: %+v %+v", r, r.Last)
	}
	if b, _ := os.ReadFile(filepath.Join(site, "index.html")); string(b) != "two\n" {
		t.Fatalf("the folder has %q", b)
	}
	// The mark survives a restart.
	m2 := New(m.opts)
	if !m2.List(ctx)[0].Auto {
		t.Fatal("the mark was lost")
	}
}

func TestAPullThatWouldOverwriteWorkIsUndoneAndSaid(t *testing.T) {
	m, site, other, bus := world(t)
	ctx := context.Background()
	id := m.List(ctx)[0].ID
	push(t, other, "index.html", "theirs\n", "theirs")
	// Someone (a conversation) is editing the same file here.
	os.WriteFile(filepath.Join(site, "index.html"), []byte("mine, unsaved\n"), 0o644)
	r, _ := m.Pull(ctx, id)
	if r.Last == nil || r.Last.OK || r.Last.Failures != 1 {
		t.Fatalf("result: %+v", r.Last)
	}
	if b, _ := os.ReadFile(filepath.Join(site, "index.html")); string(b) != "mine, unsaved\n" {
		t.Fatalf("the work in progress was touched: %q", b)
	}
	r, _ = m.Pull(ctx, id)
	if r.Last.Failures != 2 {
		t.Fatalf("the streak: %+v", r.Last)
	}
	failed := 0
	for _, e := range bus.Recent() {
		if e.Type == "repo.failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("%d repo.failed events for one streak, want 1", failed)
	}
	// A real conflict: committed here and there, merge aborted, no
	// half-merged files left behind.
	run(t, site, "commit", "-am", "mine")
	r, _ = m.Pull(ctx, id)
	if r.Last.OK || !strings.Contains(r.Last.Message, "undone") {
		t.Fatalf("conflict: %+v", r.Last)
	}
	if st := run(t, site, "status", "--porcelain"); st != "" {
		t.Fatalf("a merge was left half done:\n%s", st)
	}
}

func TestReposAreFoundBelowButNotInDependenciesOrThroughLinks(t *testing.T) {
	m, site, _, _ := world(t)
	nested := filepath.Join(site, "build", "app")
	os.MkdirAll(nested, 0o755)
	run(t, nested, "init", "-b", "main")
	dep := filepath.Join(site, "node_modules", "lib")
	os.MkdirAll(dep, 0o755)
	run(t, dep, "init")
	outside := t.TempDir()
	run(t, outside, "init")
	os.Symlink(outside, filepath.Join(site, "linked"))
	var paths []string
	for _, r := range m.List(context.Background()) {
		paths = append(paths, r.Path)
	}
	if strings.Join(paths, ",") != ".,build/app" {
		t.Fatalf("found %v", paths)
	}
	if _, err := m.Pull(context.Background(), "not-an-id"); err != ErrNotFound {
		t.Fatalf("an unknown id: %v", err)
	}
}

func TestCredentialsInARemoteAreNotShown(t *testing.T) {
	if got := cleanRemote("https://user:ghp_secret@github.com/a/b.git"); got != "https://github.com/a/b.git" {
		t.Fatalf("got %q", got)
	}
	if got := cleanRemote("git@github.com:a/b.git"); got != "git@github.com:a/b.git" {
		t.Fatalf("got %q", got)
	}
}

// The watcher runs git in folders a project or a conversation works in, so
// a command-bearing key or a hook planted there (a compromised container,
// or a pushed repo, can write them) must never run on the host. Here a
// filter smudge, a core.fsmonitor and a post-checkout hook each try to drop
// a marker; after a listing and a pull the marker must not exist, and the
// repository shows the refusal instead of running it.
func TestARefreshDoesNotRunACommandFromTheRepository(t *testing.T) {
	m, site, other, _ := world(t)
	ctx := context.Background()
	id := m.List(ctx)[0].ID
	push(t, other, "index.html", "two\n", "two") // something to pull

	marker := filepath.Join(t.TempDir(), "pwned")
	hook := filepath.Join(site, ".git", "hooks", "post-checkout")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	cfg := filepath.Join(site, ".git", "config")
	data, _ := os.ReadFile(cfg)
	extra := "[filter \"evil\"]\n\tsmudge = touch " + marker + "\n[core]\n\tfsmonitor = touch " + marker + "\n"
	os.WriteFile(cfg, append(data, []byte(extra)...), 0o644)

	list := m.List(ctx)
	if list[0].Error == "" || !strings.Contains(list[0].Error, "filter.evil.smudge") {
		t.Fatalf("describe must surface the refusal, got %q", list[0].Error)
	}
	r, _ := m.Pull(ctx, id)
	if r.Last == nil || r.Last.OK || !strings.Contains(r.Last.Message, "filter.evil.smudge") {
		t.Fatalf("a pull on an unsafe repo must be refused: %+v", r.Last)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command from the repository ran during the refresh")
	}
}

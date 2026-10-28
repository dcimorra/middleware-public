package hostgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig lays down dir/.git/config with the given body.
func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGuardRefusesEveryCommandBearingKey(t *testing.T) {
	bad := map[string]string{
		"a filter smudge":   "[filter \"evil\"]\n\tsmudge = /tmp/pwn\n",
		"a filter clean":    "[filter \"evil\"]\n\tclean = /tmp/pwn\n",
		"a filter process":  "[filter \"evil\"]\n\tprocess = /tmp/pwn\n",
		"a diff textconv":   "[diff \"evil\"]\n\ttextconv = /tmp/pwn\n",
		"a url credential":  "[credential \"https://x\"]\n\thelper = /tmp/pwn\n",
		"gpg.program":       "[gpg]\n\tprogram = /tmp/pwn\n",
		"a ssh gpg program": "[gpg \"ssh\"]\n\tprogram = /tmp/pwn\n",
		"packObjectsHook":   "[uploadpack]\n\tpackObjectsHook = /tmp/pwn\n",
		// git folds section and key case; a config that shouts must not slip by.
		"a case-folded key": "[FILTER \"evil\"]\n\tSMUDGE = /tmp/pwn\n",
	}

	for name, body := range bad {
		dir := t.TempDir()
		writeConfig(t, dir, body)
		if err := Guard(dir); err == nil {
			t.Errorf("%s: Guard let it through:\n%s", name, body)
		}
	}
}

func TestGuardPassesAnOrdinaryConfig(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = https://github.com/a/b.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n[branch \"main\"]\n\tremote = origin\n")
	if err := Guard(dir); err != nil {
		t.Fatalf("an ordinary config was refused: %v", err)
	}
}

func TestGuardFollowsIncludesInsideGitAndRefusesThoseOutside(t *testing.T) {
	// An include that stays inside .git is followed, so a key hidden there
	// is still caught.
	dir := t.TempDir()
	writeConfig(t, dir, "[include]\n\tpath = extra\n")
	if err := os.WriteFile(filepath.Join(dir, ".git", "extra"), []byte("[filter \"e\"]\n\tsmudge = /tmp/pwn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Guard(dir); err == nil {
		t.Fatal("a key in an included file was not caught")
	}

	// An include pointing outside .git is refused outright: its file is
	// not git's own and cannot be vetted.
	other := t.TempDir()
	writeConfig(t, other, "[includeIf \"gitdir:/x\"]\n\tpath = /etc/evil\n")
	if err := Guard(other); err == nil {
		t.Fatal("an include outside .git was allowed")
	}
}

func TestGuardIsSilentWithoutAGit(t *testing.T) {
	if err := Guard(t.TempDir()); err != nil {
		t.Fatalf("a folder with no .git must be fine: %v", err)
	}
}

func TestEnvCarriesNoSecret(t *testing.T) {
	t.Setenv("MIDDLEWARE_SECRET_KEY", "super-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "also-secret")
	t.Setenv("PATH", "/usr/bin:/bin")
	env := Env(Options{Home: "/home/site"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "SECRET_KEY") {
		t.Fatalf("a secret reached git's environment:\n%s", joined)
	}
	for _, want := range []string{"HOME=/home/site", "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env is missing %q:\n%s", want, joined)
		}
	}
}

func TestEnvPassesThroughOnlyWhatIsAsked(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/run/agent.sock")
	t.Setenv("MIDDLEWARE_SECRET_KEY", "secret")
	env := strings.Join(Env(Options{Home: "/h", Passthrough: []string{"SSH_AUTH_SOCK"}}), "\n")
	if !strings.Contains(env, "SSH_AUTH_SOCK=/run/agent.sock") {
		t.Fatal("an ssh-agent socket must pass through for ssh pulls")
	}
	if strings.Contains(env, "secret") {
		t.Fatal("passthrough widened to a secret")
	}
}

// A repository that sets the single-valued command keys itself (husky
// writes core.hooksPath=.husky on every npm install) is let through, and
// none of them runs: the -c overrides beat repository config.
func TestCoreCommandKeysAreOverriddenNotRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "one")
	hooks := filepath.Join(dir, ".husky")
	os.MkdirAll(hooks, 0o755)
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	for _, name := range []string{"post-checkout", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WriteString("[core]\n\thooksPath = .husky\n\tfsmonitor = touch " + marker + "\n\tpager = touch " + marker + "\n")
	cfg.Close()
	if err := Guard(dir); err != nil {
		t.Fatalf("a husky-style config was refused: %v", err)
	}
	for _, args := range [][]string{{"status"}, {"checkout", "-q", "-B", "other"}, {"log", "-1"}} {
		cmd, err := Command(context.Background(), Options{Dir: dir, Home: t.TempDir()}, args...)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command the repository named ran on the host")
	}
}

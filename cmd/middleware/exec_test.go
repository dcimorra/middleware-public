package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/chat"
)

// TestExecHelper runs `middleware exec` with the arguments in
// MW_EXEC_ARGS, in a process of its own: what is checked is the chain of
// processes above it, which the test's own would not have.
func TestExecHelper(t *testing.T) {
	args := os.Getenv("MW_EXEC_ARGS")
	if args == "" {
		t.Skip("only run as the child of another test")
	}
	if err := execCommand(strings.Split(args, "\x1f")); err != nil {
		os.Stdout.WriteString("exec error: " + err.Error() + "\n")
		os.Exit(3)
	}
	os.Stdout.WriteString("exec ran\n")
}

// From under a site's turn, exec runs in that site and refuses any
// other: its mark is on a shell above the command, which itself has
// scrubbed it, as an agent's Bash call can.
func TestExecFromASiteTurnRunsOnlyInItsSite(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a.example", "b.example"} {
		dir := filepath.Join(root, "sites", d)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2}`), 0o644)
	}
	run := func(mark string, domain string) (string, bool) {
		t.Helper()
		args := strings.Join([]string{"--root", root, domain, "--", "true"}, "\x1f")
		// `; exit $?` keeps the shell from exec'ing the command in its
		// place, so the marked shell stays its parent.
		cmd := exec.Command("sh", "-c", `env -u `+chat.MarkVar+` "$@"; exit $?`, "sh", os.Args[0], "-test.run=^TestExecHelper$")
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, chat.MarkVar+"=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "MW_EXEC_ARGS="+args)
		if mark != "" {
			cmd.Env = append(cmd.Env, chat.MarkVar+"="+mark)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err == nil && strings.Contains(string(out), "exec ran")
	}
	if out, ok := run("a.example", "b.example"); ok || !strings.Contains(out, "exec refused") {
		t.Fatalf("a.example's turn ran exec in b.example:\n%s", out)
	}
	if out, ok := run("a.example#00ff", "b.example"); ok || !strings.Contains(out, "exec refused") {
		t.Fatalf("a.example's door turn ran exec in b.example:\n%s", out)
	}
	if out, ok := run("a.example", "a.example"); !ok {
		t.Fatalf("a.example's turn could not exec in its own site:\n%s", out)
	}
	if out, ok := run(chat.Middleware, "b.example"); !ok {
		t.Fatalf("the house's turn could not exec in a site:\n%s", out)
	}
	if out, ok := run("", "b.example"); !ok {
		t.Fatalf("the operator could not exec in a site:\n%s", out)
	}
}

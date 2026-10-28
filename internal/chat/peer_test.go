package chat

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// SiteOfProcess finds a site's turn above a process, by the running turns
// or by the mark, and a forged mark can neither lift it nor point it at
// another site.
func TestASiteTurnIsFoundAboveAProcess(t *testing.T) {
	start := func(env ...string) int {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, MarkVar+"=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd.Process.Pid
	}
	self := os.Getpid()
	cases := []struct {
		name  string
		mark  string
		turns map[int]string
		want  string
		fails bool
	}{
		{name: "nothing", want: ""},
		{name: "the turn above", turns: map[int]string{self: "a.test"}, want: "a.test"},
		{name: "a door conversation's turn", turns: map[int]string{self: "a.test#00ff"}, want: "a.test"},
		{name: "the mark alone", mark: "a.test", want: "a.test"},
		{name: "a house mark under a site's turn", mark: Middleware, turns: map[int]string{self: "a.test"}, want: "a.test"},
		{name: "another site's mark under a site's turn", mark: "b.test", turns: map[int]string{self: "a.test"}, fails: true},
		{name: "the house's turn", turns: map[int]string{self: Middleware}, want: ""},
		{name: "a mark that names no site", mark: "../etc", want: ""},
	}
	for _, c := range cases {
		var env []string
		if c.mark != "" {
			env = append(env, MarkVar+"="+c.mark)
		}
		pid := start(env...)
		got, err := SiteOfProcess("/proc", pid, c.turns)
		if c.fails {
			if err == nil {
				t.Errorf("%s: %q, want an error", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	// A process gone before it was looked at cannot be told.
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := SiteOfProcess("/proc", gone.Process.Pid, nil); err == nil {
		t.Fatal("a process that is gone was told apart")
	}
}

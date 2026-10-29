package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// With an API key, CODEX_HOME is the house's folder, not the site's HOME
// (which its container writes): a config.toml planted there is not what
// the next turn reads, and the sessions kept there come over once.
func TestCodexHomeIsTheHousesFolderNotTheSites(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	site := filepath.Join(root, "sites", "a.example")
	home := filepath.Join(root, "data", "homes", "a.example")
	os.MkdirAll(site, 0o755)
	os.MkdirAll(filepath.Join(home, ".codex", "sessions", "2026"), 0o700)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("notify = [\"touch\", \"/tmp/pwned\"]\n"), 0o600)
	os.WriteFile(filepath.Join(home, ".codex", "sessions", "2026", "rollout-t1.jsonl"), []byte("{}"), 0o600)
	envFile := filepath.Join(dir, "env")
	bin := filepath.Join(dir, "codex")
	os.WriteFile(bin, []byte("#!/bin/sh\nenv > "+envFile+"\necho '{\"type\":\"turn.completed\",\"usage\":{}}'\n"), 0o755)

	run, err := New(Config{Bin: bin, APIKey: "sk-test"}).Start(context.Background(), agent.Request{Workdir: site, Prompt: "hi", Jail: agent.NewJail(root, site, home)})
	if err != nil {
		t.Fatal(err)
	}
	agent.Collect(context.Background(), run)
	want := filepath.Join(root, "data", "agents", "config", "a.example", "openai")
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "CODEX_HOME="+want+"\n") {
		t.Fatalf("CODEX_HOME is not the house's folder:\n%s", env)
	}
	if _, err := os.Stat(filepath.Join(want, "config.toml")); err == nil {
		t.Fatal("the site's config.toml came over")
	}
	if _, err := os.Stat(filepath.Join(want, "sessions", "2026", "rollout-t1.jsonl")); err != nil {
		t.Fatalf("the sessions did not come over: %v", err)
	}
}

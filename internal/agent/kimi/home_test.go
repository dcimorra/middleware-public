package kimi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// KIMI_CODE_HOME is the house's folder, not one under the site's HOME
// (which its container writes); what a conversation kept there comes
// over once, and the config is the house's own.
func TestKimiHomeIsTheHousesFolderNotTheSites(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	site := filepath.Join(root, "sites", "a.example")
	home := filepath.Join(root, "data", "homes", "a.example")
	os.MkdirAll(site, 0o755)
	old := filepath.Join(home, ".kimi-code", "a.example")
	os.MkdirAll(filepath.Join(old, "sessions"), 0o700)
	os.WriteFile(filepath.Join(old, "sessions", "last.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(old, "config.toml"), []byte("planted"), 0o600)
	envFile := filepath.Join(dir, "env")
	bin := filepath.Join(dir, "kimi")
	os.WriteFile(bin, []byte("#!/bin/sh\nenv > "+envFile+"\n"), 0o755)

	run, err := New(Config{Bin: bin, APIKey: "sk-test"}).Start(context.Background(), agent.Request{Workdir: site, Prompt: "hi", Name: "a.example", Jail: agent.NewJail(root, site, home)})
	if err != nil {
		t.Fatal(err)
	}
	agent.Collect(context.Background(), run)
	want := filepath.Join(root, "data", "agents", "config", "a.example", "kimi", "a.example")
	env, _ := os.ReadFile(envFile)
	if !strings.Contains(string(env), "KIMI_CODE_HOME="+want+"\n") {
		t.Fatalf("KIMI_CODE_HOME is not the house's folder:\n%s", env)
	}
	if cfg, _ := os.ReadFile(filepath.Join(want, "config.toml")); strings.Contains(string(cfg), "planted") || !strings.Contains(string(cfg), "KIMI_API_KEY") {
		t.Fatalf("config: %s", cfg)
	}
	if _, err := os.Stat(filepath.Join(want, "sessions", "last.json")); err != nil {
		t.Fatalf("the sessions did not come over: %v", err)
	}
}

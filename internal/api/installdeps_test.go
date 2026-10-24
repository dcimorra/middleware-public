package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A site whose start builds needs its dev tools, also on a machine that
// says production: a moved site arrived without tailwindcss and its start
// failed with "not found". With a real npm, a dev dependency from a local
// folder (no network), and NODE_ENV=production in the house's environment.
func TestASitesDevToolsAreInstalledEvenWhereTheMachineSaysProduction(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("needs npm")
	}
	t.Setenv("NODE_ENV", "production")
	t.Setenv("npm_config_omit", "dev")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tool"), 0o755)
	os.WriteFile(filepath.Join(dir, "tool", "package.json"), []byte(`{"name":"tool","version":"1.0.0","bin":{"tool":"cli.js"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "tool", "cli.js"), []byte("#!/usr/bin/env node\nconsole.log('built')\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"site","version":"1.0.0","scripts":{"build":"tool"},"devDependencies":{"tool":"file:./tool"}}`), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	said, err := installDeps(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "tool")); err != nil {
		t.Fatalf("the dev tool was left out; the install said %q", said)
	}
	if !strings.Contains(said, "--include=dev") || !strings.Contains(said, "programs in node_modules/.bin") {
		t.Fatalf("what the install said: %q", said)
	}
}

package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/agent/claude"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/modules"
)

// fakeCLI writes an executable that answers --version, so the probe runs
// a real process and not a stub of one.
func fakeCLI(t *testing.T, name, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo " + version + "; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyRunsTheBinaryAndSaysTheKeyIsNotProbed(t *testing.T) {
	m := Claude(agent.NewRegistry())
	bin := fakeCLI(t, "claude", "1.2.3")
	lines := m.Verify(context.Background(), modules.Config{"bin": bin, "api_key": "sk-x"})
	if modules.Failed(lines) {
		t.Fatalf("verify failed: %v", lines)
	}
	var sawVersion, sawSkip bool
	for _, l := range lines {
		if l.Status == modules.StatusOK && strings.Contains(l.Text, "1.2.3") {
			sawVersion = true
		}
		if l.Status == modules.StatusSkip && strings.Contains(l.Text, "not probed") {
			sawSkip = true
		}
	}
	if !sawVersion || !sawSkip {
		t.Fatalf("lines: %v", lines)
	}
}

func TestVerifyFailsWhenTheBinaryIsMissing(t *testing.T) {
	m := Gemini(agent.NewRegistry())
	m.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	lines := m.Verify(context.Background(), modules.Config{})
	if !modules.Failed(lines) {
		t.Fatalf("a missing binary must fail: %v", lines)
	}
}

func TestEnableReplacesTheProviderAndDisableRestoresIt(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(claude.New(claude.Config{}))
	before, _ := reg.Get("claude")
	m := Claude(reg)
	bin := fakeCLI(t, "claude", "9.9")
	if err := m.Enable(context.Background(), modules.Config{"bin": bin, "model": "opus"}); err != nil {
		t.Fatal(err)
	}
	after, _ := reg.Get("claude")
	if after == before {
		t.Fatal("enable did not replace the provider")
	}
	if models := after.Models(); models[0] != "opus" {
		t.Fatalf("configured model is not the default: %v", models)
	}
	if ok, where := after.Available(); !ok || where != bin {
		t.Fatalf("provider does not use the configured binary: %v %s", ok, where)
	}
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, _ := reg.Get("claude"); p != before {
		t.Fatal("disable did not put the default provider back")
	}
}

func TestRestoreReappliesAnEnabledModuleAfterARestart(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	bin := fakeCLI(t, "gemini", "0.5")
	mgr, err := modules.New(root, events.New(""))
	if err != nil {
		t.Fatal(err)
	}
	reg := agent.NewRegistry()
	mgr.Register(Gemini(reg))
	if _, err := mgr.Enable(context.Background(), GeminiKey, modules.Config{"bin": bin, "model": "gemini-x"}, nil); err != nil {
		t.Fatal(err)
	}

	// A new process: fresh manager over the same store, fresh registry.
	mgr2, err := modules.New(root, events.New(""))
	if err != nil {
		t.Fatal(err)
	}
	reg2 := agent.NewRegistry()
	m2 := Gemini(reg2)
	mgr2.Register(m2)
	if err := Restore(context.Background(), mgr2, m2); err != nil {
		t.Fatal(err)
	}
	p, err := reg2.Get("gemini")
	if err != nil {
		t.Fatal(err)
	}
	if p.Models()[0] != "gemini-x" {
		t.Fatalf("restored provider lost its config: %v", p.Models())
	}
}

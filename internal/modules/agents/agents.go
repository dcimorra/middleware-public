// Package agents holds the two agent modules, agent-claude and
// agent-gemini. Without them the chat runs the vendor CLIs with whatever
// login they have under each site's HOME; enabling one hands the CLI an
// API key, a binary that is not in PATH, a default model or a shared
// config directory, and swaps the provider the conversations use. The
// module never talks to the vendor itself: the only thing it runs is
// `<bin> --version`, so a wrong key is found by the first turn, not by
// the panel, and the panel says so.
package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/agent/claude"
	"github.com/dcimorra/middleware-public/internal/agent/gemini"
	"github.com/dcimorra/middleware-public/internal/modules"
)

// Keys of the two modules.
const (
	ClaudeKey = "agent-claude"
	GeminiKey = "agent-gemini"
)

// Module is one agent module.
type Module struct {
	key, name, desc, bin string
	needs                []modules.Need
	build                func(cfg modules.Config) agent.Provider
	reg                  *agent.Registry
	// fallback is what Disable puts back: the provider the process started
	// with, so turning the module off returns the chat to the CLI's own
	// login rather than to no provider at all.
	fallback agent.Provider

	// LookPath and Version are replaced by tests.
	LookPath func(string) (string, error)
	Version  func(ctx context.Context, bin string) (string, error)
}

// Claude builds the agent-claude module over reg.
func Claude(reg *agent.Registry) *Module {
	m := &Module{
		key:  ClaudeKey,
		name: "Claude agent",
		desc: "Credentials, binary and default model for the Claude CLI the conversations run",
		bin:  "claude",
		needs: []modules.Need{
			{Key: "api_key", Label: "API key", Secret: true, Optional: true, Help: "Passed to the CLI as ANTHROPIC_API_KEY. Leave empty to use the CLI's own login under each site's home."},
			{Key: "bin", Label: "Binary", Optional: true, Help: "Path to the claude executable when it is not in PATH."},
			{Key: "model", Label: "Default model", Optional: true, Help: "Alias or model name offered first in the panel (sonnet, opus, haiku…)."},
			{Key: "config_dir", Label: "Config directory", Optional: true, Help: "Passed as CLAUDE_CONFIG_DIR so every site shares one login instead of needing one per home."},
		},
		reg: reg,
	}
	m.build = func(cfg modules.Config) agent.Provider {
		return claude.New(claude.Config{
			Bin:       cfg["bin"],
			APIKey:    cfg["api_key"],
			ConfigDir: cfg["config_dir"],
			Models:    modelsWith(cfg["model"], claude.New(claude.Config{}).Models()),
		})
	}
	m.init()
	return m
}

// Gemini builds the agent-gemini module over reg.
func Gemini(reg *agent.Registry) *Module {
	m := &Module{
		key:  GeminiKey,
		name: "Gemini agent",
		desc: "Credentials, binary and default model for the Gemini CLI the conversations run",
		bin:  "gemini",
		needs: []modules.Need{
			{Key: "api_key", Label: "API key", Secret: true, Optional: true, Help: "Passed to the CLI as GEMINI_API_KEY. Leave empty to use the CLI's own login under each site's home."},
			{Key: "bin", Label: "Binary", Optional: true, Help: "Path to the gemini executable when it is not in PATH."},
			{Key: "model", Label: "Default model", Optional: true, Help: "Model name offered first in the panel (gemini-2.5-pro, gemini-2.5-flash…)."},
		},
		reg: reg,
	}
	m.build = func(cfg modules.Config) agent.Provider {
		return gemini.New(gemini.Config{
			Bin:    cfg["bin"],
			APIKey: cfg["api_key"],
			Models: modelsWith(cfg["model"], gemini.New(gemini.Config{}).Models()),
		})
	}
	m.init()
	return m
}

func (m *Module) init() {
	m.LookPath = exec.LookPath
	m.Version = runVersion
	if m.reg == nil {
		m.reg = agent.NewRegistry()
	}
	if p, err := m.reg.Get(m.provider()); err == nil {
		m.fallback = p
	} else {
		m.fallback = m.build(modules.Config{})
	}
}

// provider is the registry key the module manages.
func (m *Module) provider() string { return strings.TrimPrefix(m.key, "agent-") }

// Key implements modules.Module.
func (m *Module) Key() string { return m.key }

// Name implements modules.Module.
func (m *Module) Name() string { return m.name }

// Description implements modules.Module.
func (m *Module) Description() string { return m.desc }

// Needs implements modules.Module.
func (m *Module) Needs() []modules.Need { return append([]modules.Need(nil), m.needs...) }

// binary resolves what would run: the configured path, or the default
// name through PATH.
func (m *Module) binary(cfg modules.Config) (string, error) {
	bin := strings.TrimSpace(cfg["bin"])
	if bin == "" {
		bin = m.bin
	}
	if strings.ContainsRune(bin, os.PathSeparator) {
		info, err := os.Stat(bin)
		if err != nil {
			return "", err
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("%s is not an executable file", bin)
		}
		return bin, nil
	}
	return m.LookPath(bin)
}

// Verify implements modules.Module: the binary is found and answers
// --version; the key is said to be unprobed rather than reported as ok.
func (m *Module) Verify(ctx context.Context, cfg modules.Config) []modules.Line {
	var lines []modules.Line
	path, err := m.binary(cfg)
	if err != nil {
		lines = append(lines, modules.Fail(fmt.Sprintf("%s: %v", m.bin, err)))
	} else {
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		v, err := m.Version(vctx, path)
		cancel()
		if err != nil {
			lines = append(lines, modules.Fail(fmt.Sprintf("%s --version: %v", path, err)))
		} else {
			lines = append(lines, modules.OK(fmt.Sprintf("%s answers --version: %s", path, v)))
		}
	}
	if dir := strings.TrimSpace(cfg["config_dir"]); dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			lines = append(lines, modules.Fail("config_dir "+dir+" is not a directory"))
		} else {
			lines = append(lines, modules.OK("config directory "+dir+" exists"))
		}
	}
	if cfg["api_key"] != "" {
		lines = append(lines, modules.Skip("the API key is handed to the CLI and not probed here: a probe would spend, and the first turn says if it is wrong"))
	} else {
		lines = append(lines, modules.Skip("no API key: the CLI's own login under each site's home is used"))
	}
	if model := strings.TrimSpace(cfg["model"]); model != "" {
		lines = append(lines, modules.OK(model+" will be the default model"))
	}
	return lines
}

// Install implements modules.Module. Nothing is downloaded: the vendor
// CLIs are npm packages with their own update channel, and a copy under
// modules/bin would be one more thing to keep current.
func (m *Module) Install(ctx context.Context, cfg modules.Config, progress func(string)) error {
	path, err := m.binary(cfg)
	if err != nil {
		return err
	}
	progress("using " + path)
	return nil
}

// Enable implements modules.Module: the provider the conversations use is
// replaced by one built from cfg.
func (m *Module) Enable(ctx context.Context, cfg modules.Config) error {
	m.reg.Register(m.build(cfg))
	return nil
}

// Disable implements modules.Module: the default provider is put back.
func (m *Module) Disable(ctx context.Context) error {
	m.reg.Register(m.fallback)
	return nil
}

// Routes implements modules.Module.
func (m *Module) Routes() map[string]http.Handler { return nil }

// Restore re-applies every enabled module at start. The manager keeps the
// config and the enabled flag across restarts but calls nobody; without
// this the panel would say "enabled" over a registry still holding the
// defaults, which is the silent failure the modules package warns about.
func Restore(ctx context.Context, mgr *modules.Manager, mods ...*Module) error {
	var errs []error
	for _, m := range mods {
		if !mgr.Enabled(m.Key()) {
			continue
		}
		cfg, err := mgr.Config(m.Key())
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Key(), err))
			continue
		}
		if err := m.Enable(ctx, cfg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Key(), err))
		}
	}
	return errors.Join(errs...)
}

func modelsWith(first string, defaults []string) []string {
	first = strings.TrimSpace(first)
	if first == "" {
		return defaults
	}
	out := []string{first}
	for _, d := range defaults {
		if d != first {
			out = append(out, d)
		}
	}
	return out
}

func runVersion(ctx context.Context, bin string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, "--version")
	// The CLIs read their config from HOME; a version probe must not
	// create one for the service user as a side effect.
	cmd.Env = append(os.Environ(), "HOME="+os.TempDir())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(out.String())
		if text != "" {
			return "", fmt.Errorf("%v: %s", err, text)
		}
		return "", err
	}
	v := strings.TrimSpace(out.String())
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		v = "(no output)"
	}
	return v, nil
}

var _ modules.Module = (*Module)(nil)

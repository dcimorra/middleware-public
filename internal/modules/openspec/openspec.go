// Package openspec is the module that brings OpenSpec to the house: its
// CLI, installed and kept current under modules/node like the agents',
// and with it every new site set up for spec-driven work (an openspec/
// folder and OpenSpec's own skills for the agents), the house's guide
// telling the agents how to use it, and the Specs tab on each site.
//
// A module and not part of the core: the CLI is an npm package with its
// own release channel, and nothing is installed behind the operator's
// back. Enabling the module is the install.
package openspec

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/deps"
	"github.com/dcimorra/middleware-public/internal/modules"
)

// Key is the module's key; Bin the command it installs.
const (
	Key = "openspec"
	Bin = "openspec"
)

// Module installs the CLI.
type Module struct {
	Deps *deps.Manager
}

// New builds the module over the house's dependency manager.
func New(dm *deps.Manager) *Module { return &Module{Deps: dm} }

func (m *Module) Key() string  { return Key }
func (m *Module) Name() string { return "OpenSpec" }
func (m *Module) Description() string {
	return "Spec-driven development: before a change is built, the agent writes what and why, and someone approves it. New sites are set up for it, and each site shows its specs and changes in a Specs tab."
}
func (m *Module) Needs() []modules.Need { return nil }

// Path finds the CLI: the managed copy, else PATH.
func (m *Module) Path() (string, error) {
	if m.Deps != nil {
		p, _, err := m.Deps.Find(Bin)
		return p, err
	}
	return exec.LookPath(Bin)
}

// Verify says whether the CLI is there, and what will happen if not.
func (m *Module) Verify(ctx context.Context, cfg modules.Config) []modules.Line {
	p, err := m.Path()
	if err != nil {
		return []modules.Line{modules.Skip("openspec is not installed yet: enabling the module installs it")}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, "--version")
	if m.Deps != nil {
		cmd.Env = []string{"PATH=" + m.Deps.Path(), "OPENSPEC_TELEMETRY=0", "DO_NOT_TRACK=1"}
	}
	out, err := cmd.Output()
	if err != nil {
		return []modules.Line{modules.Fail("openspec does not run: " + err.Error())}
	}
	return []modules.Line{modules.OK("openspec " + strings.TrimSpace(string(out)) + " runs")}
}

// Install fetches the CLI (and the Node it needs) under modules/node.
func (m *Module) Install(ctx context.Context, cfg modules.Config, progress func(string)) error {
	if m.Deps == nil {
		return nil
	}
	if _, managed, err := m.Deps.Find(Bin); err == nil && managed {
		return nil
	}
	return m.Deps.Install(ctx, Bin, progress)
}

func (m *Module) Enable(ctx context.Context, cfg modules.Config) error { return nil }
func (m *Module) Disable(ctx context.Context) error                    { return nil }
func (m *Module) Routes() map[string]http.Handler                      { return nil }

// Package kimi drives Moonshot's Kimi Code CLI as a subprocess: `kimi -p
// --output-format stream-json`, which prints the conversation as OpenAI
// messages, one per line (an assistant message with its tool_calls, then
// a tool message with each result).
//
// Two things are unlike the other agents. Kimi Code reads credentials only
// from its config.toml, never from the environment unless the config names
// the variable (api_key_env), so a config is written for every turn naming
// KIMI_API_KEY, and the key itself travels in the environment, never in the
// file. And it never prints the session's id, so each conversation has a
// data directory of its own (KIMI_CODE_HOME) and a resumed turn is
// `--continue`: the most recent session of that directory is that
// conversation's.
package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// DefaultBaseURL is the Kimi platform's (an API key from
// platform.moonshot.ai). A Kimi Code membership uses
// https://api.kimi.com/coding/v1.
const DefaultBaseURL = "https://api.moonshot.ai/v1"

// Resumed is the session id a turn reports: there is no real one, and a
// non-empty value is what makes the next turn continue.
const Resumed = "continue"

// Config is the command template.
type Config struct {
	Bin     string
	APIKey  string
	BaseURL string
	Models  []string
}

// Provider implements agent.Provider.
type Provider struct{ cfg Config }

// New returns a provider with defaults filled in.
func New(cfg Config) *Provider {
	if cfg.Bin == "" {
		cfg.Bin = "kimi"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	return &Provider{cfg: cfg}
}

// Key implements agent.Provider.
func (p *Provider) Key() string { return "kimi" }

// Available implements agent.Provider.
func (p *Provider) Available() (bool, string) {
	path, err := exec.LookPath(p.cfg.Bin)
	if err != nil {
		return false, fmt.Sprintf("%s not found in PATH", p.cfg.Bin)
	}
	if p.cfg.APIKey == "" {
		return false, "no API key: add it in Modules"
	}
	return true, path
}

// Models implements agent.Provider.
func (p *Provider) Models() []string { return append([]string(nil), p.cfg.Models...) }

func (p *Provider) model(req agent.Request) string {
	if req.Model != "" {
		return req.Model
	}
	if len(p.cfg.Models) > 0 {
		return p.cfg.Models[0]
	}
	return "kimi-k3"
}

// toml quotes a string for TOML.
func toml(s string) string { return strconv.Quote(s) }

// Config is the config.toml of a turn: the provider (its key read from
// KIMI_API_KEY), the models, and the jail as permission rules, which Kimi
// Code applies in print mode as in any other.
func (p *Provider) Config(req agent.Request) string {
	var b strings.Builder
	model := p.model(req)
	fmt.Fprintf(&b, "default_model = %s\ntelemetry = false\n\n", toml(model))
	fmt.Fprintf(&b, "[providers.kimi]\ntype = \"kimi\"\nbase_url = %s\napi_key_env = \"KIMI_API_KEY\"\n\n", toml(p.cfg.BaseURL))
	models := append([]string(nil), p.cfg.Models...)
	found := false
	for _, m := range models {
		found = found || m == model
	}
	if !found {
		models = append(models, model)
	}
	for _, m := range models {
		fmt.Fprintf(&b, "[models.%s]\nprovider = \"kimi\"\nmodel = %s\nmax_context_size = 262144\n\n", toml(m), toml(m))
	}
	rule := func(decision, pattern string) {
		fmt.Fprintf(&b, "[[permission.rules]]\ndecision = %s\npattern = %s\n\n", toml(decision), toml(pattern))
	}
	for _, c := range req.Jail.Commands {
		rule("deny", "Bash("+c+"*)")
	}
	for _, d := range req.Jail.Deny {
		for _, t := range []string{"Read", "Write", "Edit", "Grep", "Glob"} {
			rule("deny", t+"("+filepath.Join(d, "**")+")")
		}
	}
	for _, d := range req.Jail.Lent {
		for _, t := range []string{"Write", "Edit"} {
			rule("deny", t+"("+filepath.Join(d, "**")+")")
		}
	}
	for _, s := range req.Jail.Secrets {
		rule("deny", "Read("+s+")")
	}
	if req.Tools != nil {
		keep := map[string]bool{}
		for _, t := range req.Tools {
			keep[t] = true
		}
		for _, t := range []string{"Read", "Write", "Edit", "Grep", "Glob", "Bash", "WebSearch", "FetchURL", "Agent", "AgentSwarm"} {
			name := t
			if t == "FetchURL" {
				name = "WebFetch"
			}
			if !keep[name] {
				rule("deny", t)
			}
		}
	}
	for _, t := range req.Disallowed {
		if t == "Task" {
			t = "Agent"
		}
		rule("deny", t)
	}
	return b.String()
}

// Args builds the command line for req.
func (p *Provider) Args(req agent.Request) []string {
	prompt := req.Prompt
	if req.System != "" {
		prompt = req.System + "\n\n---\n\n" + req.Prompt
	}
	args := []string{"-p", prompt, "--output-format", "stream-json", "-m", p.model(req)}
	if req.SessionID != "" {
		args = append(args, "--continue")
	}
	return args
}

// home is the conversation's own data directory: in the house's folder
// for the CLI (agent.Jail.ConfigHome), with what an earlier release kept
// under the site's HOME carried in once. In HOME, which a docker site's
// container writes, the config and whatever else the CLI reads from that
// folder were the site's to plant. Without that folder (a run outside a
// conversation), under HOME as before.
func home(req agent.Request) (string, error) {
	name := req.Name
	if name == "" {
		name = "default"
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, name)
	old := filepath.Join(req.Jail.Home, ".kimi-code", safe)
	dir, err := req.Jail.ConfigHome(filepath.Join("kimi", safe), old, ".")
	if err != nil || dir != "" {
		return dir, err
	}
	return old, os.MkdirAll(old, 0o700)
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("kimi: empty prompt")
	}
	if req.Jail.SiteDir == "" {
		req.Jail.SiteDir = req.Workdir
	}
	if err := req.Jail.Prepare(); err != nil {
		return nil, err
	}
	if req.Jail.Home == "" {
		req.Jail.Home = filepath.Join(os.TempDir(), "kimi-home")
	}
	dir, err := home(req)
	if err != nil {
		return nil, err
	}
	if err := fsx.WriteAtomic(filepath.Join(dir, "config.toml"), []byte(p.Config(req)), 0o600); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	env["KIMI_API_KEY"] = p.cfg.APIKey
	env["KIMI_CODE_HOME"] = dir
	return agent.StartCLIStream(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req)...)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

type parser struct {
	last   string
	sawAny bool
	failed string
}

type message struct {
	Role       string `json:"role"`
	Type       string `json:"type"`
	Message    string `json:"message"`
	Content    any    `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case []any:
		var b strings.Builder
		for _, part := range x {
			if m, ok := part.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (p *parser) Line(raw string) ([]agent.Event, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return nil, false
	}
	var m message
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil, false
	}
	if m.Type == "error" && m.Role == "" {
		p.failed = m.Message
		return []agent.Event{{Kind: agent.KindError, Text: m.Message}}, true
	}
	switch m.Role {
	case "assistant":
		p.sawAny = true
		var evs []agent.Event
		if t := strings.TrimSpace(text(m.Content)); t != "" {
			p.last = t
			evs = append(evs, agent.Event{Kind: agent.KindText, Text: t})
		}
		for _, c := range m.ToolCalls {
			var in map[string]any
			_ = json.Unmarshal([]byte(c.Function.Arguments), &in)
			if in == nil && c.Function.Arguments != "" {
				in = map[string]any{"arguments": c.Function.Arguments}
			}
			evs = append(evs, agent.Event{Kind: agent.KindTool, Tool: c.Function.Name, Input: in, ToolID: c.ID})
		}
		return evs, false
	case "tool":
		p.sawAny = true
		return []agent.Event{{Kind: agent.KindToolResult, ToolID: m.ToolCallID, Output: text(m.Content)}}, false
	}
	return nil, false
}

// Flush ends a turn that answered; one that did not (a wrong key) ends with
// nothing here, and the runner reports what the CLI said on stderr.
func (p *parser) Flush() []agent.Event {
	if !p.sawAny || p.failed != "" {
		return nil
	}
	return []agent.Event{{Kind: agent.KindCost, SessionID: Resumed}, {Kind: agent.KindResult, Text: p.last, SessionID: Resumed}}
}

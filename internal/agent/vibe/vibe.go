// Package vibe drives Mistral's Vibe CLI as a subprocess: `vibe -p
// --output streaming`, which prints the session's history entries as they
// change, one JSON object per line: messages, reasoning, effects (a tool
// call, with its state from pending to completed or failed) and notices.
// An entry can be printed several times while it is generated; it counts
// once, when it is finished.
//
// The key is MISTRAL_API_KEY, which Vibe reads from the environment. A
// level without some tools runs with only the ones it has
// (--enabled-tools, which in programmatic mode disables every other).
package vibe

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// Config is the command template.
type Config struct {
	Bin    string
	APIKey string
	Models []string
}

// Provider implements agent.Provider.
type Provider struct{ cfg Config }

// New returns a provider with defaults filled in.
func New(cfg Config) *Provider {
	if cfg.Bin == "" {
		cfg.Bin = "vibe"
	}
	return &Provider{cfg: cfg}
}

// Key implements agent.Provider.
func (p *Provider) Key() string { return "mistral" }

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

// vibeTools are Vibe's tool names for the house's.
var vibeTools = map[string][]string{
	"Read": {"read_file"}, "Grep": {"grep"}, "Glob": {"grep"}, "Write": {"write_file"}, "Edit": {"edit"}, "MultiEdit": {"edit"},
	"Bash": {"bash"}, "WebFetch": {"web_fetch"}, "WebSearch": {"web_search"}, "Task": {"task"}, "TodoWrite": {"todo"},
}

// Args builds the command line for req.
func (p *Provider) Args(req agent.Request) []string {
	prompt := req.Prompt
	if req.System != "" {
		prompt = req.System + "\n\n---\n\n" + req.Prompt
	}
	if strings.HasPrefix(prompt, "-") {
		prompt = " " + prompt
	}
	args := []string{"-p", prompt, "--output", "streaming", "--auto-approve"}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Tools != nil {
		if len(req.Tools) == 0 {
			args = append(args, "--disabled-tools", "*")
		}
		seen := map[string]bool{}
		for _, t := range req.Tools {
			for _, v := range vibeTools[t] {
				if !seen[v] {
					seen[v] = true
					args = append(args, "--enabled-tools", v)
				}
			}
		}
	}
	for _, t := range req.Disallowed {
		for _, v := range vibeTools[t] {
			args = append(args, "--disabled-tools", v)
		}
	}
	return args
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("vibe: empty prompt")
	}
	if req.Jail.SiteDir == "" {
		req.Jail.SiteDir = req.Workdir
	}
	if err := req.Jail.Prepare(); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	env["MISTRAL_API_KEY"] = p.cfg.APIKey
	return agent.StartCLIStream(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req)...)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

type parser struct {
	session string
	last    string
	done    map[string]bool
	tools   map[string]bool
	sawAny  bool
	failed  string
}

type entry struct {
	ID               string `json:"id"`
	SessionID        string `json:"sessionId"`
	Type             string `json:"type"`
	Role             string `json:"role"`
	GenerationStatus string `json:"generationStatus"`
	Content          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Title  string `json:"title"`
	Level  string `json:"level"`
	Detail struct {
		ToolName string `json:"toolName"`
		Input    any    `json:"input"`
	} `json:"detail"`
	State struct {
		Status     string `json:"status"`
		OutputText string `json:"outputText"`
		Reason     string `json:"reason"`
		Error      struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"state"`
	Message string `json:"message"`
}

func inputOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if v == nil {
		return nil
	}
	return map[string]any{"input": v}
}

func (p *parser) Line(raw string) ([]agent.Event, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return nil, false
	}
	var e entry
	if json.Unmarshal([]byte(raw), &e) != nil {
		return nil, false
	}
	if p.done == nil {
		p.done, p.tools = map[string]bool{}, map[string]bool{}
	}
	if e.SessionID != "" {
		p.session = e.SessionID
	}
	switch e.Type {
	case "message":
		if e.Role != "assistant" || e.GenerationStatus != "completed" || p.done[e.ID] {
			return nil, false
		}
		p.done[e.ID] = true
		var b strings.Builder
		for _, c := range e.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		t := strings.TrimSpace(b.String())
		if t == "" {
			return nil, false
		}
		p.sawAny = true
		p.last = t
		return []agent.Event{{Kind: agent.KindText, Text: t, SessionID: p.session}}, false
	case "effect":
		var evs []agent.Event
		name := e.Detail.ToolName
		if name == "" {
			name = e.Title
		}
		if !p.tools[e.ID] {
			p.tools[e.ID] = true
			p.sawAny = true
			evs = append(evs, agent.Event{Kind: agent.KindTool, Tool: name, Input: inputOf(e.Detail.Input), ToolID: e.ID, SessionID: p.session})
		}
		switch e.State.Status {
		case "completed", "failed", "cancelled", "skipped":
			if p.done[e.ID] {
				return evs, false
			}
			p.done[e.ID] = true
			out := e.State.OutputText
			if out == "" {
				out = e.State.Error.Message
			}
			if out == "" {
				out = e.State.Reason
			}
			evs = append(evs, agent.Event{Kind: agent.KindToolResult, ToolID: e.ID, Output: out, IsError: e.State.Status != "completed", SessionID: p.session})
		}
		return evs, false
	case "notice":
		if e.Level == "error" && e.Message != "" {
			p.failed = e.Message
		}
	}
	return nil, false
}

// Flush ends the turn: the stream has no closing entry. One that never
// answered ends with nothing here, and the runner reports what Vibe said
// on stderr (a wrong key is said there).
func (p *parser) Flush() []agent.Event {
	if p.failed != "" && !p.sawAny {
		return []agent.Event{{Kind: agent.KindError, Text: p.failed, SessionID: p.session}}
	}
	if !p.sawAny {
		return nil
	}
	return []agent.Event{{Kind: agent.KindCost, SessionID: p.session}, {Kind: agent.KindResult, Text: p.last, SessionID: p.session}}
}

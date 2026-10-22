// Package grok drives xAI's Grok Build CLI as a subprocess: `grok -p
// --output-format streaming-json`, one typed object per line (text in
// chunks, tool_call and tool_call_update, a usage line per model response,
// and end, always last, with the session and the cost).
//
// The jail goes in Grok's own permission rules (--deny), which apply in
// headless mode under --always-approve: the folders that are not the
// site's are denied to reading and writing, the jail's commands to the
// shell. A level without some tools removes them (--disallowed-tools).
package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dcimorra/middleware-public/internal/agent"
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
		cfg.Bin = "grok"
	}
	return &Provider{cfg: cfg}
}

// Key implements agent.Provider.
func (p *Provider) Key() string { return "xai" }

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

// grokTools are Grok's own tool ids for the house's (Claude Code's) names,
// the ones its documentation names.
var grokTools = map[string][]string{
	"Bash": {"run_terminal_cmd"}, "Edit": {"search_replace"}, "Write": {"search_replace"}, "MultiEdit": {"search_replace"},
	"WebSearch": {"web_search"}, "WebFetch": {"web_fetch"}, "Read": {"read_file"}, "Grep": {"grep"}, "LS": {"list_dir"},
	"Task": {"Agent"},
}

var allTools = []string{"run_terminal_cmd", "search_replace", "web_search", "web_fetch", "read_file", "grep", "list_dir", "Agent"}

// Removed are the tools a request leaves out.
func Removed(req agent.Request) []string {
	out := map[string]bool{}
	if req.Tools != nil {
		keep := map[string]bool{}
		for _, t := range req.Tools {
			for _, g := range grokTools[t] {
				keep[g] = true
			}
		}
		for _, g := range allTools {
			if !keep[g] {
				out[g] = true
			}
		}
	}
	for _, t := range req.Disallowed {
		for _, g := range grokTools[t] {
			out[g] = true
		}
	}
	var list []string
	for _, g := range allTools {
		if out[g] {
			list = append(list, g)
		}
	}
	return list
}

// Args builds the command line for req.
func (p *Provider) Args(req agent.Request) []string {
	prompt := req.Prompt
	if strings.HasPrefix(prompt, "-") {
		prompt = " " + prompt
	}
	args := []string{"-p", prompt, "--output-format", "streaming-json", "--always-approve", "--no-auto-update"}
	model := req.Model
	if model == "" && len(p.cfg.Models) > 0 {
		model = p.cfg.Models[0]
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.System != "" {
		args = append(args, "--rules", req.System)
	}
	for _, c := range req.Jail.Commands {
		args = append(args, "--deny", "Bash("+c+"*)")
	}
	for _, d := range req.Jail.Deny {
		for _, t := range []string{"Read", "Grep", "Edit", "Write"} {
			args = append(args, "--deny", t+"("+filepath.Join(d, "**")+")")
		}
	}
	for _, d := range req.Jail.Lent {
		for _, t := range []string{"Edit", "Write"} {
			args = append(args, "--deny", t+"("+filepath.Join(d, "**")+")")
		}
	}
	for _, s := range req.Jail.Secrets {
		args = append(args, "--deny", "Read("+s+")")
	}
	if r := Removed(req); len(r) > 0 {
		args = append(args, "--disallowed-tools", strings.Join(r, ","))
	}
	return args
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("grok: empty prompt")
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
	env["XAI_API_KEY"] = p.cfg.APIKey
	return agent.StartCLIStream(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req)...)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

type parser struct {
	text    strings.Builder
	last    string
	session string
	tools   map[string]bool
}

type line struct {
	Type       string          `json:"type"`
	Data       string          `json:"data"`
	Message    string          `json:"message"`
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	ToolName   string          `json:"toolName"`
	Status     string          `json:"status"`
	RawInput   map[string]any  `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
	Content    json.RawMessage `json:"content"`
	SessionID  string          `json:"sessionId"`
	StopReason string          `json:"stopReason"`
	Cost       float64         `json:"total_cost_usd"`
}

// flush turns the text gathered so far into one message: Grok sends the
// answer in chunks, and one bubble per chunk would be unreadable.
func (p *parser) flush() []agent.Event {
	t := strings.TrimSpace(p.text.String())
	p.text.Reset()
	if t == "" {
		return nil
	}
	p.last = t
	return []agent.Event{{Kind: agent.KindText, Text: t, SessionID: p.session}}
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "[]" {
		return ""
	}
	var blocks []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			if x.Text != "" {
				b.WriteString(x.Text)
			} else {
				b.WriteString(x.Content.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

func (p *parser) Line(raw string) ([]agent.Event, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return nil, false
	}
	var l line
	if json.Unmarshal([]byte(raw), &l) != nil {
		return nil, false
	}
	if p.tools == nil {
		p.tools = map[string]bool{}
	}
	if l.SessionID != "" {
		p.session = l.SessionID
	}
	switch l.Type {
	case "text":
		p.text.WriteString(l.Data)
		return nil, false
	case "tool_call":
		evs := p.flush()
		if !p.tools[l.ToolCallID] {
			p.tools[l.ToolCallID] = true
			name := l.Title
			if name == "" {
				name = l.ToolName
			}
			evs = append(evs, agent.Event{Kind: agent.KindTool, Tool: name, Input: l.RawInput, ToolID: l.ToolCallID, SessionID: p.session})
		}
		return evs, false
	case "tool_call_update":
		if l.Status != "completed" && l.Status != "failed" {
			return nil, false
		}
		out := contentText(l.Content)
		if out == "" && len(l.RawOutput) > 0 && string(l.RawOutput) != "null" {
			out = string(l.RawOutput)
		}
		return []agent.Event{{Kind: agent.KindToolResult, ToolID: l.ToolCallID, Output: out, IsError: l.Status == "failed", SessionID: p.session}}, false
	case "usage":
		return p.flush(), false
	case "error":
		evs := p.flush()
		return append(evs, agent.Event{Kind: agent.KindError, Text: l.Message, SessionID: p.session}), true
	case "end":
		evs := p.flush()
		answer := p.last
		evs = append(evs, agent.Event{Kind: agent.KindCost, Cost: l.Cost, SessionID: p.session})
		if l.StopReason == "refusal" || l.StopReason == "cancelled" {
			return append(evs, agent.Event{Kind: agent.KindError, Text: "grok: " + l.StopReason, SessionID: p.session}), true
		}
		return append(evs, agent.Event{Kind: agent.KindResult, Text: answer, Cost: l.Cost, SessionID: p.session}), true
	}
	return nil, false
}

func (p *parser) Flush() []agent.Event { return p.flush() }

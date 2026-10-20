// Package gemini drives the `gemini` CLI as a subprocess. Its output
// formats have moved between releases (plain text, a single JSON document,
// then line-delimited JSON), so the parser accepts all three from one
// stream: a line that is a JSON object is read for what it carries, a
// line that is not is text, and a document that only closes at exit is
// read when the stream ends.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/fsx"
)

// Config is the command template.
type Config struct {
	// Bin is the executable; "gemini" is looked up in PATH.
	Bin string
	// ExtraArgs are appended to every invocation.
	ExtraArgs []string
	// OutputFormat is the value of --output-format: "stream-json" when the
	// installed CLI supports it, "json" otherwise. Empty means "json",
	// the one every release since 0.1 accepts.
	OutputFormat string
	// APIKey is passed as GEMINI_API_KEY when set; empty relies on the
	// CLI's own login under the run's HOME.
	APIKey string
	// OAuthCreds is the oauth_creds.json a Google sign-in produced. The
	// CLI has no variable for it, so it is written under the run's HOME
	// (with settings.json saying to use it) before every turn.
	OAuthCreds string
	// ResumeFlag names the flag that resumes a session; empty disables
	// resuming for CLIs that have no such flag.
	ResumeFlag string
	// SandboxArgs are added when the jail denies anything: the CLI's own
	// sandbox (`--sandbox`) needs a container runtime, so it is opt-in and
	// the default confinement is the working directory plus the audit.
	SandboxArgs []string
	// Models offered to the panel; the first is the default.
	Models []string
}

// Provider implements agent.Provider.
type Provider struct {
	cfg Config
}

// New returns a provider with defaults filled in.
func New(cfg Config) *Provider {
	if cfg.Bin == "" {
		cfg.Bin = "gemini"
	}
	if cfg.OutputFormat == "" {
		cfg.OutputFormat = "json"
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"gemini-2.5-pro", "gemini-2.5-flash"}
	}
	return &Provider{cfg: cfg}
}

// Key implements agent.Provider.
func (p *Provider) Key() string { return "gemini" }

// Available implements agent.Provider.
func (p *Provider) Available() (bool, string) {
	path, err := exec.LookPath(p.cfg.Bin)
	if err != nil {
		return false, fmt.Sprintf("%s not found in PATH", p.cfg.Bin)
	}
	return true, path
}

// Models implements agent.Provider.
func (p *Provider) Models() []string { return append([]string(nil), p.cfg.Models...) }

// Args builds the command line for req.
func (p *Provider) Args(req agent.Request) []string {
	prompt := req.Prompt
	if req.System != "" {
		// The CLI has no system-prompt flag in --prompt mode, so the
		// preamble goes ahead of the prompt, set off so the model reads it
		// as context rather than as the request.
		prompt = req.System + "\n\n---\n\n" + req.Prompt
	}
	args := []string{"-p", prompt, "--output-format", p.cfg.OutputFormat, "--yolo"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SessionID != "" && p.cfg.ResumeFlag != "" {
		args = append(args, p.cfg.ResumeFlag, req.SessionID)
	}
	if len(req.Jail.Deny) > 0 {
		args = append(args, p.cfg.SandboxArgs...)
	}
	return append(args, p.cfg.ExtraArgs...)
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("gemini: empty prompt")
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
	if p.cfg.APIKey != "" {
		env["GEMINI_API_KEY"] = p.cfg.APIKey
	} else if p.cfg.OAuthCreds != "" && req.Jail.Home != "" {
		if err := writeCreds(req.Jail.Home, p.cfg.OAuthCreds); err != nil {
			return nil, err
		}
	}
	if req.Jail.Home != "" {
		if err := writeJail(req.Jail.Home, req.Jail); err != nil {
			return nil, err
		}
	}
	// Never a browser: a turn runs on a server, and a CLI that tried to
	// open one would sit waiting for it.
	env["NO_BROWSER"] = "true"
	return agent.StartCLIStream(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req)...)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

// parser is deliberately loose. Known shapes:
//
//	json:        {"response":"…","stats":{…}} as one document, possibly
//	             pretty-printed over many lines;
//	stream-json: {"type":"init","session_id":…}, {"type":"message",
//	             "role":"assistant","content":"…"}, {"type":"tool_use",
//	             "tool_name":…,"parameters":{…}}, {"type":"result",
//	             "status":"success","stats":{…}}.
//
// A pretty-printed document is accumulated until it parses; a line that
// is neither JSON nor part of a document is text.
type parser struct {
	sessionID string
	pending   strings.Builder
	sawResult bool
}

type doc struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Status     string          `json:"status"`
	Response   string          `json:"response"`
	Content    json.RawMessage `json:"content"`
	Text       string          `json:"text"`
	Error      json.RawMessage `json:"error"`
	SessionID  string          `json:"session_id"`
	ToolName   string          `json:"tool_name"`
	Name       string          `json:"name"`
	Parameters map[string]any  `json:"parameters"`
	Input      map[string]any  `json:"input"`
	Cost       float64         `json:"total_cost_usd"`
	Stats      struct {
		Cost float64 `json:"cost"`
	} `json:"stats"`
}

func (p *parser) Line(raw string) ([]agent.Event, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, false
	}
	if p.pending.Len() > 0 {
		p.pending.WriteString(raw)
		p.pending.WriteByte('\n')
		if d, ok := parseDoc(p.pending.String()); ok {
			p.pending.Reset()
			return p.events(d)
		}
		return nil, false
	}
	if strings.HasPrefix(trimmed, "{") {
		if d, ok := parseDoc(trimmed); ok {
			return p.events(d)
		}
		p.pending.WriteString(raw)
		p.pending.WriteByte('\n')
		return nil, false
	}
	return []agent.Event{{Kind: agent.KindText, Text: trimmed}}, false
}

func (p *parser) Flush() []agent.Event {
	var evs []agent.Event
	if p.pending.Len() > 0 {
		// A document that never closed is still something the operator
		// should see rather than lose.
		evs = append(evs, agent.Event{Kind: agent.KindText, Text: strings.TrimSpace(p.pending.String())})
		p.pending.Reset()
	}
	if !p.sawResult && len(evs) == 0 {
		return nil
	}
	if !p.sawResult {
		evs = append(evs, agent.Event{Kind: agent.KindResult, SessionID: p.sessionID})
	}
	return evs
}

func parseDoc(s string) (doc, bool) {
	var d doc
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return doc{}, false
	}
	return d, true
}

func (p *parser) events(d doc) ([]agent.Event, bool) {
	if d.SessionID != "" {
		p.sessionID = d.SessionID
	}
	cost := d.Cost
	if cost == 0 {
		cost = d.Stats.Cost
	}
	var evs []agent.Event
	if len(d.Error) > 0 && string(d.Error) != "null" {
		p.sawResult = true
		return append(evs, agent.Event{Kind: agent.KindError, Text: errorText(d.Error), SessionID: p.sessionID}), true
	}
	switch d.Type {
	case "tool_use", "tool_call":
		name := d.ToolName
		if name == "" {
			name = d.Name
		}
		in := d.Parameters
		if in == nil {
			in = d.Input
		}
		return append(evs, agent.Event{Kind: agent.KindTool, Tool: name, Input: in, SessionID: p.sessionID}), false
	case "result":
		p.sawResult = true
		evs = append(evs, agent.Event{Kind: agent.KindCost, Cost: cost, SessionID: p.sessionID})
		if d.Status != "" && d.Status != "success" {
			return append(evs, agent.Event{Kind: agent.KindError, Text: d.Status, Cost: cost, SessionID: p.sessionID}), true
		}
		return append(evs, agent.Event{Kind: agent.KindResult, Text: d.Response, Cost: cost, SessionID: p.sessionID}), true
	}
	if text := d.Response; text != "" {
		// The single-document json format: the whole answer, then the end.
		p.sawResult = true
		evs = append(evs, agent.Event{Kind: agent.KindText, Text: text, SessionID: p.sessionID})
		evs = append(evs, agent.Event{Kind: agent.KindCost, Cost: cost, SessionID: p.sessionID})
		return append(evs, agent.Event{Kind: agent.KindResult, Text: text, Cost: cost, SessionID: p.sessionID}), true
	}
	if d.Role == "user" {
		return nil, false
	}
	if text := contentText(d.Content, d.Text); text != "" {
		return append(evs, agent.Event{Kind: agent.KindText, Text: text, SessionID: p.sessionID}), false
	}
	return nil, false
}

func contentText(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return fallback
}

func errorText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &o) == nil && o.Message != "" {
		return o.Message
	}
	return string(raw)
}

// writeCreds puts the signed-in credentials where the CLI reads them
// under home, and makes settings.json pick that login (both spellings
// the CLI has used), touching nothing else in that file. Written only
// when different, so a turn every few seconds does not rewrite it.
func writeCreds(home, creds string) error {
	dir := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	credsPath := filepath.Join(dir, "oauth_creds.json")
	if cur, err := os.ReadFile(credsPath); err != nil || string(cur) != creds {
		if err := fsx.WriteAtomic(credsPath, []byte(creds), 0o600); err != nil {
			return err
		}
	}
	settingsPath := filepath.Join(dir, "settings.json")
	var st map[string]any
	if raw, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st == nil {
		st = map[string]any{}
	}
	sec, _ := st["security"].(map[string]any)
	if sec == nil {
		sec = map[string]any{}
	}
	auth, _ := sec["auth"].(map[string]any)
	if auth == nil {
		auth = map[string]any{}
	}
	if auth["selectedType"] == "oauth-personal" && st["selectedAuthType"] == "oauth-personal" {
		return nil
	}
	auth["selectedType"] = "oauth-personal"
	sec["auth"] = auth
	st["security"] = sec
	st["selectedAuthType"] = "oauth-personal"
	return fsx.WriteJSON(settingsPath, st, 0o600)
}

// writeJail puts the jail's commands in Gemini's settings as excluded
// shell commands (tools.exclude, matched by prefix). Gemini's file tools
// are confined to the working directory by the CLI itself; its shell is
// not, and it runs with --yolo, so without this the only wall was the
// audit after the turn. Paths in a shell command are still only caught by
// the audit: the CLI has no rule that matches a path inside a command.
func writeJail(home string, j agent.Jail) error {
	dir := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "settings.json")
	var st map[string]any
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st == nil {
		st = map[string]any{}
	}
	var exclude []any
	for _, c := range j.Commands {
		exclude = append(exclude, "run_shell_command("+c+")")
	}
	tools, _ := st["tools"].(map[string]any)
	if tools == nil {
		tools = map[string]any{}
	}
	tools["exclude"] = exclude
	st["tools"] = tools
	// The older name, read by releases before the settings were nested.
	st["excludeTools"] = exclude
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(path, data, 0o600)
}

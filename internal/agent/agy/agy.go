// Package agy drives Google's Antigravity CLI (`agy`) as a subprocess:
// `agy -p … --output-format stream-json`, which prints one JSON object per
// line ({"event":"init"}, {"event":"step_update"}, {"event":"result"}) and
// resumes a conversation with `--conversation <id>`. It replaced the
// Gemini CLI, which Google stopped serving to individual accounts.
//
// The jail is written as Antigravity's own permission rules, which it
// enforces even with --dangerously-skip-permissions (tried: a deny on a
// folder refuses read_file and write_file under it, and a deny on a
// command refuses it in run_command). A path inside a shell command is
// still only caught by the audit after the turn.
package agy

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
	// Bin is the executable; "agy" is looked up in PATH.
	Bin string
	// Token is the antigravity-oauth-token file a Google sign-in produced.
	// It is written under the run's HOME, where the CLI reads it.
	Token string
	// Models offered to the panel; the first is the default.
	Models []string
}

// Provider implements agent.Provider.
type Provider struct{ cfg Config }

// New returns a provider with defaults filled in.
func New(cfg Config) *Provider {
	if cfg.Bin == "" {
		cfg.Bin = "agy"
	}
	if len(cfg.Models) == 0 {
		cfg.Models = DefaultModels()
	}
	return &Provider{cfg: cfg}
}

// DefaultModels is what `agy models` listed on 1.4.6, the first being the
// CLI's own default.
func DefaultModels() []string {
	return []string{"gemini-3.8-flash-high", "gemini-3.8-flash-medium", "gemini-3.8-flash-low", "claude-sonnet-4-6", "claude-opus-4-6-thinking"}
}

// Key implements agent.Provider.
func (p *Provider) Key() string { return "agy" }

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
		// No system-prompt flag in print mode: the preamble goes ahead of
		// the prompt, set off so it reads as context.
		prompt = req.System + "\n\n---\n\n" + req.Prompt
	}
	if strings.HasPrefix(prompt, "-") || strings.HasPrefix(prompt, "/") {
		// A leading slash would run one of its slash commands.
		prompt = " " + prompt
	}
	args := []string{"-p", prompt, "--output-format", "stream-json", "--dangerously-skip-permissions"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SessionID != "" {
		args = append(args, "--conversation", req.SessionID)
	}
	return args
}

// Env is what every run of the CLI the house starts needs: no update of
// its own (the house keeps the binary, and one that rewrites itself
// mid-turn is a binary nobody chose), and no browser to open.
func Env() map[string]string {
	return map[string]string{"AGY_CLI_DISABLE_AUTO_UPDATE": "1", "NO_BROWSER": "true"}
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("agy: empty prompt")
	}
	if req.Jail.SiteDir == "" {
		req.Jail.SiteDir = req.Workdir
	}
	if err := req.Jail.Prepare(); err != nil {
		return nil, err
	}
	env := Env()
	for k, v := range req.Env {
		env[k] = v
	}
	if req.Jail.Home != "" {
		dir := Dir(req.Jail.Home)
		if p.cfg.Token != "" {
			if err := SeedToken(dir, p.cfg.Token); err != nil {
				return nil, err
			}
		}
		if err := writeJail(dir, req.Jail); err != nil {
			return nil, err
		}
	}
	return agent.StartCLIStream(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req)...)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

// Dir is where the CLI keeps its state under home.
func Dir(home string) string { return filepath.Join(home, ".gemini", "antigravity-cli") }

// TokenFile is the sign-in's file name in Dir.
const TokenFile = "antigravity-oauth-token"

// SeedToken writes the stored sign-in into dir unless the file there is
// already of that sign-in: the CLI refreshes the access token every hour
// and rewrites the file, and putting the stale copy back on every turn
// would make every turn start with a refresh. Same refresh token, same
// sign-in.
func SeedToken(dir, token string) error {
	path := filepath.Join(dir, TokenFile)
	if cur, err := os.ReadFile(path); err == nil && refreshOf(string(cur)) != "" && refreshOf(string(cur)) == refreshOf(token) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsx.WriteAtomic(path, []byte(token), 0o600)
}

func refreshOf(token string) string {
	var t struct {
		Token struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
	}
	_ = json.Unmarshal([]byte(token), &t)
	return t.Token.RefreshToken
}

// HasSignIn says token is a sign-in file the CLI would accept.
func HasSignIn(token string) bool { return refreshOf(token) != "" }

// writeJail writes the jail as permission rules in settings.json under
// dir, keeping whatever else the file says. Deny is closed to reading and
// writing, Lent to writing, Secrets (patterns inside the site) to reading.
func writeJail(dir string, j agent.Jail) error {
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
	deny := []any{}
	for _, d := range j.Deny {
		deny = append(deny, "read_file("+d+")", "write_file("+d+")")
	}
	for _, d := range j.Lent {
		deny = append(deny, "write_file("+d+")")
	}
	for _, s := range j.Secrets {
		if !filepath.IsAbs(s) && j.SiteDir != "" {
			s = filepath.Join(j.SiteDir, s)
		}
		deny = append(deny, "read_file("+s+")")
	}
	for _, c := range j.Commands {
		deny = append(deny, "command("+c+")")
	}
	perms, _ := st["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	perms["deny"] = deny
	st["permissions"] = perms
	return fsx.WriteJSON(path, st, 0o600)
}

// parser reads stream-json. Known shapes, from 1.1 and 1.4:
//
//	{"event":"init","conversation_id":"…","init":{"cwd":…,"tools":[…]}}
//	{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE"|"DONE"|"ERROR",
//	  "step_type":"agent_response"|"tool"|…,"text_delta":"…",
//	  "tool_name":"run_command","tool_info":{"parameters":{…},"output":"…","error":{"message":"…"}}}}
//	{"event":"result","result":{"status":"SUCCESS"|"ERROR","response":"…","error":"…"}}
//
// An answer arrives as deltas of one step; the chat takes a text event as
// a whole block, so the deltas are joined and said when the step is done.
type parser struct {
	conv    string
	text    map[int]*strings.Builder
	started map[int]bool
	sawEnd  bool
}

type stepUpdate struct {
	ConversationID string `json:"conversation_id"`
	StepIndex      int    `json:"step_index"`
	State          string `json:"state"`
	StepType       string `json:"step_type"`
	TextDelta      string `json:"text_delta"`
	ToolName       string `json:"tool_name"`
	ToolInfo       struct {
		Name       string         `json:"name"`
		Parameters map[string]any `json:"parameters"`
		Output     string         `json:"output"`
		Error      *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"tool_info"`
}

type line struct {
	Event          string          `json:"event"`
	ConversationID string          `json:"conversation_id"`
	Step           *stepUpdate     `json:"step_update"`
	Result         *result         `json:"result"`
	Error          json.RawMessage `json:"error"`
}

type result struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
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
	if p.text == nil {
		p.text, p.started = map[int]*strings.Builder{}, map[int]bool{}
	}
	if l.ConversationID != "" {
		p.conv = l.ConversationID
	}
	switch l.Event {
	case "step_update":
		if l.Step == nil {
			return nil, false
		}
		if l.Step.ConversationID != "" {
			p.conv = l.Step.ConversationID
		}
		return p.step(l.Step), false
	case "result":
		if l.Result == nil {
			return nil, false
		}
		if l.Result.ConversationID != "" {
			p.conv = l.Result.ConversationID
		}
		p.sawEnd = true
		evs := p.pending()
		if !strings.EqualFold(l.Result.Status, "SUCCESS") {
			msg := strings.TrimSpace(l.Result.Error)
			if msg == "" {
				msg = "agy: " + strings.ToLower(l.Result.Status)
			}
			return append(evs, agent.Event{Kind: agent.KindError, Text: msg, SessionID: p.conv}), true
		}
		return append(evs, agent.Event{Kind: agent.KindResult, Text: l.Result.Response, SessionID: p.conv}), true
	}
	return nil, false
}

func (p *parser) step(s *stepUpdate) []agent.Event {
	id := fmt.Sprintf("agy-%d", s.StepIndex)
	switch s.StepType {
	case "agent_response":
		if s.TextDelta != "" {
			b := p.text[s.StepIndex]
			if b == nil {
				b = &strings.Builder{}
				p.text[s.StepIndex] = b
			}
			b.WriteString(s.TextDelta)
		}
		if s.State == "DONE" || s.State == "ERROR" {
			return p.flushStep(s.StepIndex)
		}
	case "tool":
		name, in := tool(s)
		var evs []agent.Event
		if !p.started[s.StepIndex] {
			p.started[s.StepIndex] = true
			evs = append(evs, agent.Event{Kind: agent.KindTool, Tool: name, Input: in, ToolID: id, SessionID: p.conv})
		}
		switch s.State {
		case "DONE":
			evs = append(evs, agent.Event{Kind: agent.KindToolResult, ToolID: id, Output: s.ToolInfo.Output, SessionID: p.conv})
		case "ERROR":
			out := s.ToolInfo.Output
			if s.ToolInfo.Error != nil && s.ToolInfo.Error.Message != "" {
				out = s.ToolInfo.Error.Message
			}
			evs = append(evs, agent.Event{Kind: agent.KindToolResult, ToolID: id, Output: out, IsError: true, SessionID: p.conv})
		}
		return evs
	}
	return nil
}

func (p *parser) flushStep(i int) []agent.Event {
	b := p.text[i]
	delete(p.text, i)
	if b == nil {
		return nil
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return nil
	}
	return []agent.Event{{Kind: agent.KindText, Text: text, SessionID: p.conv}}
}

// pending says what was left half-said, in step order.
func (p *parser) pending() []agent.Event {
	var evs []agent.Event
	for len(p.text) > 0 {
		first := -1
		for i := range p.text {
			if first < 0 || i < first {
				first = i
			}
		}
		evs = append(evs, p.flushStep(first)...)
	}
	return evs
}

func (p *parser) Flush() []agent.Event {
	if p.sawEnd {
		return nil
	}
	evs := p.pending()
	if len(evs) == 0 && p.conv == "" {
		return nil
	}
	// A stream that stopped without its result line: the CLI died. What it
	// said is kept, and the turn ends as an error so it is not mistaken
	// for an answer.
	return append(evs, agent.Event{Kind: agent.KindError, Text: "agy ended without a result", SessionID: p.conv})
}

// tool names a step in the panel's words, so the chat summarises it the
// way it does Claude's.
func tool(s *stepUpdate) (string, map[string]any) {
	name := s.ToolName
	if name == "" {
		name = s.ToolInfo.Name
	}
	in := s.ToolInfo.Parameters
	str := func(k string) string { v, _ := in[k].(string); return v }
	switch name {
	case "run_command":
		return "Bash", map[string]any{"command": str("CommandLine")}
	case "read_file", "view_file":
		return "Read", map[string]any{"file_path": firstOf(str("Path"), str("AbsolutePath"), str("File"))}
	case "write_file", "write_to_file":
		return "Write", map[string]any{"file_path": firstOf(str("Path"), str("TargetFile"), str("File"))}
	case "replace_file_content", "multi_replace_file_content", "sed_file":
		return "Edit", map[string]any{"file_path": firstOf(str("Path"), str("TargetFile"), str("File"))}
	case "grep_search":
		return "Grep", map[string]any{"pattern": firstOf(str("Query"), str("Pattern")), "path": firstOf(str("SearchPath"), str("Path"))}
	case "find_by_name", "list_dir":
		return "Glob", map[string]any{"pattern": firstOf(str("Pattern"), str("DirectoryPath"), str("SearchDirectory"), str("Path"))}
	case "search_web":
		return "WebSearch", map[string]any{"query": firstOf(str("query"), str("Query"))}
	case "read_url_content":
		return "WebFetch", map[string]any{"url": firstOf(str("Url"), str("url"))}
	}
	return name, in
}

func firstOf(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// Package claude drives the `claude` CLI (Claude Code) as a subprocess and
// turns its stream-json output into agent events. The command line is
// data, not code: a renamed flag is a Config change, because the CLI has
// renamed flags before and a release of this binary must not be needed to
// follow it.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/fsx"
)

// Config is the command template.
type Config struct {
	// Key is the provider's name in the chat: "claude", or a vendor whose
	// models Claude Code drives through an Anthropic-compatible API
	// ("deepseek", "zai").
	Key string
	// Env is that vendor's: ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN and
	// the model names. With it, the subscription and the API key of the
	// Claude module are never passed: a turn for DeepSeek must not be able
	// to spend on Anthropic.
	Env map[string]string
	// Bin is the executable; "claude" is looked up in PATH.
	Bin string
	// ExtraArgs are appended to every invocation, after the fixed ones.
	ExtraArgs []string
	// APIKey is passed as ANTHROPIC_API_KEY when set. Empty relies on the
	// CLI's own login under the run's HOME, or on ConfigDir.
	APIKey string
	// ConfigDir, when set, is passed as CLAUDE_CONFIG_DIR so every site
	// shares one login instead of needing one per home.
	ConfigDir string
	// OAuthToken is the long-lived token `claude setup-token` gives for a
	// subscription, passed as CLAUDE_CODE_OAUTH_TOKEN. The API key wins
	// when both are set, which is the CLI's rule, not ours.
	OAuthToken string
	// Models offered to the panel; the first is the default. Aliases are
	// used by default because the CLI resolves them to the current
	// release, which a full model name would pin.
	Models []string
	// PermissionMode is the CLI's --permission-mode; acceptEdits lets the
	// agent edit inside the site without a prompt nobody is there to answer.
	PermissionMode string
}

// Provider implements agent.Provider.
type Provider struct {
	cfg Config
}

// New returns a provider with defaults filled in.
func New(cfg Config) *Provider {
	if cfg.Bin == "" {
		cfg.Bin = "claude"
	}
	if cfg.Key == "" {
		cfg.Key = "claude"
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"sonnet", "opus", "haiku"}
	}
	if cfg.PermissionMode == "" {
		// A turn has nobody to approve a prompt: under acceptEdits every
		// Bash call of a headless run is refused, so the agent could not
		// as much as run npm. Permissions are skipped and the jail's deny
		// rules (which win in every mode) are the wall, as in V1.
		cfg.PermissionMode = "bypassPermissions"
	}
	return &Provider{cfg: cfg}
}

// Key implements agent.Provider.
func (p *Provider) Key() string { return p.cfg.Key }

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

// Args builds the command line for req. It is exported so a test, or an
// operator wondering what will run, can see it without running anything.
func (p *Provider) Args(req agent.Request, settingsFile string) []string {
	args := []string{"-p", req.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", p.cfg.PermissionMode}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.System != "" {
		args = append(args, "--append-system-prompt", req.System)
	}
	if req.Jail.SiteDir != "" {
		args = append(args, "--add-dir", req.Jail.SiteDir)
	}
	for _, d := range req.Jail.Lent {
		args = append(args, "--add-dir", d)
	}
	if settingsFile != "" {
		args = append(args, "--settings", settingsFile)
	}
	if req.Tools != nil {
		args = append(args, "--tools", strings.Join(req.Tools, ","))
	}
	if len(req.Disallowed) > 0 {
		args = append(args, "--disallowed-tools", strings.Join(req.Disallowed, ","))
	}
	return append(args, p.cfg.ExtraArgs...)
}

// Settings is the settings file the jail becomes: a deny rule per tool per
// denied path, and one per denied command. Deny wins over allow and over
// the permission mode in Claude Code, which is why the jail never lists the
// root itself (see agent.NewJail).
//
// An absolute path is written with two slashes: in Claude Code's rules
// "/x" is relative to the settings file and only "//x" is the root's /x.
// V2's first draft wrote one slash, so every path rule matched nothing and
// only the audit after the turn stood between a site and its neighbours.
// Bash is matched on the command text, so a path spelled differently
// slips through here and is caught by the audit instead.
func Settings(j agent.Jail) map[string]any {
	var deny []string
	for _, d := range j.Deny {
		for _, tool := range []string{"Read", "Edit", "Write", "MultiEdit", "Glob", "Grep", "NotebookEdit"} {
			deny = append(deny, fmt.Sprintf("%s(/%s/**)", tool, d))
			deny = append(deny, fmt.Sprintf("%s(/%s)", tool, d))
		}
		deny = append(deny, fmt.Sprintf("Bash(*%s*)", d))
	}
	for _, c := range j.Commands {
		deny = append(deny, fmt.Sprintf("Bash(%s:*)", c))
	}
	for _, d := range j.Lent {
		for _, tool := range []string{"Edit", "Write", "MultiEdit", "NotebookEdit"} {
			deny = append(deny, fmt.Sprintf("%s(/%s/**)", tool, d))
		}
		// Bash is matched on its text and could write as easily as read;
		// the Read tool is how a lent folder is read.
		deny = append(deny, fmt.Sprintf("Bash(*%s*)", d))
	}
	for _, s := range j.Secrets {
		for _, tool := range []string{"Read", "Grep", "Glob"} {
			deny = append(deny, fmt.Sprintf("%s(/%s)", tool, s))
		}
	}
	return map[string]any{"permissions": map[string]any{"deny": deny}}
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("claude: empty prompt")
	}
	if req.Jail.SiteDir == "" {
		req.Jail.SiteDir = req.Workdir
	}
	if err := req.Jail.Prepare(); err != nil {
		return nil, err
	}
	settingsFile := ""
	if len(req.Jail.Deny) > 0 || len(req.Jail.Secrets) > 0 || len(req.Jail.Commands) > 0 || len(req.Jail.Lent) > 0 {
		dir := req.Jail.Home
		if dir == "" {
			dir = req.Workdir
		}
		// One per conversation: two conversations of one site with different
		// tool levels used to rewrite each other's file without a single
		// error anywhere (V1). A '#' is fine in a file name.
		file := "claude-settings.json"
		if req.Name != "" && strings.Contains(req.Name, "#") {
			file = "claude-settings-" + strings.ReplaceAll(req.Name, "/", "_") + ".json"
		}
		settingsFile = filepath.Join(dir, ".middleware", file)
		if err := fsx.WriteJSON(settingsFile, Settings(req.Jail), 0o600); err != nil {
			return nil, err
		}
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	if len(p.cfg.Env) > 0 {
		for k, v := range p.cfg.Env {
			env[k] = v
		}
	} else if p.cfg.APIKey != "" {
		env["ANTHROPIC_API_KEY"] = p.cfg.APIKey
	}
	// In print mode the CLI ends a background subagent after ten minutes
	// unless told not to wait with a ceiling; V1 lost long agents that way.
	env["CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS"] = "0"
	if p.cfg.ConfigDir != "" && len(p.cfg.Env) == 0 {
		env["CLAUDE_CONFIG_DIR"] = p.cfg.ConfigDir
	}
	if p.cfg.OAuthToken != "" && p.cfg.APIKey == "" && len(p.cfg.Env) == 0 {
		env["CLAUDE_CODE_OAUTH_TOKEN"] = p.cfg.OAuthToken
	}
	return agent.StartCLIStream(ctx, &parser{}, req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req, settingsFile)...)
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, &parser{}, pid, stream)
}

// parser reads stream-json: one JSON object per line with a "type" of
// system (init, carries session_id; task_notification, a subagent's
// state), assistant (a message whose content blocks are text or
// tool_use), user (tool results, matched to their call by id) and result
// (the end, with total_cost_usd). Anything that is not JSON is passed on
// as text so a CLI that prints a warning before the stream is still read.
type parser struct {
	sessionID string
}

type line struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	IsError   bool    `json:"is_error"`
	Result    string  `json:"result"`
	Cost      float64 `json:"total_cost_usd"`
	// Error is where Qwen Code, which prints this same format, puts a
	// failed turn's reason; Claude Code puts it in Result.
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	// ParentToolUseID marks a subagent's messages: the Task call that
	// started it.
	ParentToolUseID string `json:"parent_tool_use_id"`
	// A task_notification's call and state.
	ToolUseID string `json:"tool_use_id"`
	Status    string `json:"status"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
	// A tool_result, in a "user" line.
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// textOf reads a tool result's content: a string, or text blocks.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func (p *parser) Line(raw string) ([]agent.Event, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	var l line
	if !strings.HasPrefix(raw, "{") || json.Unmarshal([]byte(raw), &l) != nil {
		return []agent.Event{{Kind: agent.KindText, Text: raw}}, false
	}
	if l.SessionID != "" {
		p.sessionID = l.SessionID
	}
	switch l.Type {
	case "assistant":
		var blocks []block
		if err := json.Unmarshal(l.Message.Content, &blocks); err != nil {
			var s string
			if json.Unmarshal(l.Message.Content, &s) == nil && s != "" {
				return []agent.Event{{Kind: agent.KindText, Text: s, SessionID: p.sessionID}}, false
			}
			return nil, false
		}
		var evs []agent.Event
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					evs = append(evs, agent.Event{Kind: agent.KindText, Text: b.Text, SessionID: p.sessionID, Thread: l.ParentToolUseID})
				}
			case "tool_use":
				evs = append(evs, agent.Event{Kind: agent.KindTool, Tool: b.Name, ToolID: b.ID, Input: b.Input, SessionID: p.sessionID, Thread: l.ParentToolUseID})
			}
		}
		return evs, false
	case "user":
		// What comes back to the agent: tool results, matched by id.
		var blocks []block
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return nil, false
		}
		var evs []agent.Event
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				evs = append(evs, agent.Event{Kind: agent.KindToolResult, ToolID: b.ToolUseID, Output: textOf(b.Content), IsError: b.IsError, SessionID: p.sessionID, Thread: l.ParentToolUseID})
			}
		}
		return evs, false
	case "system":
		// A subagent that ends (or fails, or is stopped) says so here,
		// with the id of the call that launched it. Background shells send
		// it too; the chat keeps only the ones that name a subagent.
		if l.Subtype == "task_notification" && l.ToolUseID != "" && l.Status != "" {
			return []agent.Event{{Kind: agent.KindAgentStatus, ToolID: l.ToolUseID, Status: l.Status, SessionID: p.sessionID}}, false
		}
		return nil, false
	case "rate_limit_event":
		if w, ok := limitOf(raw); ok {
			return []agent.Event{{Kind: agent.KindLimits, Limits: []agent.Window{w}, SessionID: p.sessionID}}, false
		}
		return nil, false
	case "result":
		evs := []agent.Event{{Kind: agent.KindCost, Cost: l.Cost, SessionID: p.sessionID}}
		if l.IsError || (l.Subtype != "" && l.Subtype != "success") {
			text := l.Result
			if text == "" {
				text = l.Error.Message
			}
			if text == "" {
				text = l.Subtype
			}
			evs = append(evs, agent.Event{Kind: agent.KindError, Text: text, Cost: l.Cost, SessionID: p.sessionID})
		} else {
			evs = append(evs, agent.Event{Kind: agent.KindResult, Text: l.Result, Cost: l.Cost, SessionID: p.sessionID})
		}
		return evs, true
	}
	return nil, false
}

func (p *parser) Flush() []agent.Event { return nil }

// claudeWindows are Claude Code's rateLimitType names for the house's.
var claudeWindows = map[string]string{"five_hour": "5h", "seven_day": "7d", "seven_day_opus": "7d_opus", "seven_day_sonnet": "7d_sonnet"}

// limitOf reads a rate_limit_event: a subscription window's state, which
// Claude Code passes on from the API's anthropic-ratelimit-unified-*
// headers. utilization is the header's, a fraction of 1, and comes only
// when the API sent it (past a warning threshold, mostly); without it the
// window's state and reset are still worth keeping.
func limitOf(raw string) (agent.Window, bool) {
	var e struct {
		Info struct {
			Status        string   `json:"status"`
			ResetsAt      float64  `json:"resetsAt"`
			RateLimitType string   `json:"rateLimitType"`
			Utilization   *float64 `json:"utilization"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal([]byte(raw), &e) != nil {
		return agent.Window{}, false
	}
	name, ok := claudeWindows[e.Info.RateLimitType]
	if !ok {
		return agent.Window{}, false
	}
	w := agent.Window{Name: name, Used: -1, Status: e.Info.Status}
	if e.Info.Utilization != nil {
		w.Used = *e.Info.Utilization * 100
	}
	if e.Info.ResetsAt > 0 {
		w.ResetsAt = time.Unix(int64(e.Info.ResetsAt), 0).UTC()
	}
	return w, true
}

// NewParser is the stream-json reader, for CLIs that print Claude Code's
// format (Qwen Code does).
func NewParser() agent.Parser { return &parser{} }

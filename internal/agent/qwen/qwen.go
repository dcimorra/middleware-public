// Package qwen drives Qwen Code, Alibaba's agent CLI, as a subprocess.
// It forked Gemini CLI and prints Claude Code's stream-json, so this is
// the shortest adapter: Gemini CLI's way of writing the jail into settings,
// Claude's reader of the stream. The key and the endpoint go in the
// environment (OpenAI-compatible), as Qwen Code documents for scripts.
package qwen

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/agent/claude"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// DefaultBaseURL is QwenCloud's international pay-as-you-go endpoint.
const DefaultBaseURL = "https://maas.qwencloudapi.com/compatible-mode/v1"

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
		cfg.Bin = "qwen"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	return &Provider{cfg: cfg}
}

// Key implements agent.Provider.
func (p *Provider) Key() string { return "qwen" }

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

// claudeTools maps the house's tool names (Claude Code's) to Qwen Code's.
var claudeTools = map[string][]string{
	"Read": {"read_file", "read_many_files"}, "Glob": {"glob"}, "Grep": {"grep_search"}, "LS": {"list_directory"},
	"Write": {"write_file"}, "Edit": {"edit"}, "MultiEdit": {"edit"}, "Bash": {"run_shell_command"},
	"WebFetch": {"web_fetch"}, "WebSearch": {"web_search"}, "Task": {"agent"}, "TodoWrite": {"todo_write"},
}

var allTools = []string{"read_file", "read_many_files", "glob", "grep_search", "list_directory", "write_file", "edit", "run_shell_command", "web_fetch", "web_search", "agent", "todo_write", "save_memory", "manage_memory"}

// Excluded are Qwen Code's tools a request leaves out.
func Excluded(req agent.Request) []string {
	out := map[string]bool{}
	if req.Tools != nil {
		keep := map[string]bool{}
		for _, t := range req.Tools {
			for _, q := range claudeTools[t] {
				keep[q] = true
			}
		}
		for _, q := range allTools {
			if !keep[q] {
				out[q] = true
			}
		}
	}
	for _, t := range req.Disallowed {
		for _, q := range claudeTools[t] {
			out[q] = true
		}
	}
	var list []string
	for _, q := range allTools {
		if out[q] {
			list = append(list, q)
		}
	}
	return list
}

// Args builds the command line for req.
func (p *Provider) Args(req agent.Request) []string {
	args := []string{"--output-format", "stream-json", "--approval-mode", "yolo"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	} else if len(p.cfg.Models) > 0 {
		args = append(args, "--model", p.cfg.Models[0])
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.System != "" {
		args = append(args, "--append-system-prompt", req.System)
	}
	if ex := Excluded(req); len(ex) > 0 {
		args = append(args, "--exclude-tools", strings.Join(ex, ","))
	}
	prompt := req.Prompt
	if strings.HasPrefix(prompt, "-") {
		prompt = " " + prompt
	}
	return append(args, prompt)
}

// Start implements agent.Provider.
func (p *Provider) Start(ctx context.Context, req agent.Request) (agent.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("qwen: empty prompt")
	}
	if req.Jail.SiteDir == "" {
		req.Jail.SiteDir = req.Workdir
	}
	if err := req.Jail.Prepare(); err != nil {
		return nil, err
	}
	if req.Jail.Home != "" {
		if err := WriteExclusions(filepath.Join(req.Jail.Home, ".qwen"), req.Jail); err != nil {
			return nil, err
		}
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	env["OPENAI_API_KEY"] = p.cfg.APIKey
	env["OPENAI_BASE_URL"] = p.cfg.BaseURL
	if m := firstModel(req, p.cfg.Models); m != "" {
		env["OPENAI_MODEL"] = m
	}
	// The house's jail and audit are the walls; the CLI's warning about
	// running without its own sandbox would only be noise in the stream.
	env["QWEN_CODE_SUPPRESS_YOLO_WARNING"] = "1"
	env["NO_BROWSER"] = "true"
	return agent.StartCLIStream(ctx, claude.NewParser(), req.Stream, req.Workdir, req.Jail.Env(env), p.cfg.Bin, p.Args(req)...)
}

func firstModel(req agent.Request, models []string) string {
	if req.Model != "" {
		return req.Model
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

// Adopt implements agent.Adopter.
func (p *Provider) Adopt(ctx context.Context, pid int, stream string) agent.Run {
	return agent.AdoptCLI(ctx, claude.NewParser(), pid, stream)
}

// WriteExclusions writes the jail's commands as excluded shell commands in
// settings.json under dir: Qwen Code's, which forked Gemini CLI and reads
// its keys from ~/.qwen.
func WriteExclusions(dir string, j agent.Jail) error {
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

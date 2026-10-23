package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/agent/agy"
	"github.com/dcimorra/middleware-public/internal/modules"
)

// Subscription usage: how much of each window is spent and when it starts
// again. Two sources, and neither is enough alone:
//
//   - The vendor, asked: Anthropic's /api/oauth/usage (what Claude Code's
//     /usage reads), ChatGPT's /backend-api/wham/usage (what Codex's
//     /status reads), and Antigravity's own `/usage`, run through the CLI. Neither spends anything, but a `claude setup-token`
//     session may lack the scope the first one asks for.
//   - The turn, listened to: Claude Code prints a rate_limit_event on its
//     stream, and the chat keeps it in the usage book. Free, but only as
//     fresh as the last turn, and the percentage comes only past a warning.
//
// The panel shows whichever is newer, with its time and its source.

// usageTTL is how long an answer from the vendor is good for: the panel
// asks every time the screen opens, and the vendor need not hear it.
const usageTTL = time.Minute

// UsageClient is replaced by tests.
var UsageClient = &http.Client{Timeout: 15 * time.Second}

// Usage endpoints, replaced by tests.
var (
	ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	CodexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
)

func getJSON(ctx context.Context, url string, header map[string]string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, val := range header {
		req.Header.Set(k, val)
	}
	resp, err := UsageClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = e.Detail
		}
		if msg == "" {
			msg = strings.TrimSpace(string(body))
			if len(msg) > 200 {
				msg = msg[:200]
			}
		}
		return fmt.Errorf("%s: %s", resp.Status, msg)
	}
	return json.Unmarshal(body, v)
}

// claudeUsage asks Anthropic with the subscription's OAuth token.
func claudeUsage(ctx context.Context, token string) (string, []agent.Window, error) {
	var out map[string]json.RawMessage
	err := getJSON(ctx, ClaudeUsageURL, map[string]string{
		"Authorization":  "Bearer " + token,
		"anthropic-beta": "oauth-2025-04-20",
		"Accept":         "application/json",
	}, &out)
	if err != nil {
		return "", nil, err
	}
	var ws []agent.Window
	for key, name := range map[string]string{"five_hour": "5h", "seven_day": "7d", "seven_day_opus": "7d_opus", "seven_day_sonnet": "7d_sonnet"} {
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if raw, ok := out[key]; !ok || json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			continue
		}
		win := agent.Window{Name: name, Used: *w.Utilization}
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			win.ResetsAt = t.UTC()
		}
		ws = append(ws, win)
	}
	if len(ws) == 0 {
		return "", nil, errors.New("the answer names no usage window")
	}
	return "", ws, nil
}

// codexUsage asks ChatGPT with the access token of Codex's auth.json.
func codexUsage(ctx context.Context, authJSON string) (string, []agent.Window, error) {
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal([]byte(authJSON), &auth) != nil || auth.Tokens.AccessToken == "" {
		return "", nil, errors.New("the sign-in holds no access token")
	}
	header := map[string]string{"Authorization": "Bearer " + auth.Tokens.AccessToken, "Accept": "application/json", "User-Agent": "codex_cli_rs"}
	if auth.Tokens.AccountID != "" {
		header["ChatGPT-Account-Id"] = auth.Tokens.AccountID
	}
	type window struct {
		UsedPercent        float64 `json:"used_percent"`
		LimitWindowSeconds int64   `json:"limit_window_seconds"`
		ResetAfterSeconds  int64   `json:"reset_after_seconds"`
		ResetAt            int64   `json:"reset_at"`
	}
	var out struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			LimitReached bool    `json:"limit_reached"`
			Primary      *window `json:"primary_window"`
			Secondary    *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := getJSON(ctx, CodexUsageURL, header, &out); err != nil {
		return "", nil, err
	}
	if out.RateLimit == nil {
		return out.PlanType, nil, errors.New("the answer names no usage window")
	}
	var ws []agent.Window
	for _, w := range []*window{out.RateLimit.Primary, out.RateLimit.Secondary} {
		if w == nil {
			continue
		}
		win := agent.Window{Name: windowName(w.LimitWindowSeconds), Used: w.UsedPercent}
		switch {
		case w.ResetAt > 0:
			win.ResetsAt = time.Unix(w.ResetAt, 0).UTC()
		case w.ResetAfterSeconds > 0:
			win.ResetsAt = time.Now().Add(time.Duration(w.ResetAfterSeconds) * time.Second).UTC()
		}
		if out.RateLimit.LimitReached && w.UsedPercent >= 100 {
			win.Status = "rejected"
		}
		ws = append(ws, win)
	}
	if len(ws) == 0 {
		return out.PlanType, nil, errors.New("the answer names no usage window")
	}
	return out.PlanType, ws, nil
}

// windowName names a window by its length: 5h, 7d, or whatever it is.
func windowName(seconds int64) string {
	switch {
	case seconds <= 0:
		return "window"
	case seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	}
	return fmt.Sprintf("%dm", seconds/60)
}

// usageFetch is how a module asks its vendor, or nil when it cannot.
func (m *Module) usageFetch(cfg modules.Config) func(context.Context) (string, []agent.Window, error) {
	login := strings.TrimSpace(cfg[m.secretKey])
	if login == "" || strings.TrimSpace(cfg["api_key"]) != "" {
		return nil
	}
	switch m.provider() {
	case "claude":
		return func(ctx context.Context) (string, []agent.Window, error) { return claudeUsage(ctx, login) }
	case "openai":
		// Codex refreshes the token in its own home, and that copy is
		// newer than the one the module stored at sign-in.
		if raw, err := os.ReadFile(filepath.Join(m.home, "codex-home", "auth.json")); err == nil && len(raw) > 0 {
			login = string(raw)
		}
		return func(ctx context.Context) (string, []agent.Window, error) { return codexUsage(ctx, login) }
	case "agy":
		if m.home == "" {
			return nil
		}
		return func(ctx context.Context) (string, []agent.Window, error) {
			bin, err := m.binary(cfg)
			if err != nil {
				return "", nil, err
			}
			home := filepath.Join(m.home, "usage-home")
			if err := agy.SeedToken(agy.Dir(home), login); err != nil {
				return "", nil, err
			}
			return agy.Usage(ctx, bin, m.extraPath(), home)
		}
	}
	return nil
}

// usageState is what GET /usage answers: the book's entry, and why the
// vendor could not be asked, when it could not.
func (m *Module) usageState(ctx context.Context, cfg modules.Config, refresh bool) map[string]any {
	out := map[string]any{"provider": m.provider()}
	fetch := m.usageFetch(cfg)
	if fetch == nil {
		out["supported"] = false
		if u, ok := m.Usage.Get(m.provider()); ok {
			out["usage"] = u
		}
		return out
	}
	out["supported"] = true
	u, ok := m.Usage.Get(m.provider())
	m.mu.Lock()
	stale := refresh || time.Since(m.usageAt) > usageTTL
	if stale {
		m.usageAt = time.Now()
	}
	m.mu.Unlock()
	if stale {
		plan, ws, err := fetch(ctx)
		m.mu.Lock()
		if err != nil {
			m.usageErr = err.Error()
		} else {
			m.usageErr = ""
		}
		m.mu.Unlock()
		if err == nil {
			m.Usage.Record(m.provider(), plan, "vendor", ws)
			u, ok = m.Usage.Get(m.provider())
		}
	}
	m.mu.Lock()
	if m.usageErr != "" {
		out["error"] = m.usageErr
	}
	m.mu.Unlock()
	if ok {
		out["usage"] = u
	}
	return out
}

func (m *Module) handleUsage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, m.usageState(ctx, m.config(), r.URL.Query().Get("refresh") == "1"))
}

// UsageOf is every subscription's usage, for /agents/usage: the modules
// that can ask their vendor, and the ones a turn told something about.
// With provider, only that agent's: what a chat asks while its turn runs,
// so the other vendors are not asked every minute for nothing.
func UsageOf(ctx context.Context, refresh bool, provider string, mods ...*Module) []map[string]any {
	out := []map[string]any{}
	for _, m := range mods {
		if m == nil || m.Store == nil || !m.Store.Enabled(m.key) {
			continue
		}
		if provider != "" && m.provider() != provider {
			continue
		}
		st := m.usageState(ctx, m.config(), refresh)
		if st["supported"] == false && st["usage"] == nil {
			continue
		}
		st["name"], st["module"] = m.name, m.key
		out = append(out, st)
	}
	return out
}

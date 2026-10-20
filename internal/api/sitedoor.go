package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/auth"
	"github.com/dcimorra/middleware-public/internal/chat"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/sitetoken"
)

// The site door is the third door of the house: the panel is for whoever
// runs the machine, the API tokens for scripts, and this one for the code
// of a published site. It exists because a site that wanted Claude used
// to write its own engine: find the binary, build the command line, write
// a jail if it remembered, read the JSON at the end, record the cost if it
// felt like it. V1's comparaloo had exactly that file, a second engine of
// conversations on the machine with no resume, no bill and a credential
// that opened everything. What the house already had was one call away.
//
// A site's conversation is a normal conversation: kept, costed, readable
// and stoppable from the panel. It does not notify anyone: a website at
// work is not "your turn". And it cannot reach the panel's conversation of
// the same site, where the machine is discussed with every tool on.

// SiteDoorOptions wire the site door.
type SiteDoorOptions struct {
	Chat   *chat.Manager
	Tokens *sitetoken.Keys
	// Lookup returns a site's manifest as the engine has it now.
	Lookup func(domain string) (*manifest.Manifest, bool)
	// HouseDaily is the daily cap of a site whose manifest names none.
	HouseDaily func() float64
	// Ledger reads what a book spent on a day.
	Spent func(book string, at time.Time) float64
}

type doorCaller struct {
	Domain string
	Rules  manifest.ClaudeAPI
	Daily  float64
}

// RegisterSiteDoor adds /site and /site/claude/…. The routes are public to
// the API's own authentication and check the site token themselves.
func (s *Server) RegisterSiteDoor(o SiteDoorOptions) {
	p := Prefix + "/site"
	route := func(method, path, summary string, h func(http.ResponseWriter, *http.Request, doorCaller) (any, error)) {
		s.Handle(method, path, summary, func(w http.ResponseWriter, r *http.Request) (any, error) {
			c, err := o.door(r)
			if err != nil {
				return nil, err
			}
			return h(w, r, c)
		})
		s.Public(method, path)
	}
	route("GET", p, "Who this site is and what it may ask: tools, model, spent and left today", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		spent := o.Spent(chat.SiteDoorBook(c.Domain), time.Now())
		return map[string]any{"domain": c.Domain, "claude": map[string]any{
			"tools": chat.Level(c.Rules.Tools).Name, "model": c.Rules.Model,
			"daily_usd": c.Daily, "spent_usd": round4(spent), "left_usd": round4(max(0, c.Daily-spent)),
			"chats_open": len(o.Chat.List(c.Domain)),
		}}, nil
	})
	route("POST", p+"/claude/ask", "One question and its answer in one call (body: prompt, system?, model?, format?: json, schema?, wait?)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		var b askBody
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		name, err := o.open(c, b)
		if err != nil {
			return nil, err
		}
		return o.sendAndAnswer(r.Context(), c, name, b)
	})
	route("GET", p+"/claude/chats", "This site's conversations", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		var out []chat.Summary
		for _, sm := range o.Chat.List(c.Domain) {
			if sm.Origin == chat.OriginSite {
				out = append(out, sm)
			}
		}
		if out == nil {
			out = []chat.Summary{}
		}
		return out, nil
	})
	route("POST", p+"/claude/chats", "Open a conversation (body: title?, system?, model?, prompt? to send a first message)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		var b askBody
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		name, err := o.open(c, b)
		if err != nil {
			return nil, err
		}
		_, id, _ := strings.Cut(name, "#")
		if b.said() == "" {
			WriteJSON(w, http.StatusCreated, map[string]any{"id": id})
			return Handled, nil
		}
		return o.sendAndAnswer(r.Context(), c, name, b)
	})
	route("GET", p+"/claude/chats/{id}", "One conversation, every turn", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		return o.Chat.Conversation(name)
	})
	route("POST", p+"/claude/chats/{id}/messages", "Send the next message (body: prompt, model?, format?, schema?, wait?)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		var b askBody
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		if b.said() == "" {
			return nil, Errorf(http.StatusBadRequest, "prompt: empty")
		}
		if err := o.allowance(c); err != nil {
			return nil, err
		}
		return o.sendAndAnswer(r.Context(), c, name, b)
	})
	route("POST", p+"/claude/chats/{id}/stop", "Stop the running turn", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		_ = o.Chat.Stop(name)
		return OK, nil
	})
	route("POST", p+"/claude/chats/{id}/reap", "Stop the turn and kill what the conversation left running", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		return map[string]int{"killed": o.Chat.Reap(name)}, nil
	})
	route("DELETE", p+"/claude/chats/{id}", "Delete a conversation", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		if err := o.Chat.Delete(name); err != nil {
			return nil, chatError(err)
		}
		return OK, nil
	})
}

// door resolves the caller: a site token, or the operator (session or API
// token) naming the site in X-Middleware-Site. What decides is the manifest
// as it is now: a site whose door was closed still carries its token in
// its environment until it restarts, and must not get in with it.
func (o SiteDoorOptions) door(r *http.Request) (doorCaller, error) {
	domain := ""
	if tok, ok := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "); ok && o.Tokens != nil {
		domain = o.Tokens.Domain(strings.TrimSpace(tok))
	}
	if domain == "" {
		if c := auth.FromContext(r.Context()); c.Authenticated() {
			domain = strings.ToLower(strings.TrimSpace(r.Header.Get("X-Middleware-Site")))
			if domain == "" {
				return doorCaller{}, Errorf(http.StatusBadRequest, "as the operator, name the site in X-Middleware-Site")
			}
		}
	}
	if domain == "" {
		// The same pause as the other doors: a token that can be tried a
		// thousand times a second is shorter than it looks.
		time.Sleep(400 * time.Millisecond)
		return doorCaller{}, Errorf(http.StatusUnauthorized, "this site's token is required (MIDDLEWARE_TOKEN)")
	}
	m, ok := o.Lookup(domain)
	if !ok || m == nil {
		return doorCaller{}, Errorf(http.StatusNotFound, "%s: no such site", domain)
	}
	if m.ClaudeAPI == nil {
		return doorCaller{}, Errorf(http.StatusForbidden, "%s has no site door open: add claude_api to its manifest", domain)
	}
	daily := m.ClaudeAPI.DailyUSD
	if daily <= 0 && o.HouseDaily != nil {
		daily = o.HouseDaily()
	}
	return doorCaller{Domain: domain, Rules: *m.ClaudeAPI, Daily: daily}, nil
}

// allowance checks the money before a turn starts, not on the invoice.
func (o SiteDoorOptions) allowance(c doorCaller) error {
	if spent := o.Spent(chat.SiteDoorBook(c.Domain), time.Now()); spent >= c.Daily {
		return Errorf(http.StatusTooManyRequests, "%s spent $%.2f through the site door today and its cap is $%.2f; raise claude_api.daily_usd or wait until tomorrow (UTC)", c.Domain, spent, c.Daily)
	}
	return nil
}

func (o SiteDoorOptions) open(c doorCaller, b askBody) (string, error) {
	if err := o.allowance(c); err != nil {
		return "", err
	}
	model := strings.TrimSpace(b.Model)
	if model == "" {
		model = c.Rules.Model
	}
	max := c.Rules.MaxChats
	if max == 0 {
		max = 20
	}
	name, err := o.Chat.Create(c.Domain, chat.CreateOptions{
		Origin: chat.OriginSite, Title: b.Title, System: b.System, Tools: c.Rules.Tools,
		Model: model, Hidden: c.Rules.HideChats, Max: max,
	})
	if errors.Is(err, chat.ErrTooMany) {
		return "", Errorf(http.StatusTooManyRequests, "%v (claude_api.max_chats)", err)
	}
	return name, err
}

// siteChat resolves {id} to a conversation the site's code opened, and
// refuses any other: the panel's own is never reachable from here.
func (o SiteDoorOptions) siteChat(r *http.Request, c doorCaller) (string, error) {
	name := chat.Name(c.Domain, r.PathValue("id"))
	conv, err := o.Chat.Conversation(name)
	if err != nil || conv.Origin != chat.OriginSite || r.PathValue("id") == "" {
		return "", Errorf(http.StatusNotFound, "no such conversation")
	}
	return name, nil
}

// askBody is what may be asked; only the text is required.
type askBody struct {
	// Prompt and Text are the same: who comes from Anthropic's API writes
	// one, who comes from the panel the other, and refusing the wrong one
	// teaches nobody anything.
	Prompt string `json:"prompt"`
	Text   string `json:"text"`
	System string `json:"system"`
	Model  string `json:"model"`
	Title  string `json:"title"`
	// Format "json" asks for an answer that is only JSON; with Schema,
	// one that fits it.
	Format string          `json:"format"`
	Schema json.RawMessage `json:"schema"`
	// Wait is how long this request waits for the answer: "2m" by
	// default, at most 15m; "0" or "no" returns once the turn starts and
	// leaves polling to the caller.
	Wait string `json:"wait"`
}

func (b askBody) said() string {
	if s := strings.TrimSpace(b.Prompt); s != "" {
		return s
	}
	return strings.TrimSpace(b.Text)
}

func waitFor(s string) time.Duration {
	d := 2 * time.Minute
	switch strings.TrimSpace(s) {
	case "":
	case "0", "no":
		return 0
	default:
		if v, err := time.ParseDuration(s); err == nil {
			d = v
		}
	}
	return min(max(d, 0), 15*time.Minute)
}

func (o SiteDoorOptions) sendAndAnswer(ctx context.Context, c doorCaller, name string, b askBody) (any, error) {
	text := b.said()
	if text == "" {
		return nil, Errorf(http.StatusBadRequest, "prompt: empty")
	}
	if strings.EqualFold(b.Format, "json") {
		text += "\n\nAnswer with JSON only: no prose, no code fence."
		if len(b.Schema) > 0 {
			text += " It must validate against this JSON Schema:\n" + string(b.Schema)
		}
	}
	turn, err := o.Chat.SendWith(name, text, chat.SendOptions{Model: b.Model})
	if err != nil {
		return nil, chatError(err)
	}
	_, id, _ := strings.Cut(name, "#")
	wait := waitFor(b.Wait)
	if wait == 0 {
		return map[string]any{"id": id, "turn_id": turn, "running": true}, nil
	}
	last, done := o.Chat.Wait(ctx, name, wait)
	if !done {
		return map[string]any{"id": id, "turn_id": turn, "running": true, "note": "still running: GET the conversation to read the answer"}, nil
	}
	out := map[string]any{"id": id, "turn_id": turn, "text": last.Text, "cost_usd": round4(last.Cost)}
	if last.Role == "error" {
		out["error"] = last.Text
	}
	if strings.EqualFold(b.Format, "json") && last.Role != "error" {
		v, err := looseJSON(last.Text)
		if err != nil {
			out["json_error"] = err.Error()
		} else {
			out["json"] = v
		}
	}
	return out, nil
}

// looseJSON reads the JSON in an answer that was asked to be only JSON
// and sometimes still comes in a fence or after a line of prose.
func looseJSON(said string) (any, error) {
	s := strings.TrimSpace(said)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		if j := strings.Index(rest, "```"); j >= 0 {
			s = strings.TrimSpace(rest[:j])
		}
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v, nil
	}
	for _, open := range []string{"{", "["} {
		closeCh := map[string]string{"{": "}", "[": "]"}[open]
		if i, j := strings.Index(s, open), strings.LastIndex(s, closeCh); i >= 0 && j > i {
			if json.Unmarshal([]byte(s[i:j+1]), &v) == nil {
				return v, nil
			}
		}
	}
	return nil, fmt.Errorf("the answer is not JSON")
}

func round4(v float64) float64 { return float64(int64(v*10000+0.5)) / 10000 }

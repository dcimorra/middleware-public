package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/auth"
	"github.com/dcimorra/middleware-public/internal/chat"
	"github.com/dcimorra/middleware-public/internal/events"
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
//
// The contract is V1's, field for field and path for path: a site's code
// written against V1 (MIDDLEWARE_API is the bare address, the routes are
// /api/site/…, the answers carry chat, state, text, model, turns,
// cost_usd, json, question) moves here without a line changed. The same
// routes answer under /api/v1/site as well.

// SiteDoorOptions wire the site door.
type SiteDoorOptions struct {
	Chat   *chat.Manager
	Tokens *sitetoken.Keys
	Bus    *events.Bus
	// Lookup returns a site's manifest as the engine has it now.
	Lookup func(domain string) (*manifest.Manifest, bool)
	// SiteDir is the site's folder, for files named in a request.
	SiteDir func(domain string) string
	// HouseDaily is the daily cap of a site whose manifest names none.
	HouseDaily func() float64
	// Ledger reads what a book spent on a day.
	Spent func(book string, at time.Time) float64
	// Available says whether Claude can run on this machine now.
	Available func() bool
	// DefaultModel is the house's model, for a site that names none.
	DefaultModel func() string
	// Models are the models a request may name.
	Models func() []string
	// Domains lists the sites, for the usage of doors open at zero.
	Domains func() []string
	// API and Socket are what the panel shows a developer: where the
	// door answers, from the machine and from a container.
	API, Socket string
}

// DefaultMaxChats is how many conversations a site keeps when its manifest
// says nothing.
const DefaultMaxChats = 20

// doorRules are a site's limits resolved: what its manifest says and,
// where it is silent, the defaults. One place, so the panel never shows
// one number while the door applies another.
type doorRules struct {
	Tools    string  `json:"tools"`
	DailyUSD float64 `json:"daily_usd"`
	MaxChats int     `json:"max_chats"`
	Model    string  `json:"model,omitempty"`
	Hide     bool    `json:"hide_chats"`
}

func rulesOf(c *manifest.ClaudeAPI, houseDaily float64) doorRules {
	r := doorRules{Tools: "none", DailyUSD: houseDaily, MaxChats: DefaultMaxChats}
	if c == nil {
		return r
	}
	r.Tools = chat.Level(c.Tools).Name
	if c.DailyUSD > 0 {
		r.DailyUSD = c.DailyUSD
	}
	if c.MaxChats > 0 {
		r.MaxChats = c.MaxChats
	}
	r.Model, r.Hide = c.Model, c.HideChats
	return r
}

type doorCaller struct {
	Domain string
	Rules  doorRules
}

// RegisterSiteDoor adds /site and /site/claude/…. The routes are public to
// the API's own authentication and check the site token themselves.
func (s *Server) RegisterSiteDoor(o SiteDoorOptions) {
	s.registerDoorPanel(o)
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
	route("GET", p, "Who this site is and what it may ask: tools, model, limits, spent and left today, conversations open and at work", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		spent := o.spent(c.Domain)
		return map[string]any{"domain": c.Domain, "claude": map[string]any{
			"available":  o.Available == nil || o.Available(),
			"model":      o.model(c, ""),
			"tools":      c.Rules.Tools,
			"limits":     c.Rules,
			"spent_usd":  round4(spent),
			"left_usd":   round4(max(0, c.Rules.DailyUSD-spent)),
			"chats_open": len(o.siteChats(c.Domain)),
			"busy":       o.busy(c.Domain),
		}}, nil
	})
	route("POST", p+"/claude/ask", "One question and its answer in one call (body: prompt, system?, model?, title?, format?: json, schema?, wait?, files?)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		var b askBody
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		if b.said() == "" && len(b.Files) == 0 {
			return nil, Errorf(http.StatusBadRequest, "prompt: empty")
		}
		name, err := o.open(c, b)
		if err != nil {
			return nil, err
		}
		return o.sendAndAnswer(w, r, c, name, b)
	})
	route("GET", p+"/claude/chats", "This site's conversations", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		out := []map[string]any{}
		for _, sm := range o.siteChats(c.Domain) {
			if conv, err := o.Chat.Conversation(sm.Name); err == nil {
				out = append(out, infoOf(conv))
			}
		}
		return map[string]any{"chats": out}, nil
	})
	route("POST", p+"/claude/chats", "Open a conversation (body: title?, system?, model?, format?, schema?; prompt? sends a first message)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		var b askBody
		// No body is fine: open now, talk later.
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &b); err != nil {
				return nil, err
			}
		}
		name, err := o.open(c, b)
		if err != nil {
			return nil, err
		}
		if b.said() == "" && len(b.Files) == 0 {
			conv, err := o.Chat.Conversation(name)
			if err != nil {
				return nil, err
			}
			return map[string]any{"chat": infoOf(conv)}, nil
		}
		return o.sendAndAnswer(w, r, c, name, b)
	})
	route("GET", p+"/claude/chats/{id}", "One conversation: its state, the last answer and its messages (?limit=150, the last ones)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		conv, err := o.Chat.Conversation(name)
		if err != nil {
			return nil, err
		}
		limit := 150
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
			limit = v
		}
		msgs := MessagesOf(conv)
		more := false
		if len(msgs) > limit {
			msgs, more = msgs[len(msgs)-limit:], true
		}
		state := "done"
		if conv.Running {
			state = "running"
		}
		return map[string]any{"chat": infoOf(conv), "state": state, "text": said(conv), "messages": msgs, "more": more}, nil
	})
	route("POST", p+"/claude/chats/{id}/messages", "Send the next message (body: prompt, model?, format?, schema?, wait?, files?)", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		var b askBody
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		// The limits are checked here too and not only on opening: a
		// conversation opened yesterday under yesterday's cap must not be
		// the way around today's.
		if !o.Chat.Running(name) {
			if err := o.allowance(c); err != nil {
				return nil, err
			}
		}
		if err := o.knownModel(b.Model); err != nil {
			return nil, err
		}
		return o.sendAndAnswer(w, r, c, name, b)
	})
	route("GET", p+"/claude/chats/{id}/stream", "The conversation as it happens, as server-sent events: a message per event, and the conversation's state after each turn", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		o.stream(w, r, name)
		return Handled, nil
	})
	route("POST", p+"/claude/chats/{id}/files", "Attach a file to the next message: multipart (field \"file\") or the raw body with ?name=; 25 MB at most", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		r.Body = http.MaxBytesReader(w, r.Body, chat.MaxUpload+1<<20)
		file, body, err := oneUpload(r)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		defer body.Close()
		up, err := o.Chat.AddUpload(name, file, body)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return map[string]any{"ok": true, "file": attachmentOf(up)}, nil
	})
	route("DELETE", p+"/claude/chats/{id}/files/{name}", "Drop an attachment before it is sent", func(w http.ResponseWriter, r *http.Request, c doorCaller) (any, error) {
		name, err := o.siteChat(r, c)
		if err != nil {
			return nil, err
		}
		if err := o.Chat.RemoveUpload(name, r.PathValue("name")); err != nil {
			return nil, Errorf(http.StatusNotFound, "%v", err)
		}
		return OK, nil
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
		return map[string]any{"ok": true, "killed": o.Chat.Reap(name)}, nil
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
		return doorCaller{}, Errorf(http.StatusForbidden, "%s has no Claude door open: it is opened in its page in the panel (or with claude_api in its manifest)", domain)
	}
	return doorCaller{Domain: domain, Rules: rulesOf(m.ClaudeAPI, o.houseDaily())}, nil
}

func (o SiteDoorOptions) houseDaily() float64 {
	if o.HouseDaily != nil {
		return o.HouseDaily()
	}
	return 5
}

func (o SiteDoorOptions) spent(domain string) float64 {
	if o.Spent == nil {
		return 0
	}
	return o.Spent(chat.SiteDoorBook(domain), time.Now())
}

// siteChats are the conversations the site's code opened, newest first.
func (o SiteDoorOptions) siteChats(domain string) []chat.Summary {
	var out []chat.Summary
	for _, sm := range o.Chat.List(domain) {
		if sm.Origin == chat.OriginSite {
			out = append(out, sm)
		}
	}
	return out
}

// busy is how many of them are at work now.
func (o SiteDoorOptions) busy(domain string) int {
	n := 0
	for _, sm := range o.siteChats(domain) {
		if sm.Running {
			n++
		}
	}
	return n
}

func (o SiteDoorOptions) model(c doorCaller, asked string) string {
	if m := strings.TrimSpace(asked); m != "" {
		return m
	}
	if c.Rules.Model != "" {
		return c.Rules.Model
	}
	if o.DefaultModel != nil {
		return o.DefaultModel()
	}
	return ""
}

// knownModel refuses a model that is not on the list: a typo would
// otherwise reach the CLI and come back as a failed turn that was paid
// for.
func (o SiteDoorOptions) knownModel(m string) error {
	m = strings.TrimSpace(m)
	if m == "" || o.Models == nil {
		return nil
	}
	for _, k := range o.Models() {
		if strings.EqualFold(k, m) {
			return nil
		}
	}
	return Errorf(http.StatusBadRequest, "model %q is not on the list (%s)", m, strings.Join(o.Models(), ", "))
}

// allowance checks the money before a turn starts, not on the invoice.
func (o SiteDoorOptions) allowance(c doorCaller) error {
	if o.Available != nil && !o.Available() {
		return Errorf(http.StatusServiceUnavailable, "there is no Claude Code on this machine: enable the Claude module")
	}
	if spent := o.spent(c.Domain); spent >= c.Rules.DailyUSD {
		return Errorf(http.StatusTooManyRequests, "%s spent $%.2f through its door today and its cap is $%.2f; raise claude_api.daily_usd or wait until tomorrow (UTC)", c.Domain, spent, c.Rules.DailyUSD)
	}
	return nil
}

// open opens a conversation of the site with its limits in front: the
// money, the model, and room made among its old conversations.
func (o SiteDoorOptions) open(c doorCaller, b askBody) (string, error) {
	if err := o.allowance(c); err != nil {
		return "", err
	}
	// The model it is opened with is the one it carries on with, the
	// house's written in when the site names none: changing model halfway
	// and not saying so is the explanation nobody finds afterwards.
	model := o.model(c, b.Model)
	if err := o.knownModel(model); err != nil {
		return "", err
	}
	o.Chat.Prune(c.Domain, chat.OriginSite, c.Rules.MaxChats)
	name, err := o.Chat.Create(c.Domain, chat.CreateOptions{
		Origin: chat.OriginSite, Title: strings.TrimSpace(b.Title), System: systemFor(c.Domain, b), Tools: c.Rules.Tools,
		Model: model, Hidden: c.Rules.Hide, Max: c.Rules.MaxChats,
	})
	if errors.Is(err, chat.ErrTooMany) {
		// Only when every one of them is at work or waiting for an answer.
		return "", Errorf(http.StatusTooManyRequests, "%v: all of them are at work (claude_api.max_chats)", err)
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
	// System is the conversation's brief: who Claude is in it. It is kept
	// and holds for every turn.
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
	// Files are files of the site's own folder that go with the message.
	// They are copied, so they work with tools "none" too, and what goes
	// is the file as it is now.
	Files []string `json:"files"`
}

func (b askBody) said() string {
	if s := strings.TrimSpace(b.Prompt); s != "" {
		return s
	}
	return strings.TrimSpace(b.Text)
}

func (b askBody) wantsJSON() bool {
	return strings.EqualFold(strings.TrimSpace(b.Format), "json") || len(b.Schema) > 0
}

// systemFor is the conversation's brief: the site's own, with the JSON
// instruction when the answer is for a program. In the brief and not in
// the message, so it holds for every turn and is said once.
func systemFor(domain string, b askBody) string {
	own := strings.TrimSpace(b.System)
	if b.wantsJSON() {
		if own == "" {
			own = chat.SiteIntro(domain)
		}
		own += "\n\n" + jsonBrief(b.Schema)
	}
	return own
}

func jsonBrief(schema json.RawMessage) string {
	s := "Answer with valid JSON only: no text before or after it and no code fence."
	if len(schema) > 0 {
		s += " It must fit this JSON Schema:\n\n" + string(schema)
	}
	return s
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

func (o SiteDoorOptions) sendAndAnswer(w http.ResponseWriter, r *http.Request, c doorCaller, name string, b askBody) (any, error) {
	text := b.said()
	if err := o.attachPaths(c, name, b.Files); err != nil {
		return nil, err
	}
	if text == "" && len(b.Files) > 0 {
		text = "Look at what I sent you."
	}
	if text == "" {
		return nil, Errorf(http.StatusBadRequest, "prompt: empty")
	}
	// A conversation opened without the JSON brief and asked for JSON now:
	// the instruction goes with this message.
	if b.wantsJSON() {
		if conv, err := o.Chat.Conversation(name); err == nil && !strings.Contains(conv.System, "valid JSON only") {
			text += "\n\n" + jsonBrief(b.Schema)
		}
	}
	_, id, _ := strings.Cut(name, "#")
	if _, err := o.Chat.SendWith(name, text, chat.SendOptions{Model: strings.TrimSpace(b.Model)}); err != nil {
		if errors.Is(err, chat.ErrBusy) {
			WriteJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "chat": id})
			return Handled, nil
		}
		return nil, chatError(err)
	}
	running := func(why string) (any, error) {
		out := map[string]any{"chat": id, "state": "running"}
		if why != "" {
			out["why"] = why
		}
		WriteJSON(w, http.StatusAccepted, out)
		return Handled, nil
	}
	wait := waitFor(b.Wait)
	if wait == 0 {
		return running("")
	}
	// The request's context and the deadline: if the caller goes away
	// this stops waiting, but the turn goes on and its answer is written
	// in the conversation. Killing it would throw away what was paid for.
	if _, done := o.Chat.Wait(r.Context(), name, wait); !done {
		return running("still running after " + wait.String() + ": GET the conversation, or its stream, for the answer")
	}
	conv, err := o.Chat.Conversation(name)
	if err != nil {
		return nil, err
	}
	return answerOf(conv, b), nil
}

// answerOf is the answer as the caller reads it.
func answerOf(conv chat.Conversation, b askBody) map[string]any {
	info := infoOf(conv)
	text := said(conv)
	out := map[string]any{
		"chat":     conv.ID,
		"state":    "done",
		"text":     text,
		"model":    conv.Model,
		"turns":    info["turns"],
		"cost_usd": info["cost_usd"],
	}
	if n := len(conv.Turns); n > 0 && conv.Turns[n-1].Role == "error" {
		out["error"] = conv.Turns[n-1].Text
	}
	if b.wantsJSON() {
		if v, err := looseJSON(text); err == nil {
			out["json"] = v
		} else {
			// Said here, not discovered by the caller's JSON.parse with an
			// error that names nobody.
			out["json_error"] = err.Error()
		}
	}
	// Claude may end by asking, and on the other side nobody presses a
	// button: said apart so the code can tell "answered" from "asked".
	if q := question(conv); q != "" {
		out["question"] = q
	}
	return out
}

// said is the last thing the agent answered.
func said(conv chat.Conversation) string {
	for i := len(conv.Turns) - 1; i >= 0; i-- {
		if t := conv.Turns[i]; t.Role != "user" {
			return t.Text
		}
	}
	return ""
}

func question(conv chat.Conversation) string {
	if !chat.Asking(conv) {
		return ""
	}
	return conv.Turns[len(conv.Turns)-1].Questions[0].Question
}

// infoOf is a conversation as V1's chat.Info described it.
func infoOf(c chat.Conversation) map[string]any {
	turns, cost := 0, 0.0
	updated := c.Created
	for _, t := range c.Turns {
		if t.Role != "user" {
			turns++
			cost += t.Cost
		}
		if t.At.After(updated) {
			updated = t.At
		}
	}
	return map[string]any{
		"id": c.ID, "domain": c.Domain, "title": c.Title, "model": c.Model,
		"created": c.Created, "updated": updated, "turns": turns, "cost_usd": round4(cost),
		"busy": c.Running, "messages": len(MessagesOf(c)), "origin": c.Origin,
		"system": c.System, "tools": c.Tools, "asking": chat.Asking(c),
	}
}

// Message is one line of a conversation as V1's panel and site door
// showed it: what someone said, a tool used (with its answer once back),
// or a turn's end.
type Message struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Role    string    `json:"role"` // user, assistant, tool, result
	Text    string    `json:"text,omitempty"`
	Tool    *DoorTool `json:"tool,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	CostUSD float64   `json:"cost_usd,omitempty"`
	Error   bool      `json:"error,omitempty"`
}

// DoorTool is a tool use in a Message.
type DoorTool struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Input      string `json:"input,omitempty"`
	Result     string `json:"result,omitempty"`
	Error      bool   `json:"error,omitempty"`
	Done       bool   `json:"done"`
	Run        string `json:"run,omitempty"`
	Background bool   `json:"background,omitempty"`
}

func doorTool(t *chat.ToolUse, background bool) *DoorTool {
	in := ""
	if len(t.Input) > 0 {
		b, _ := json.Marshal(t.Input)
		in = string(b)
	}
	return &DoorTool{ID: t.ID, Name: t.Name, Input: in, Result: t.Output, Error: t.Error, Done: t.Done, Run: t.Run, Background: background}
}

// MessagesOf lays a conversation out as messages, oldest first.
func MessagesOf(c chat.Conversation) []Message {
	var out []Message
	add := func(m Message) {
		m.Seq = int64(len(out) + 1)
		out = append(out, m)
	}
	for _, t := range c.Turns {
		if t.Role == "user" {
			add(Message{At: t.At, Role: "user", Text: t.Text})
			continue
		}
		bg := map[string]bool{}
		for _, th := range t.Threads {
			bg[th.ID] = th.Background
		}
		steps := func(steps []chat.Step, agent string) {
			for _, st := range steps {
				if st.Tool != nil {
					add(Message{At: t.At, Role: "tool", Tool: doorTool(st.Tool, bg[st.Tool.ID]), Agent: agent})
				} else if st.Text != "" {
					add(Message{At: t.At, Role: "assistant", Text: st.Text, Agent: agent})
				}
			}
		}
		if len(t.Steps) > 0 {
			steps(t.Steps, "")
			for _, th := range t.Threads {
				steps(th.Steps, th.ID)
			}
		} else if t.Text != "" && t.Role != "error" {
			add(Message{At: t.At, Role: "assistant", Text: t.Text})
		}
		res := Message{At: t.At, Role: "result", CostUSD: round4(t.Cost), Error: t.Role == "error"}
		if t.Role == "error" {
			res.Text = t.Text
		}
		add(res)
	}
	if out == nil {
		out = []Message{}
	}
	return out
}

// attachPaths puts files of the site's own folder on the next message.
// They are copied into the conversation's attachments rather than passed
// by path: so they work with tools "none", where Claude cannot open a
// file by itself, and what goes is the file as it is now.
func (o SiteDoorOptions) attachPaths(c doorCaller, name string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if o.SiteDir == nil {
		return Errorf(http.StatusBadRequest, "files cannot be attached here")
	}
	dir := o.SiteDir(c.Domain)
	for _, p := range paths {
		full, err := insideDir(dir, p)
		if err != nil {
			return Errorf(http.StatusBadRequest, "%v", err)
		}
		f, err := openRegular(full)
		if err != nil {
			return Errorf(http.StatusBadRequest, "%s: %v", p, err)
		}
		_, err = o.Chat.AddUpload(name, filepath.Base(filepath.Clean(p)), f)
		f.Close()
		if err != nil {
			return Errorf(http.StatusBadRequest, "%s: %v", p, err)
		}
	}
	return nil
}

// insideDir resolves a name against the site's folder and refuses one
// that leaves it: what arrives here is written by the site's code, and a
// path that ends in an os.Open is how the neighbour's .env is read with
// the house's eyes. Symbolic links are followed before deciding.
func insideDir(dir, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("a file in the list has no name")
	}
	full := filepath.Join(dir, name)
	if filepath.IsAbs(name) {
		full = filepath.Clean(name)
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("%q: %v", name, err)
	}
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside this site's folder", name)
	}
	return real, nil
}

// openRegular opens a real file: a directory, a socket or a device is not
// an attachment, and what would be read from /dev/zero is a full disk.
func openRegular(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("not a file")
	}
	return f, nil
}

// oneUpload takes the file from the request however it comes: a form, as
// a browser sends it, or the raw body with the name in the address, as
// three lines of code send it.
func oneUpload(r *http.Request) (string, io.ReadCloser, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		mr, err := r.MultipartReader()
		if err != nil {
			return "", nil, errors.New("not a file upload")
		}
		for {
			part, err := mr.NextPart()
			if err != nil {
				return "", nil, errors.New("no file came with it (field \"file\")")
			}
			if part.FormName() == "file" && part.FileName() != "" {
				return part.FileName(), part, nil
			}
			_ = part.Close()
		}
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		return "", nil, errors.New("name the file in ?name=")
	}
	return name, r.Body, nil
}

func attachmentOf(u chat.Upload) map[string]any {
	kind := "document"
	switch strings.ToLower(filepath.Ext(u.Name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		kind = "image"
	}
	return map[string]any{"name": u.Name, "size": u.Bytes, "kind": kind, "at": time.Now().UTC()}
}

// stream is the conversation as it happens: an "event: message" per
// message, as GET returns them, and an "event: session" with the
// conversation after each turn ends, which is when a client puts its
// button back.
func (o SiteDoorOptions) stream(w http.ResponseWriter, r *http.Request, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok || o.Bus == nil {
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming is not available"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	ch, stop := o.Bus.Subscribe()
	defer stop()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	var seq int64
	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Subject != name {
				continue
			}
			m, end := liveMessage(e)
			if m == nil {
				continue
			}
			seq++
			m.Seq = seq
			if !send("message", m) {
				return
			}
			if end {
				if conv, err := o.Chat.Conversation(name); err == nil && !send("session", infoOf(conv)) {
					return
				}
			}
		}
	}
}

// liveMessage turns a progress event into the message it stands for.
func liveMessage(e events.Event) (*Message, bool) {
	d := e.Data
	str := func(k string) string { s, _ := d[k].(string); return s }
	b := func(k string) bool { v, _ := d[k].(bool); return v }
	m := &Message{At: e.Time, Agent: str("thread")}
	switch e.Type {
	case "agent.text":
		m.Role, m.Text = "assistant", str("text")
	case "agent.tool":
		in := ""
		if v, ok := d["input"]; ok {
			raw, _ := json.Marshal(v)
			in = string(raw)
		}
		bg := false
		if inp, ok := d["input"].(map[string]any); ok {
			bg, _ = inp["run_in_background"].(bool)
		}
		m.Role, m.Tool = "tool", &DoorTool{ID: str("id"), Name: str("tool"), Input: in, Run: str("run"), Background: bg}
	case "agent.tool.result":
		m.Role, m.Tool = "tool", &DoorTool{ID: str("id"), Result: str("output"), Error: b("error"), Done: true, Run: str("run")}
	case "agent.subagent":
		m.Role, m.Tool = "tool", &DoorTool{ID: str("id"), Run: str("run")}
	case "agent.done":
		cost, _ := d["cost"].(float64)
		m.Role, m.CostUSD, m.Agent = "result", round4(cost), ""
		return m, true
	case "agent.error":
		if str("turn_id") == "" {
			return nil, false
		}
		cost, _ := d["cost"].(float64)
		m.Role, m.CostUSD, m.Error, m.Text, m.Agent = "result", round4(cost), true, e.Message, ""
		return m, true
	default:
		return nil, false
	}
	return m, false
}

// looseJSON reads the JSON in an answer that was asked to be only JSON
// and sometimes still comes in a fence or after a line of prose. It does
// not repair JSON: what is between the braces either reads or is said not
// to.
func looseJSON(said string) (any, error) {
	s := strings.TrimSpace(said)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if j := strings.Index(rest, "\n"); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.Index(rest, "```"); k >= 0 {
			s = strings.TrimSpace(rest[:k])
		}
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v, nil
	}
	if i := strings.IndexAny(s, "{["); i > 0 {
		s = s[i:]
	}
	if i := strings.LastIndexAny(s, "}]"); i >= 0 && i < len(s)-1 {
		s = s[:i+1]
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("the answer is not JSON: %w", err)
	}
	return v, nil
}

func round4(v float64) float64 { return float64(int64(v*10000+0.5)) / 10000 }

// V1Site is where V1 served the site door. A site's code written for V1
// calls $MIDDLEWARE_API/api/site/…; the panel's handler maps it onto
// Prefix/site/….
const V1Site = "/api/site"

// SiteDoorPath maps a V1 site door path to this API's, and says whether
// it is one.
func SiteDoorPath(path string) (string, bool) {
	if path == V1Site || strings.HasPrefix(path, V1Site+"/") {
		return Prefix + strings.TrimPrefix(path, "/api"), true
	}
	if path == Prefix+"/site" || strings.HasPrefix(path, Prefix+"/site/") {
		return path, true
	}
	return path, false
}

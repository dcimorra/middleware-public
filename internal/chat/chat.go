// Package chat is one conversation per site, plus the reserved one named
// "middleware" about the installation itself. A turn is a provider run
// inside the site's jail; its events are re-emitted on the bus with the
// domain as subject so the panel streams them, and both sides of the
// exchange are written to data/chat/<domain>.json with the provider's
// session id, so the next turn resumes where this one ended.
package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// Middleware is the reserved conversation name. It is not a valid domain,
// so it can never collide with a site folder.
const Middleware = "middleware"

// Turn is one message, either side.
type Turn struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"` // "user" | "assistant" | "error"
	Text      string    `json:"text"`
	Cost      float64   `json:"cost,omitempty"`
	At        time.Time `json:"at"`
	SessionID string    `json:"session_id,omitempty"`
	Tools     []string  `json:"tools,omitempty"`
	// Questions are the decisions the agent asked for in this turn.
	Questions []Question `json:"questions,omitempty"`
}

// Conversation is data/chat/<domain>.json.
type Conversation struct {
	Domain    string `json:"domain"`
	Provider  string `json:"provider"`
	Model     string `json:"model,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Turns     []Turn `json:"turns"`
	// Running is not stored: it is filled by History from the manager.
	Running bool `json:"running,omitempty"`
}

// Options configure a Manager.
type Options struct {
	Root     string
	Bus      *events.Bus
	Registry *agent.Registry
	// DefaultProvider is used when the site's manifest names none. It
	// overrides Limits.DefaultProvider; empty takes the latter.
	DefaultProvider string
	// Ledger defaults to one under Root/data.
	Ledger *agent.Ledger
	// Limits is the concurrency section of the settings; a zero value
	// takes the defaults. SetLimits changes it live.
	Limits settings.Agents
	// Path, when set, is the PATH a turn runs with instead of the
	// supervisor's: the wiring puts the managed CLIs and their Node first.
	Path func() string
}

// Manager runs the conversations.
type Manager struct {
	opts    Options
	mu      sync.Mutex
	running map[string]*running
	limits  settings.Agents
}

type running struct {
	turnID   string
	provider string
	started  time.Time
	run      agent.Run
	cancel   context.CancelFunc
}

// ErrBusy says a turn is already running for that conversation.
var ErrBusy = errors.New("a turn is already running")

// ErrCapacity says the house, or one provider, has no free slot. The
// numbers are what the caller shows: a 429 that says "8 of 8" is one the
// operator can act on in Settings.
type ErrCapacity struct {
	Used, Max int
	// Provider is set when the provider's own cap was the one hit.
	Provider                  string
	ProviderUsed, ProviderMax int
}

// Error implements error.
func (e *ErrCapacity) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("no capacity for %s: %d of %d %s turns running; raise agents.per_provider.%s in Settings or wait", e.Provider, e.ProviderUsed, e.ProviderMax, e.Provider, e.Provider)
	}
	return fmt.Sprintf("no capacity: %d of %d agent turns running; raise agents.max_concurrent in Settings or wait", e.Used, e.Max)
}

// New returns a manager. Nothing runs until Send.
func New(opts Options) *Manager {
	if opts.Bus == nil {
		opts.Bus = events.New("")
	}
	if opts.Registry == nil {
		opts.Registry = agent.NewRegistry()
	}
	if opts.Ledger == nil {
		opts.Ledger = agent.NewLedger(filepath.Join(opts.Root, "data"))
	}
	// A relative root would put relative paths in the jail's deny list and
	// in the child's HOME, and a deny rule is only as good as its path.
	if abs, err := filepath.Abs(opts.Root); err == nil {
		opts.Root = abs
	}
	m := &Manager{opts: opts, running: map[string]*running{}}
	m.SetLimits(opts.Limits)
	return m
}

// SetLimits replaces the concurrency limits at once. Turns already
// running are not stopped by a lower cap: they finish, and the next Send
// sees the new numbers. The default provider follows the settings unless
// the Options pinned one.
func (m *Manager) SetLimits(a settings.Agents) {
	d := settings.DefaultAgents()
	if a.MaxConcurrent == 0 && a.DefaultProvider == "" && a.PerProvider == nil {
		a = d
	}
	if a.Validate() != nil {
		if a.MaxConcurrent < 1 || a.MaxConcurrent > settings.MaxAgents {
			a.MaxConcurrent = d.MaxConcurrent
		}
		if a.ReservedForHouse < 0 || a.ReservedForHouse >= a.MaxConcurrent {
			a.ReservedForHouse = 0
			if a.MaxConcurrent > 1 {
				a.ReservedForHouse = d.ReservedForHouse
			}
		}
		if a.DefaultProvider == "" {
			a.DefaultProvider = d.DefaultProvider
		}
	}
	if m.opts.DefaultProvider != "" {
		a.DefaultProvider = m.opts.DefaultProvider
	}
	per := map[string]int{}
	for k, v := range a.PerProvider {
		if v > 0 {
			per[k] = v
		}
	}
	a.PerProvider = per
	m.mu.Lock()
	m.limits = a
	m.mu.Unlock()
}

// Capacity is the live picture: slots used and available, per provider,
// and which conversations hold them.
type Capacity struct {
	Used      int                         `json:"used"`
	Max       int                         `json:"max"`
	Reserved  int                         `json:"reserved_for_house"`
	Providers map[string]ProviderCapacity `json:"providers"`
	Running   []RunningTurn               `json:"running"`
}

// ProviderCapacity is one provider's share; Max 0 means no cap of its own.
type ProviderCapacity struct {
	Used int `json:"used"`
	Max  int `json:"max"`
}

// RunningTurn is one conversation with a turn in progress.
type RunningTurn struct {
	Domain   string    `json:"domain"`
	Provider string    `json:"provider"`
	Started  time.Time `json:"started"`
	TurnID   string    `json:"turn_id"`
}

// Capacity returns the live numbers.
func (m *Manager) Capacity() Capacity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capacityLocked()
}

func (m *Manager) capacityLocked() Capacity {
	c := Capacity{Used: len(m.running), Max: m.limits.MaxConcurrent, Reserved: m.limits.ReservedForHouse, Providers: map[string]ProviderCapacity{}, Running: []RunningTurn{}}
	for _, key := range m.opts.Registry.Keys() {
		c.Providers[key] = ProviderCapacity{Max: m.limits.PerProvider[key]}
	}
	for domain, r := range m.running {
		p := c.Providers[r.provider]
		p.Used++
		if p.Max == 0 {
			p.Max = m.limits.PerProvider[r.provider]
		}
		c.Providers[r.provider] = p
		c.Running = append(c.Running, RunningTurn{Domain: domain, Provider: r.provider, Started: r.started, TurnID: r.turnID})
	}
	sort.Slice(c.Running, func(i, j int) bool { return c.Running[i].Started.Before(c.Running[j].Started) })
	return c
}

// admit decides, under the lock, whether a turn for domain on provider
// may start now. The house keeps its reserved slots: a site is refused
// once used reaches max minus reserved, the house once it reaches max.
func (m *Manager) admit(domain, provider string) error {
	used := len(m.running)
	limit := m.limits.MaxConcurrent
	if domain != Middleware {
		limit -= m.limits.ReservedForHouse
	}
	if used >= limit {
		return &ErrCapacity{Used: used, Max: m.limits.MaxConcurrent}
	}
	if pmax := m.limits.PerProvider[provider]; pmax > 0 {
		pused := 0
		for _, r := range m.running {
			if r.provider == provider {
				pused++
			}
		}
		if pused >= pmax {
			return &ErrCapacity{Used: used, Max: m.limits.MaxConcurrent, Provider: provider, ProviderUsed: pused, ProviderMax: pmax}
		}
	}
	return nil
}

func (m *Manager) file(domain string) string {
	return filepath.Join(m.opts.Root, "data", "chat", domain+".json")
}

// workdir resolves where a conversation runs and fails for anything that
// is not a site folder or the reserved name: a domain that merely looks
// right must not open a conversation in a folder that does not exist.
func (m *Manager) workdir(domain string) (string, error) {
	if domain == Middleware {
		return m.opts.Root, nil
	}
	if !manifest.ValidDomain(domain) {
		return "", fmt.Errorf("%q is not a domain", domain)
	}
	dir := filepath.Join(m.opts.Root, "sites", domain)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s: no such site", domain)
	}
	return dir, nil
}

// Settings is what a conversation would run with: provider and model
// from the manifest with the manager's default, the cap, the jail.
type Settings struct {
	Provider string
	Model    string
	Cap      float64
	Workdir  string
	Jail     agent.Jail
}

// Resolve computes the settings for domain without starting anything.
func (m *Manager) Resolve(domain string) (Settings, error) {
	dir, err := m.workdir(domain)
	if err != nil {
		return Settings{}, err
	}
	m.mu.Lock()
	s := Settings{Provider: m.limits.DefaultProvider, Workdir: dir}
	m.mu.Unlock()
	if domain != Middleware {
		site := agent.ReadSiteSettings(dir)
		if site.Provider != "" {
			s.Provider = site.Provider
		}
		s.Model = site.Model
		s.Cap = site.MonthlyCapUSD
	}
	home := filepath.Join(m.opts.Root, "data", "homes", domain)
	s.Jail = agent.NewJail(m.opts.Root, dir, home)
	return s, nil
}

// History returns the conversation, empty when it has not started.
func (m *Manager) History(domain string) (Conversation, error) {
	if _, err := m.workdir(domain); err != nil {
		return Conversation{}, err
	}
	c := m.load(domain)
	if c.Provider == "" {
		if s, err := m.Resolve(domain); err == nil {
			c.Provider, c.Model = s.Provider, s.Model
		}
	}
	m.mu.Lock()
	_, c.Running = m.running[domain]
	m.mu.Unlock()
	return c, nil
}

func (m *Manager) load(domain string) Conversation {
	var c Conversation
	if err := fsx.ReadJSON(m.file(domain), &c); err != nil {
		c = Conversation{}
	}
	c.Domain = domain
	if c.Turns == nil {
		c.Turns = []Turn{}
	}
	return c
}

func (m *Manager) save(c Conversation) error {
	c.Running = false
	return fsx.WriteJSON(m.file(c.Domain), c, 0o600)
}

// Reset forgets the conversation: the next turn starts a new session.
func (m *Manager) Reset(domain string) error {
	if _, err := m.workdir(domain); err != nil {
		return err
	}
	m.mu.Lock()
	_, busy := m.running[domain]
	m.mu.Unlock()
	if busy {
		return ErrBusy
	}
	if err := os.Remove(m.file(domain)); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.emit("chat.reset", domain, "", "conversation reset", nil)
	return nil
}

// Running reports whether a turn is in progress.
func (m *Manager) Running(domain string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.running[domain]
	return ok
}

// Stop ends the running turn, if any.
func (m *Manager) Stop(domain string) error {
	m.mu.Lock()
	r, ok := m.running[domain]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: no turn is running", domain)
	}
	r.run.Stop()
	r.cancel()
	return nil
}

// Send starts a turn and returns at once with its id; progress arrives on
// the bus. At most one turn runs per conversation: two agents editing the
// same folder at the same time would each undo the other.
func (m *Manager) Send(domain, text, model string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("empty message")
	}
	s, err := m.Resolve(domain)
	if err != nil {
		return "", err
	}
	provider, err := m.opts.Registry.Get(s.Provider)
	if err != nil {
		return "", err
	}
	if ok, why := provider.Available(); !ok {
		return "", fmt.Errorf("%s: %s", s.Provider, why)
	}
	if err := m.opts.Ledger.Allow(domain, s.Cap, time.Now()); err != nil {
		m.emit("agent.cost_cap", domain, "warn", err.Error(), map[string]any{"cap": s.Cap})
		return "", err
	}
	if model == "" {
		model = s.Model
	}
	m.mu.Lock()
	if _, busy := m.running[domain]; busy {
		m.mu.Unlock()
		return "", ErrBusy
	}
	if err := m.admit(domain, s.Provider); err != nil {
		cap := m.capacityLocked()
		m.mu.Unlock()
		m.emit("agent.capacity", domain, "warn", err.Error(), map[string]any{"used": cap.Used, "max": cap.Max, "provider": s.Provider, "providers": cap.Providers})
		return "", err
	}
	turnID := newID()
	ctx, cancel := context.WithCancel(context.Background())
	m.running[domain] = &running{turnID: turnID, provider: s.Provider, started: time.Now(), cancel: cancel}
	m.mu.Unlock()

	fail := func(err error) (string, error) {
		cancel()
		m.mu.Lock()
		delete(m.running, domain)
		m.mu.Unlock()
		return "", err
	}
	c := m.load(domain)
	c.Provider, c.Model = s.Provider, model
	c.Turns = append(c.Turns, Turn{ID: turnID, Role: "user", Text: text, At: time.Now()})
	if err := m.save(c); err != nil {
		return fail(err)
	}
	// The skill goes in before every turn, not once at start: the home
	// may be new, and the binary may have been upgraded since the last one.
	if err := installSkillFor(s.Jail.Home, s.Provider); err != nil {
		return fail(fmt.Errorf("skill not installed: %w", err))
	}
	system := Preamble(domain, s.Workdir, s.Jail)
	if s.Provider == "gemini" {
		system = SkillHint + "\n\n" + system
	}
	req := agent.Request{
		Workdir:   s.Workdir,
		Prompt:    text,
		Model:     model,
		SessionID: c.SessionID,
		System:    system,
		Jail:      s.Jail,
		Env:       map[string]string{"PATH": m.agentPath()},
	}
	run, err := provider.Start(ctx, req)
	if err != nil {
		m.emit("agent.error", domain, "error", err.Error(), map[string]any{"turn_id": turnID})
		return fail(err)
	}
	m.mu.Lock()
	m.running[domain].run = run
	m.mu.Unlock()
	m.emit("agent.started", domain, "", "", map[string]any{"turn_id": turnID, "provider": s.Provider, "model": model})
	go m.follow(domain, turnID, s, run, cancel)
	return turnID, nil
}

// follow consumes a run to its end and records the outcome.
func (m *Manager) follow(domain, turnID string, s Settings, run agent.Run, cancel context.CancelFunc) {
	defer cancel()
	var all []agent.Event
	var text strings.Builder
	var tools []string
	var questions []Question
	var cost float64
	sessionID := ""
	var errText string
	ended := false
	// Each text event is one complete piece of what the agent said, so a
	// question in it is whole and goes out as the piece arrives, not when
	// the turn ends: the operator can answer while the agent still works.
	ask := func(piece string) {
		for _, q := range ParseQuestions(piece) {
			questions = append(questions, q)
			m.emit("agent.question", domain, "", q.Question, map[string]any{"turn_id": turnID, "question": q.Question, "options": q.Options, "recommended": q.Recommended})
		}
	}
	for ev := range run.Events() {
		all = append(all, ev)
		if ev.SessionID != "" {
			sessionID = ev.SessionID
		}
		switch ev.Kind {
		case agent.KindText:
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(ev.Text)
			m.emit("agent.text", domain, "", "", map[string]any{"turn_id": turnID, "text": ev.Text})
			ask(ev.Text)
		case agent.KindTool:
			tools = append(tools, ev.Tool)
			m.emit("agent.tool", domain, "", "", map[string]any{"turn_id": turnID, "tool": ev.Tool, "input": ev.Input})
		case agent.KindCost:
			if ev.Cost > cost {
				cost = ev.Cost
			}
		case agent.KindResult:
			ended = true
			if ev.Cost > cost {
				cost = ev.Cost
			}
			if text.Len() == 0 {
				text.WriteString(ev.Text)
				ask(ev.Text)
			}
		case agent.KindError:
			ended = true
			errText = ev.Text
			if ev.Cost > cost {
				cost = ev.Cost
			}
		}
	}
	if !ended && errText == "" {
		errText = "the agent ended without a result"
	}
	if cost > 0 {
		if err := m.opts.Ledger.Add(domain, cost, time.Now()); err != nil {
			m.emit("agent.error", domain, "warn", "cost not recorded: "+err.Error(), map[string]any{"turn_id": turnID})
		}
	}
	// The audit runs on every turn, finished or not: a stopped turn may
	// already have read what it should not have.
	if v := s.Jail.Violations(agent.Transcript(all)); len(v) > 0 {
		m.emit("agent.jail_violation", domain, "error", fmt.Sprintf("the agent referred to %d denied path(s)", len(v)), map[string]any{"turn_id": turnID, "paths": v})
	}

	c := m.load(domain)
	if sessionID != "" {
		c.SessionID = sessionID
	}
	turn := Turn{ID: turnID, Role: "assistant", Text: text.String(), Cost: cost, At: time.Now(), SessionID: sessionID, Tools: tools, Questions: questions}
	if errText != "" {
		turn.Role = "error"
		if turn.Text == "" {
			turn.Text = errText
		} else {
			turn.Text += "\n\n" + errText
		}
	}
	c.Turns = append(c.Turns, turn)
	if err := m.save(c); err != nil {
		m.emit("agent.error", domain, "error", "conversation not saved: "+err.Error(), map[string]any{"turn_id": turnID})
	}
	// The turn counts as running until its file is written, and the final
	// event goes out only after that: whoever reacts to agent.done can
	// Send again and read the turn from History at once.
	m.mu.Lock()
	delete(m.running, domain)
	m.mu.Unlock()
	if errText != "" {
		m.emit("agent.error", domain, "error", errText, map[string]any{"turn_id": turnID, "cost": cost})
		return
	}
	m.emit("agent.done", domain, "", "", map[string]any{"turn_id": turnID, "cost": cost, "session_id": sessionID, "text": text.String(), "tools": len(tools)})
}

func (m *Manager) emit(typ, domain, level, msg string, data map[string]any) {
	m.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: "agent", Level: level, Message: msg, Data: data})
}

// SetPath replaces the PATH source a turn runs with.
func (m *Manager) SetPath(f func() string) {
	m.mu.Lock()
	m.opts.Path = f
	m.mu.Unlock()
}

// agentPath is the PATH the agent runs with: the supervisor's, plus the
// folder this binary lives in when it is not there yet, so `middleware
// site add` works from a conversation whether the binary was installed
// under /usr/local/bin or is being run from a build folder.
func (m *Manager) agentPath() string {
	path := os.Getenv("PATH")
	if m.opts.Path != nil {
		if p := m.opts.Path(); p != "" {
			path = p
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return path
	}
	dir := filepath.Dir(exe)
	for _, p := range filepath.SplitList(path) {
		if p == dir {
			return path
		}
	}
	if path == "" {
		return dir
	}
	return path + string(os.PathListSeparator) + dir
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Preamble tells the agent where it is and what it may not do. It is
// passed as system context so the user's own words stay the prompt.
func Preamble(domain, workdir string, j agent.Jail) string {
	var b strings.Builder
	if domain == Middleware {
		b.WriteString("You are working on the middleware installation itself, the supervisor that serves every site on this machine. ")
		b.WriteString("Your working directory is the installation root: " + workdir + ". ")
		b.WriteString("You may read and edit the installation's own files, its settings and its tasks. ")
		b.WriteString("You may create new sites, in either of two ways: make the folder sites/<domain>/ with the project inside and a site.json (the engine picks it up within a few seconds), ")
		b.WriteString("or run `middleware site add <domain> --template <static|node|go|docker>` or `middleware site add <domain> --manifest -` with the manifest on stdin; the command talks to the supervisor over its unix socket and needs no credentials. ")
		b.WriteString("Once a site exists, hand detailed work on it to that site's own conversation unless you are asked to continue yourself. ")
		b.WriteString("You must not edit a site that already existed when this turn began: each of those has its own conversation, and the paths listed below are denied.\n")
	} else {
		b.WriteString("You are working on the site " + domain + ", served by the middleware on this machine. ")
		b.WriteString("Your working directory is the site's folder: " + workdir + ". The whole project lives there, including site.json, its manifest. ")
		b.WriteString("You may read and edit anything inside that folder. ")
		b.WriteString("You must not read, write or run anything outside it: other sites, the middleware's data, its tasks or its root are off limits, and every tool call is audited for that.\n")
	}
	b.WriteString("Do not start long-running processes: the middleware supervises the site's process from its manifest, and a change to the manifest or to a pointer file is how a deployment happens.\n")
	b.WriteString("When this turn ends, its whole process group is killed. Anything that takes longer than a turn (a build, an install, a deployment) must be started detached, exactly like this: `setsid nohup <cmd> >/tmp/<name>.log 2>&1 </dev/null &`, and your reply must state the log's path so the next message can check how it went.\n")
	b.WriteString("When you need the operator to decide something, ask with a fenced block the panel turns into buttons; two to four short options, the one you would pick as recommended, or an empty options list for a free-text answer:\n")
	b.WriteString("```question\n{\"question\": \"Which database?\", \"options\": [\"SQLite\", \"Postgres\"], \"recommended\": \"SQLite\"}\n```\n")
	b.WriteString("Ask one thing at a time, then end your turn; the answer arrives as the next message.\n")
	if len(j.Deny) > 0 {
		b.WriteString("Paths you must not touch:\n")
		for _, d := range j.Deny {
			b.WriteString("  - " + d + "\n")
		}
	}
	return b.String()
}

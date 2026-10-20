package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dcimorra/middleware-public/internal/agent"
	"github.com/dcimorra/middleware-public/internal/chat"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// AgentsOptions wire the agents section.
type AgentsOptions struct {
	Registry *agent.Registry
	Chat     *chat.Manager
}

// AgentInfo is one provider as GET /agents lists it.
type AgentInfo struct {
	Provider  string   `json:"provider"`
	Available bool     `json:"available"`
	Where     string   `json:"where,omitempty"`
	Models    []string `json:"models"`
}

// Agents is what GET /agents answers: the providers and the live capacity.
type Agents struct {
	Providers []AgentInfo    `json:"providers"`
	Capacity  *chat.Capacity `json:"capacity,omitempty"`
}

// RegisterAgents adds /agents, the per-site conversations and the
// installation's own one. The two conversation sets are the same handlers
// over different names: chat.Middleware is not a valid domain, which is
// what keeps the routes from colliding.
func (s *Server) RegisterAgents(o AgentsOptions) {
	s.Handle("GET", Prefix+"/agents", "The agent providers, whether each can run and its models, with the live capacity", func(w http.ResponseWriter, r *http.Request) (any, error) {
		out := Agents{Providers: []AgentInfo{}}
		if o.Registry != nil {
			for _, key := range o.Registry.Keys() {
				p, err := o.Registry.Get(key)
				if err != nil {
					continue
				}
				ok, where := p.Available()
				models := p.Models()
				if models == nil {
					models = []string{}
				}
				out.Providers = append(out.Providers, AgentInfo{Provider: key, Available: ok, Where: where, Models: models})
			}
		}
		if o.Chat != nil {
			c := o.Chat.Capacity()
			out.Capacity = &c
		}
		return out, nil
	})
	if o.Chat == nil {
		return
	}
	s.Handle("GET", Prefix+"/agents/capacity", "How many agent turns run now, of how many, per provider, and which conversations hold them", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return o.Chat.Capacity(), nil
	})
	// A new cap applies to the next turn, not to the next restart.
	s.OnSettingsSaved(func(st settings.Settings) { o.Chat.SetLimits(st.Agents) })
	s.registerChat(o.Chat, Prefix+"/sites/{domain}/chat", domainOf)
	s.registerChat(o.Chat, Prefix+"/chat/middleware", func(*http.Request) (string, error) { return chat.Middleware, nil })
	// More conversations per site: the panel's own beyond the first, and
	// the ones the site's code opened through the site door, which the
	// panel reads and stops but does not write into (a program is on the
	// other side of those, and its turns follow its own rules).
	s.registerChat(o.Chat, Prefix+"/sites/{domain}/chats/{id}", func(r *http.Request) (string, error) {
		domain, err := domainOf(r)
		if err != nil {
			return "", err
		}
		name := chat.Name(domain, r.PathValue("id"))
		if _, err := o.Chat.Conversation(name); err != nil || r.PathValue("id") == "" {
			return "", Errorf(http.StatusNotFound, "no such conversation")
		}
		return name, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/chats", "The site's other conversations: the panel's and the ones its code opened (?all=1 includes those the manifest hides)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		out := []chat.Summary{}
		for _, sm := range o.Chat.List(domain) {
			if sm.Hidden && !isTrue(r.URL.Query().Get("all")) {
				continue
			}
			out = append(out, sm)
		}
		return out, nil
	})
	s.Handle("POST", Prefix+"/sites/{domain}/chats", "Open another conversation about the site (body: title?)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Title string `json:"title"`
		}
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &body); err != nil {
				return nil, err
			}
		}
		name, err := o.Chat.Create(domain, chat.CreateOptions{Origin: chat.OriginPanel, Title: body.Title})
		if err != nil {
			return nil, chatError(err)
		}
		_, id, _ := strings.Cut(name, "#")
		WriteJSON(w, http.StatusCreated, map[string]string{"name": name, "id": id})
		return Handled, nil
	})
}

func (s *Server) registerChat(m *chat.Manager, p string, name func(*http.Request) (string, error)) {
	s.Handle("GET", p, "The conversation so far", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		c, err := m.History(domain)
		if err != nil {
			return nil, chatError(err)
		}
		return c, nil
	})
	s.Handle("POST", p, "Send a message; progress arrives on the event stream as agent.*", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Text  string `json:"text"`
			Model string `json:"model"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if c, err := m.Conversation(domain); err == nil && c.Origin == chat.OriginSite {
			return nil, Errorf(http.StatusConflict, "this conversation belongs to the site's code; open one of your own to talk about the site")
		}
		id, err := m.SendWith(domain, body.Text, chat.SendOptions{Model: body.Model, Queue: true})
		if errors.Is(err, chat.ErrQueued) {
			WriteJSON(w, http.StatusAccepted, map[string]any{"queued": true})
			return Handled, nil
		}
		if err != nil {
			return nil, chatError(err)
		}
		return map[string]string{"turn_id": id}, nil
	})
	s.Handle("POST", p+"/stop", "Stop the running turn", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		if err := m.Stop(domain); err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return OK, nil
	})
	s.Handle("DELETE", p, "Forget the conversation; the next message starts a new one", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		if err := m.Reset(domain); err != nil {
			return nil, chatError(err)
		}
		return OK, nil
	})
	s.Handle("GET", p+"/processes", "What the conversation has running now, also what escaped its turn with setsid", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		procs := m.Processes(domain)
		if procs == nil {
			procs = []chat.Proc{}
		}
		return procs, nil
	})
	s.Handle("POST", p+"/reap", "Stop the turn and kill everything the conversation left running", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		return map[string]int{"killed": m.Reap(domain)}, nil
	})
	s.Handle("POST", p+"/processes/{pid}/kill", "Kill one process the conversation left; any other pid is refused", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := name(r)
		if err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(r.PathValue("pid"))
		if err != nil || pid <= 1 {
			return nil, Errorf(http.StatusBadRequest, "pid: not a process id")
		}
		if !m.Kill(domain, pid) {
			return nil, Errorf(http.StatusNotFound, "%d is not a process of this conversation", pid)
		}
		return OK, nil
	})
}

// chatError maps the manager's errors: a busy conversation is 409, a
// full house 429, an unknown site 404, an empty message or an unknown
// provider 400.
func chatError(err error) error {
	msg := err.Error()
	var capacity *chat.ErrCapacity
	switch {
	case errors.Is(err, chat.ErrBusy):
		return Errorf(http.StatusConflict, "%s", msg)
	case errors.As(err, &capacity):
		return Errorf(http.StatusTooManyRequests, "%s", msg)
	case errors.Is(err, agent.ErrUnknownProvider):
		return Errorf(http.StatusBadRequest, "%s", msg)
	case strings.HasSuffix(msg, "no such site"), strings.HasSuffix(msg, "is not a domain"):
		return Errorf(http.StatusNotFound, "%s", msg)
	case msg == "empty message":
		return Errorf(http.StatusBadRequest, "%s", msg)
	}
	return err
}

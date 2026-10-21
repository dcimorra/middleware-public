package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/auth"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/nodes"
)

// nodeTokenName names the token minted for a linked node, so the tokens
// list says what each one is for.
const nodeTokenName = "node: "

// RegisterNodes mounts the links between installations: who this node is,
// the others it is linked to, the invitation that links two, a summary of
// each for the dashboard and the door through which the panel of one
// works on another.
func (s *Server) RegisterNodes(st *nodes.Store) {
	s.mu.Lock()
	s.nodes = st
	s.mu.Unlock()
	s.Handle("GET", Prefix+"/nodes", "This node and the nodes linked to it (never their tokens)", s.nodesList)
	s.Handle("PUT", Prefix+"/nodes/self", "Rename this node", s.nodesRename)
	s.Handle("POST", Prefix+"/nodes/invite", "Make a single-use code, valid fifteen minutes, for another node to link with this one", s.nodesInvite)
	s.Handle("POST", Prefix+"/nodes", "Link with the node that made an invitation code", s.nodesLink)
	s.Handle("POST", Prefix+"/nodes/join", "The other half of a link: a node presents an invitation and its token, and gets one back", s.nodesJoin)
	s.Public("POST", Prefix+"/nodes/join")
	s.Handle("POST", Prefix+"/nodes/leave", "A linked node says it unlinked: its token here is revoked", s.nodesLeave)
	s.Handle("DELETE", Prefix+"/nodes/{id}", "Unlink a node, on both sides when it can be reached", s.nodesUnlink)
	s.Handle("GET", Prefix+"/nodes/overview", "Every node's summary, this one first, asked in parallel; an unreachable node says why", s.nodesOverview)
	s.Handle("GET", Prefix+"/node/summary", "What the dashboard shows of this node: machine, sites, conversations, recent trouble", s.nodeSummary)
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		s.Handle(m, Prefix+"/nodes/{id}/api/{path...}", "The API of a linked node, through this one: what the panel uses to work on another machine", s.nodesProxy)
	}
}

func (s *Server) nodeStore() (*nodes.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodes == nil {
		return nil, Errorf(http.StatusNotImplemented, "nodes are not available")
	}
	return s.nodes, nil
}

// fromNode is the linked node a caller is, if it is one.
func (s *Server) fromNode(c auth.Caller) (nodes.Peer, bool) {
	if c.Via != auth.ViaToken {
		return nodes.Peer{}, false
	}
	s.mu.Lock()
	st := s.nodes
	s.mu.Unlock()
	if st == nil {
		return nodes.Peer{}, false
	}
	return st.ByToken(c.TokenID)
}

// nodeMay is what a linked node may ask of this one: everything an
// operator can, except the links themselves and the operator's
// credentials. A node that can link others, mint tokens or add a passkey
// could hand this machine to a third without anyone here deciding it,
// and a chain of proxies would hide who is at the far end.
func nodeMay(pattern string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	switch pattern {
	case "POST " + Prefix + "/nodes/leave", "GET " + Prefix + "/auth/me":
		return true
	}
	return !strings.HasPrefix(path, Prefix+"/nodes") && !strings.HasPrefix(path, Prefix+"/auth/")
}

type nodeView struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	URL   string    `json:"url,omitempty"`
	Added time.Time `json:"added,omitzero"`
	Self  bool      `json:"self,omitempty"`
}

func (s *Server) nodesList(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	self := st.Self()
	out := map[string]any{"self": nodeView{ID: self.ID, Name: self.Name, Self: true}}
	peers := []nodeView{}
	for _, p := range st.Peers() {
		peers = append(peers, nodeView{ID: p.ID, Name: p.Name, URL: p.URL, Added: p.Added})
	}
	out["peers"] = peers
	return out, nil
}

func (s *Server) nodesRename(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	if err := st.SetName(body.Name); err != nil {
		return nil, Errorf(http.StatusBadRequest, "%v", err)
	}
	return st.Self(), nil
}

func (s *Server) nodesInvite(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	var body struct {
		// URL is where the other node reaches this one; the panel sends
		// the address it is open at, which is right unless the other
		// machine must come in another way (the VPN's address).
		URL string `json:"url"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	code, exp, err := st.NewInvite(nodes.BaseURL(body.URL))
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "%v", err)
	}
	s.opts.Bus.Emit(events.Event{Type: "node.invited", Actor: "operator", Message: "an invitation to link a node was made", Data: map[string]any{"expires": exp}})
	return map[string]any{"code": code, "expires": exp}, nil
}

// joinRequest is what the linking node sends to the inviting one.
type joinRequest struct {
	Secret string `json:"secret"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Token  string `json:"token"`
}

type joinAnswer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// nodesLink is the half of a link done where the code is pasted: mint the
// token the other node will use here, present it with the code, and keep
// the one that comes back. Whatever fails after the token is minted
// revokes it, so a failed attempt leaves no key behind.
func (s *Server) nodesLink(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	as, err := s.authStore()
	if err != nil {
		return nil, err
	}
	var body struct {
		Code string `json:"code"`
		// URL is where the other node reaches this one.
		URL string `json:"url"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	inv, err := st.Decode(body.Code)
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "%v", err)
	}
	selfURL := nodes.BaseURL(body.URL)
	if err := nodes.CheckURL(selfURL); err != nil {
		return nil, Errorf(http.StatusBadRequest, "%v", err)
	}
	pub, secret, err := as.CreateToken(nodeTokenName + inv.Name)
	if err != nil {
		return nil, err
	}
	self := st.Self()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var ans joinAnswer
	err = nodes.Call(ctx, inv.URL, "", "POST", "/nodes/join", joinRequest{Secret: inv.Secret, ID: self.ID, Name: self.Name, URL: selfURL, Token: secret}, &ans)
	if err == nil && (ans.Token == "" || ans.ID != inv.ID) {
		err = errors.New("the other node answered, but not as the one that made the code")
	}
	if err != nil {
		_ = as.DeleteToken(pub.ID)
		return nil, Errorf(http.StatusBadGateway, "linking with %s: %v", inv.URL, err)
	}
	old, had, err := st.Add(nodes.Peer{ID: ans.ID, Name: ans.Name, URL: inv.URL, Token: ans.Token, TokenID: pub.ID})
	if err != nil {
		_ = as.DeleteToken(pub.ID)
		return nil, err
	}
	if had {
		_ = as.DeleteToken(old.TokenID)
	}
	s.opts.Bus.Emit(events.Event{Type: "node.linked", Actor: "operator", Subject: ans.Name, Message: "linked with node " + ans.Name + " (" + inv.URL + ")"})
	return nodeView{ID: ans.ID, Name: ans.Name, URL: inv.URL}, nil
}

func (s *Server) nodesJoin(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	as, err := s.authStore()
	if err != nil {
		return nil, err
	}
	var body joinRequest
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	// The secret first, before anything about the request is believed:
	// without it this route is a 401 to anyone and says nothing else.
	if body.Secret == "" || !st.Spend(body.Secret) {
		return nil, Errorf(http.StatusUnauthorized, "%v", nodes.ErrBadInvite)
	}
	if body.ID == "" || body.ID == st.Self().ID || body.Token == "" {
		return nil, Errorf(http.StatusBadRequest, "the joining node did not say who it is")
	}
	url := nodes.BaseURL(body.URL)
	pub, secret, err := as.CreateToken(nodeTokenName + body.Name)
	if err != nil {
		return nil, err
	}
	old, had, err := st.Add(nodes.Peer{ID: body.ID, Name: body.Name, URL: url, Token: body.Token, TokenID: pub.ID})
	if err != nil {
		_ = as.DeleteToken(pub.ID)
		return nil, Errorf(http.StatusBadRequest, "%v", err)
	}
	if had {
		_ = as.DeleteToken(old.TokenID)
	}
	self := st.Self()
	s.opts.Bus.Emit(events.Event{Type: "node.linked", Actor: "node", Subject: body.Name, Message: "node " + body.Name + " (" + url + ") linked with an invitation"})
	return joinAnswer{ID: self.ID, Name: self.Name, Token: secret}, nil
}

func (s *Server) nodesLeave(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	p, ok := s.fromNode(auth.FromContext(r.Context()))
	if !ok {
		return nil, Errorf(http.StatusForbidden, "only a linked node can leave")
	}
	if _, err := st.Remove(p.ID); err != nil {
		return nil, err
	}
	if as, err := s.authStore(); err == nil {
		_ = as.DeleteToken(p.TokenID)
	}
	s.opts.Bus.Emit(events.Event{Type: "node.unlinked", Actor: "node", Subject: p.Name, Message: "node " + p.Name + " unlinked itself"})
	return OK, nil
}

func (s *Server) nodesUnlink(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	p, ok := st.Peer(r.PathValue("id"))
	if !ok {
		return nil, nodes.ErrNotFound
	}
	// The other side first, while its token still works here and there;
	// unreachable is no reason to keep the link: its token here is
	// revoked either way, which is what closes this machine to it.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	remote := nodes.Call(ctx, p.URL, p.Token, "POST", "/nodes/leave", map[string]any{}, nil)
	if _, err := st.Remove(p.ID); err != nil {
		return nil, err
	}
	if as, err := s.authStore(); err == nil {
		_ = as.DeleteToken(p.TokenID)
	}
	out := map[string]any{"ok": true}
	msg := "unlinked node " + p.Name
	if remote != nil {
		out["remote_error"] = remote.Error()
		msg += " (the other side could not be told: " + remote.Error() + ")"
	}
	s.opts.Bus.Emit(events.Event{Type: "node.unlinked", Actor: "operator", Subject: p.Name, Message: msg})
	return out, nil
}

func (s *Server) nodesProxy(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	p, ok := st.Peer(r.PathValue("id"))
	if !ok {
		return nil, nodes.ErrNotFound
	}
	h, err := nodes.Proxy(p, r.PathValue("path"))
	if err != nil {
		return nil, err
	}
	h.ServeHTTP(w, r)
	return Handled, nil
}

// Summarizer adds a part to the node summary under its key.
type Summarizer func() any

// AddSummary registers a part of GET /node/summary. The packages that know
// a thing (the vitals, the conversations, the updater) put it there; the
// summary itself knows none of them.
func (s *Server) AddSummary(key string, fn Summarizer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.summaries == nil {
		s.summaries = map[string]Summarizer{}
	}
	s.summaries[key] = fn
}

// SiteCount is the sites part of a summary.
type SiteCount struct {
	Sites    int `json:"sites"`
	Projects int `json:"projects"`
	Serving  int `json:"serving"`
	Stopped  int `json:"stopped"`
	// Trouble is every site that should answer and does not, by name: the
	// one list the dashboard exists to show.
	Trouble []SiteTrouble `json:"trouble"`
}

// SiteTrouble is one site in trouble.
type SiteTrouble struct {
	Domain string `json:"domain"`
	Why    string `json:"why"`
}

func (s *Server) nodeSummary(w http.ResponseWriter, r *http.Request) (any, error) {
	out := map[string]any{
		"version":  s.opts.Version,
		"started":  s.opts.Started,
		"hostname": hostname(),
	}
	if st, err := s.nodeStore(); err == nil {
		self := st.Self()
		out["id"], out["name"] = self.ID, self.Name
	}
	if s.opts.Sites != nil {
		list, err := s.opts.Sites.List()
		if err == nil {
			c := SiteCount{Trouble: []SiteTrouble{}}
			for _, site := range list {
				mode := site.Mode
				if mode == manifest.ModeProject {
					c.Projects++
					continue
				}
				c.Sites++
				switch {
				case !site.Managed:
					c.Trouble = append(c.Trouble, SiteTrouble{Domain: site.Domain, Why: "unmanaged"})
				case mode == manifest.ModeDisabled || mode == manifest.ModeHold:
					c.Stopped++
				case site.Handing:
					c.Serving++
				case site.Health == "down" && (mode == manifest.ModeRun || mode == manifest.ModeStatic):
					c.Trouble = append(c.Trouble, SiteTrouble{Domain: site.Domain, Why: "not_answering"})
				case mode == manifest.ModeRun && !site.Alive:
					c.Trouble = append(c.Trouble, SiteTrouble{Domain: site.Domain, Why: "down"})
				default:
					c.Serving++
				}
			}
			out["sites"] = c
		}
	}
	// The last warnings and errors: what went wrong lately, newest first.
	recent := s.opts.Bus.Recent()
	trouble := []events.Event{}
	for i := len(recent) - 1; i >= 0 && len(trouble) < 8; i-- {
		if e := recent[i]; e.Level == "warn" || e.Level == "error" {
			trouble = append(trouble, e)
		}
	}
	out["events"] = trouble
	s.mu.Lock()
	parts := make(map[string]Summarizer, len(s.summaries))
	for k, fn := range s.summaries {
		parts[k] = fn
	}
	s.mu.Unlock()
	for k, fn := range parts {
		out[k] = fn()
	}
	return out, nil
}

// NodeCard is one node in the overview.
type NodeCard struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	URL       string         `json:"url,omitempty"`
	Self      bool           `json:"self,omitempty"`
	OK        bool           `json:"ok"`
	Error     string         `json:"error,omitempty"`
	LatencyMS int64          `json:"latency_ms,omitempty"`
	Summary   map[string]any `json:"summary,omitempty"`
}

func (s *Server) nodesOverview(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.nodeStore()
	if err != nil {
		return nil, err
	}
	self := st.Self()
	peers := st.Peers()
	cards := make([]NodeCard, len(peers)+1)
	mine, _ := s.nodeSummary(w, r)
	cards[0] = NodeCard{ID: self.ID, Name: self.Name, Self: true, OK: true, Summary: mine.(map[string]any)}
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p nodes.Peer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			c := NodeCard{ID: p.ID, Name: p.Name, URL: p.URL}
			start := time.Now()
			var sum map[string]any
			if err := nodes.Call(ctx, p.URL, p.Token, "GET", "/node/summary", nil, &sum); err != nil {
				c.Error = err.Error()
			} else {
				c.OK, c.Summary, c.LatencyMS = true, sum, time.Since(start).Milliseconds()
				if name, _ := sum["name"].(string); name != "" && name != p.Name {
					st.Rename(p.ID, name)
					c.Name = name
				}
			}
			cards[i+1] = c
		}(i, p)
	}
	wg.Wait()
	return cards, nil
}

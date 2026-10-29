package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// The users are the operator's delegates: each signs in with a name and
// a password on this node and manages the sites the operator granted,
// wherever in the network they live (delegate.go). No grant opens these
// routes, so only the operator reaches them.
func (s *Server) registerUsers() {
	s.Handle("GET", Prefix+"/users", "The delegated users and what each may do, by site", s.usersList)
	s.Handle("POST", Prefix+"/users", "Make a delegated user (body: name, password, grants: [{site, perms}]; perms of view, operate, code, agents; agents: {provider: [models]} limits their conversations, null or absent for no limit, [] for every model of that provider)", s.usersCreate)
	s.Handle("PUT", Prefix+"/users/{id}", "Change what a user may do (body: grants, agents: {provider: [models]} or null for no limit) or their password (body: password); a new password ends their sessions", s.usersUpdate)
	s.Handle("DELETE", Prefix+"/users/{id}", "Remove a user and end their sessions", s.usersDelete)
	s.Handle("PUT", Prefix+"/auth/password", "A delegated user changes their own password (body: current, password); their sessions end", s.authOwnPassword)
}

// checkGrants validates grants against names, not against the sites of
// this node: a grant may name a site that lives on another node, which
// this one learns only when the user asks for it.
func checkGrants(gs []auth.Grant) ([]auth.Grant, error) {
	out, err := auth.CheckGrants(gs, manifest.ValidName)
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "%v", err)
	}
	return out, nil
}

// checkAgents reads a user's limits on agents. A provider or a model the
// machine does not have is kept: the limit is there when it comes.
func checkAgents(raw json.RawMessage) (map[string][]string, error) {
	var limits map[string][]string
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &limits); err != nil {
			return nil, Errorf(http.StatusBadRequest, "agents: %v", err)
		}
	}
	out, err := auth.CheckAgents(limits)
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "agents: %v", err)
	}
	return out, nil
}

func userErr(err error) error {
	switch {
	case errors.Is(err, auth.ErrBadUserName), errors.Is(err, auth.ErrWeakPassword):
		return Errorf(http.StatusBadRequest, "%v", err)
	case errors.Is(err, auth.ErrUserExists):
		return Errorf(http.StatusConflict, "%v", err)
	}
	return err
}

func (s *Server) usersList(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	return st.Users()
}

func (s *Server) usersCreate(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	var body struct {
		Name     string          `json:"name"`
		Password string          `json:"password"`
		Grants   []auth.Grant    `json:"grants"`
		Agents   json.RawMessage `json:"agents"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	grants, err := checkGrants(body.Grants)
	if err != nil {
		return nil, err
	}
	agents, err := checkAgents(body.Agents)
	if err != nil {
		return nil, err
	}
	u, err := st.CreateUser(body.Name, body.Password, grants, agents)
	if err != nil {
		return nil, userErr(err)
	}
	WriteJSON(w, http.StatusCreated, u)
	return Handled, nil
}

func (s *Server) usersUpdate(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	var body struct {
		Grants *[]auth.Grant `json:"grants"`
		// Absent leaves the limits as they are; null lifts them.
		Agents   json.RawMessage `json:"agents"`
		Password string          `json:"password"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if body.Grants == nil && body.Agents == nil && body.Password == "" {
		return nil, Errorf(http.StatusBadRequest, "nothing to change: send grants, agents, password or more than one")
	}
	var agents map[string][]string
	if body.Agents != nil {
		var err error
		if agents, err = checkAgents(body.Agents); err != nil {
			return nil, err
		}
	}
	if body.Password != "" {
		if err := st.SetUserPassword(id, body.Password); err != nil {
			return nil, userErr(err)
		}
	}
	if body.Grants != nil {
		grants, err := checkGrants(*body.Grants)
		if err != nil {
			return nil, err
		}
		if _, err := st.SetUserGrants(id, grants); err != nil {
			return nil, err
		}
	}
	if body.Agents != nil {
		if _, err := st.SetUserAgents(id, agents); err != nil {
			return nil, err
		}
	}
	u, err := st.User(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": u.ID, "name": u.Name, "grants": u.Grants, "agents": u.Agents, "created": u.Created}, nil
}

func (s *Server) usersDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	if err := st.DeleteUser(r.PathValue("id")); err != nil {
		return nil, err
	}
	return OK, nil
}

// authOwnPassword is the one thing a user changes about their account.
// The operator's password is changed at the box (`middleware setup`), so
// this route is a user's only.
func (s *Server) authOwnPassword(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	c := auth.FromContext(r.Context())
	if c.UserID == "" {
		return nil, Errorf(http.StatusForbidden, "this is a delegated user's own password; the operator's is set with `middleware setup`")
	}
	var body struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	if err := st.CheckUserPassword(c.UserID, body.Current); err != nil {
		return nil, err
	}
	if err := st.SetUserPassword(c.UserID, body.Password); err != nil {
		return nil, userErr(err)
	}
	auth.ClearCookie(w, r)
	return OK, nil
}

// meOfUser is what /auth/me says of a delegate: who, and which sites
// with what. Nothing about where each site lives.
func meOfUser(c auth.Caller) map[string]any {
	sites := []auth.Grant{}
	for site, perms := range c.Grants {
		sites = append(sites, auth.Grant{Site: site, Perms: append([]string{auth.PermView}, without(perms, auth.PermView)...)})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Site < sites[j].Site })
	return map[string]any{"name": c.User, "sites": sites}
}

func without(list []string, x string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

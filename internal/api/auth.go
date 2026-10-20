package api

import (
	"github.com/dcimorra/middleware-public/internal/manifest"
	"net/http"

	"github.com/dcimorra/middleware-public/internal/auth"
)

func (s *Server) registerAuth() {
	s.Handle("POST", Prefix+"/auth/setup", "Create the operator with the one-time code printed by `middleware setup`", s.authSetup)
	s.Public("POST", Prefix+"/auth/setup")
	s.Handle("POST", Prefix+"/auth/login", "Log in with the operator password; sets the session cookie", s.authLogin)
	s.Public("POST", Prefix+"/auth/login")
	s.Handle("POST", Prefix+"/auth/logout", "End the session", s.authLogout)
	s.Handle("GET", Prefix+"/auth/me", "Who the caller is and how it was recognised", s.authMe)
	s.Public("GET", Prefix+"/auth/me")
	s.Handle("GET", Prefix+"/auth/tokens", "List API tokens", s.tokensList)
	s.Handle("POST", Prefix+"/auth/tokens", "Create an API token; the secret is shown once", s.tokensCreate)
	s.Handle("DELETE", Prefix+"/auth/tokens/{id}", "Revoke an API token", s.tokensDelete)
}

func (s *Server) authStore() (*auth.Store, error) {
	if s.opts.Auth == nil {
		return nil, Errorf(http.StatusNotImplemented, "no authentication store")
	}
	return s.opts.Auth, nil
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	var body struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	if err := st.Setup(body.Code, body.Password); err != nil {
		return nil, err
	}
	return OK, nil
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	id, err := st.Login(body.Password, auth.ClientIP(r))
	if err != nil {
		return nil, err
	}
	auth.SetCookie(w, r, id)
	return OK, nil
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	if c, err := r.Cookie(auth.CookieName); err == nil {
		if err := st.DeleteSession(c.Value); err != nil {
			return nil, err
		}
	}
	auth.ClearCookie(w, r)
	return OK, nil
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) (any, error) {
	c := auth.FromContext(r.Context())
	out := map[string]any{"authenticated": c.Authenticated(), "via": string(c.Via)}
	if c.Via == auth.ViaToken {
		out["token"] = map[string]string{"id": c.TokenID, "name": c.Token}
	}
	if s.opts.Auth != nil {
		// Tells a fresh panel whether to show the setup screen or the
		// login; harmless to anyone else.
		out["setup_pending"] = !s.opts.Auth.HasOperator()
	}
	return out, nil
}

func (s *Server) tokensList(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	return st.Tokens()
}

func (s *Server) tokensCreate(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := ReadJSON(r, &body); err != nil {
		return nil, err
	}
	if err := auth.CheckScopes(body.Scopes, manifest.ValidName); err != nil {
		return nil, Errorf(http.StatusBadRequest, "%v", err)
	}
	// A scoped token cannot mint a wider one: only the operator's (full)
	// callers reach here, because no scope opens this route.
	pub, secret, err := st.CreateScopedToken(body.Name, body.Scopes)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": pub.ID, "name": pub.Name, "scopes": pub.Scopes, "created": pub.Created, "token": secret}, nil
}

func (s *Server) tokensDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.authStore()
	if err != nil {
		return nil, err
	}
	if err := st.DeleteToken(r.PathValue("id")); err != nil {
		return nil, err
	}
	return OK, nil
}

package api

import (
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/dcimorra/middleware-public/internal/auth"
	"github.com/dcimorra/middleware-public/internal/edge"
)

// origin and rpID are where the browser is: what it writes into the
// client data and the name a passkey is bound to. Both come from the
// request; a forged Host gets nothing, because the browser signs the
// origin it is really on.
func origin(r *http.Request) string {
	scheme := "http"
	if edge.HTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func rpID(r *http.Request) string {
	h := r.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

var b64u = base64.RawURLEncoding

func (s *Server) registerPasskeys() {
	p := Prefix + "/auth/passkeys"
	s.Handle("GET", p, "The registered passkeys", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		return st.Passkeys()
	})
	s.Handle("POST", p+"/register/options", "Begin registering a passkey for this panel's name", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		return st.RegisterOptions(rpID(r))
	})
	s.Handle("POST", p+"/register", "Finish registering a passkey", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		var body struct {
			ChallengeID       string `json:"challenge_id"`
			Name              string `json:"name"`
			ClientDataJSON    string `json:"client_data_json"`
			AttestationObject string `json:"attestation_object"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		cd, err1 := b64u.DecodeString(body.ClientDataJSON)
		ao, err2 := b64u.DecodeString(body.AttestationObject)
		if err1 != nil || err2 != nil {
			return nil, Errorf(http.StatusBadRequest, "client_data_json and attestation_object are base64url")
		}
		return st.Register(body.ChallengeID, body.Name, origin(r), rpID(r), cd, ao)
	})
	s.Handle("DELETE", p+"/{id}", "Remove a passkey", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		return OK, st.DeletePasskey(r.PathValue("id"))
	})
	s.Handle("POST", p+"/login/options", "Begin signing in with a passkey", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		return st.LoginOptions(rpID(r), auth.ClientIP(r))
	})
	s.Public("POST", p+"/login/options")
	s.Handle("POST", p+"/login", "Sign in with a passkey; sets the session cookie", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		var body struct {
			ChallengeID       string `json:"challenge_id"`
			ID                string `json:"id"`
			ClientDataJSON    string `json:"client_data_json"`
			AuthenticatorData string `json:"authenticator_data"`
			Signature         string `json:"signature"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		cd, e1 := b64u.DecodeString(body.ClientDataJSON)
		ad, e2 := b64u.DecodeString(body.AuthenticatorData)
		sig, e3 := b64u.DecodeString(body.Signature)
		if e1 != nil || e2 != nil || e3 != nil {
			return nil, Errorf(http.StatusBadRequest, "client_data_json, authenticator_data and signature are base64url")
		}
		id, err := st.LoginPasskey(body.ChallengeID, body.ID, origin(r), rpID(r), auth.ClientIP(r), cd, ad, sig)
		if err != nil {
			return nil, err
		}
		auth.SetCookie(w, r, id)
		return OK, nil
	})
	s.Public("POST", p+"/login")

	a := Prefix + "/auth/panel-access"
	s.Handle("GET", a, "Who may reach the panel, the caller's address and any emergency opening", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		out := map[string]any{"rules": st.PanelAccess(), "from_machine": auth.FromMachine(r)}
		if ip, ok := auth.CallerAddr(r); ok {
			out["your_address"] = ip.String()
		}
		if u := st.OpenUntil(); !u.IsZero() {
			out["open_until"] = u
		}
		return out, nil
	})
	s.Handle("PUT", a, "Replace the panel's address lists and the passkey-only switch", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		var body auth.PanelAccess
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		ip, _ := auth.CallerAddr(r)
		if err := st.SetPanelAccess(body, ip, auth.FromMachine(r)); err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return st.PanelAccess(), nil
	})
	s.Handle("DELETE", Prefix+"/auth/panel-open", "End an emergency opening early", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.authStore()
		if err != nil {
			return nil, err
		}
		return OK, st.ClosePanel()
	})
}

// sameSite refuses a cookie request whose browser says it came from
// another origin. X-Requested-With already stops a cross-site form, which
// cannot set it; this is the second wall, and the one V1 learned it needed:
// SameSite separates sites, and every *.example.com is one site to the
// browser, so a page on a sibling subdomain sends the panel's cookie.
func sameSite(r *http.Request) error {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		if sfs != "same-origin" && sfs != "none" {
			return errors.New("a request from another origin (" + sfs + ")")
		}
		return nil
	}
	if o := r.Header.Get("Origin"); o != "" && o != origin(r) {
		return errors.New("a request from another origin (" + o + ")")
	}
	return nil
}

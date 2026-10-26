package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/brake"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/webauthn"
)

// Passkeys are the operator's second way in, and the spare key that
// survives a password change: changing the password ends every session,
// it does not touch the passkeys. Each one is bound to the name it was
// made on (the panel's domain), so one made on the panel's domain does not
// work through 127.0.0.1, and none works over a bare IP: browsers only
// offer them on a secure origin with a name. That is not a defect, it is
// what keeps a copy of the panel at another address from asking for a
// signature that is valid here.

// Passkey is one registered passkey.
type Passkey struct {
	webauthn.Credential
	Name     string    `json:"name"`
	RPID     string    `json:"rp_id"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`
}

// PasskeyInfo is a passkey as listed: no key material.
type PasskeyInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	RPID     string    `json:"rp_id"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`
}

var b64url = base64.RawURLEncoding

// challengeLife is how long a begun ceremony may be finished. Challenges
// live in memory and are spent on first use: a supervisor restart in the
// middle of the dialog is fixed by pressing the button again.
const challengeLife = 5 * time.Minute

type challenge struct {
	value   []byte
	purpose string // "register" or "login"
	rpID    string
	expires time.Time
	// from is the address (brake.KeyOf) a login ceremony was begun from;
	// empty for a registration, which only a session can begin.
	from string
}

// The ceremonies in flight are bounded, and the login's begins without a
// session. The bound used to refuse every new one once 256 were waiting:
// 257 anonymous requests every five minutes then kept the operator out of
// the panel, entirely when the password is closed. Now one address keeps
// a few and a full table drops the oldest login, so filling it takes a
// flood that outruns the operator's own ceremony, and the WAF sees that.
const (
	maxChallenges        = 256
	maxChallengesPerAddr = 4
)

// Errors the API maps.
var (
	ErrNoPasskey      = errors.New("no such passkey")
	ErrPasskeyFailed  = errors.New("the passkey was not accepted")
	ErrLastPasskey    = errors.New("this is the last passkey and the password is closed: open the password first")
	ErrPasskeyOnly    = errors.New("the password is closed on this panel: sign in with a passkey")
	ErrNeedsAPasskey  = errors.New("closing the password needs a passkey registered first")
	ErrBadPasskeyName = errors.New("a passkey needs a name")
)

func (s *Store) passkeysPath() string { return s.dir + "/passkeys.json" }

func (s *Store) loadPasskeys() ([]Passkey, error) {
	var list []Passkey
	if err := fsx.ReadJSON(s.passkeysPath(), &list); err != nil && !fsx.IsNotExist(err) {
		return nil, err
	}
	return list, nil
}

func (s *Store) savePasskeys(list []Passkey) error {
	if list == nil {
		list = []Passkey{}
	}
	return fsx.WriteJSON(s.passkeysPath(), list, 0o600)
}

// Passkeys lists the registered passkeys.
func (s *Store) Passkeys() ([]PasskeyInfo, error) {
	s.pkMu.Lock()
	defer s.pkMu.Unlock()
	list, err := s.loadPasskeys()
	if err != nil {
		return nil, err
	}
	out := make([]PasskeyInfo, 0, len(list))
	for _, p := range list {
		out = append(out, PasskeyInfo{ID: b64url.EncodeToString(p.ID), Name: p.Name, RPID: p.RPID, Created: p.Created, LastUsed: p.LastUsed})
	}
	return out, nil
}

// HasPasskeys says whether any passkey is registered for rpID ("" is any).
func (s *Store) HasPasskeys(rpID string) bool {
	list, _ := s.Passkeys()
	for _, p := range list {
		if rpID == "" || p.RPID == rpID {
			return true
		}
	}
	return false
}

func (s *Store) newChallenge(purpose, rpID, from string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := b64url.EncodeToString(b)
	s.pkMu.Lock()
	defer s.pkMu.Unlock()
	now := s.now()
	mine := 0
	for k, c := range s.challenges {
		if now.After(c.expires) {
			delete(s.challenges, k)
			continue
		}
		if from != "" && c.from == from {
			mine++
		}
	}
	if from != "" && mine >= maxChallengesPerAddr {
		s.dropOldestLogin(from)
	}
	if len(s.challenges) >= maxChallenges {
		s.dropOldestLogin("")
	}
	if len(s.challenges) >= maxChallenges {
		// Only registrations, which need a session, are left.
		return "", ErrRateLimited
	}
	s.challenges[id] = challenge{value: b, purpose: purpose, rpID: rpID, expires: now.Add(challengeLife), from: from}
	return id, nil
}

// dropOldestLogin forgets the login ceremony that expires first, begun
// from the address from, or from any when from is empty. pkMu is held.
func (s *Store) dropOldestLogin(from string) {
	oldest, at := "", time.Time{}
	for k, c := range s.challenges {
		if c.purpose != "login" || (from != "" && c.from != from) {
			continue
		}
		if oldest == "" || c.expires.Before(at) {
			oldest, at = k, c.expires
		}
	}
	if oldest != "" {
		delete(s.challenges, oldest)
	}
}

// spend takes a challenge out, so it is used once whatever happens next.
func (s *Store) spend(id, purpose, rpID string) ([]byte, bool) {
	s.pkMu.Lock()
	defer s.pkMu.Unlock()
	c, ok := s.challenges[id]
	delete(s.challenges, id)
	if !ok || c.purpose != purpose || c.rpID != rpID || s.now().After(c.expires) {
		return nil, false
	}
	return c.value, true
}

// RegisterOptions are what navigator.credentials.create() needs, in the
// JSON the panel turns into its arguments.
func (s *Store) RegisterOptions(rpID string) (map[string]any, error) {
	ch, err := s.newChallenge("register", rpID, "")
	if err != nil {
		return nil, err
	}
	list, err := s.loadPasskeysLocked()
	if err != nil {
		return nil, err
	}
	var exclude []map[string]string
	for _, p := range list {
		if p.RPID == rpID {
			exclude = append(exclude, map[string]string{"type": "public-key", "id": b64url.EncodeToString(p.ID)})
		}
	}
	var params []map[string]any
	for _, a := range webauthn.Algorithms {
		params = append(params, map[string]any{"type": "public-key", "alg": a})
	}
	return map[string]any{
		"challenge_id": ch,
		"publicKey": map[string]any{
			"challenge":          ch,
			"rp":                 map[string]string{"id": rpID, "name": "middleware"},
			"user":               map[string]string{"id": b64url.EncodeToString([]byte("operator")), "name": "operator", "displayName": "middleware operator"},
			"pubKeyCredParams":   params,
			"excludeCredentials": exclude,
			"authenticatorSelection": map[string]any{
				"residentKey": "preferred", "userVerification": "preferred",
			},
			"attestation": "none",
			"timeout":     int(challengeLife / time.Millisecond),
		},
	}, nil
}

func (s *Store) loadPasskeysLocked() ([]Passkey, error) {
	s.pkMu.Lock()
	defer s.pkMu.Unlock()
	return s.loadPasskeys()
}

// Register keeps a passkey made with RegisterOptions.
func (s *Store) Register(challengeID, name, origin, rpID string, clientDataJSON, attestationObject []byte) (PasskeyInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return PasskeyInfo{}, ErrBadPasskeyName
	}
	ch, ok := s.spend(challengeID, "register", rpID)
	if !ok {
		return PasskeyInfo{}, ErrPasskeyFailed
	}
	cred, err := webauthn.Register(clientDataJSON, attestationObject, webauthn.Expect{Challenge: ch, Origin: origin, RPID: rpID})
	if err != nil {
		s.emit("auth.passkey_refused", "passkey registration refused: "+err.Error(), nil)
		return PasskeyInfo{}, ErrPasskeyFailed
	}
	s.pkMu.Lock()
	defer s.pkMu.Unlock()
	list, err := s.loadPasskeys()
	if err != nil {
		return PasskeyInfo{}, err
	}
	p := Passkey{Credential: cred, Name: name, RPID: rpID, Created: s.now()}
	list = append(list, p)
	if err := s.savePasskeys(list); err != nil {
		return PasskeyInfo{}, err
	}
	s.emit("auth.passkey_added", "passkey "+name+" added", map[string]any{"rp_id": rpID})
	return PasskeyInfo{ID: b64url.EncodeToString(p.ID), Name: p.Name, RPID: rpID, Created: p.Created}, nil
}

// LoginOptions are what navigator.credentials.get() needs; ip is the
// caller's address, which bounds how many it may have waiting.
func (s *Store) LoginOptions(rpID, ip string) (map[string]any, error) {
	ch, err := s.newChallenge("login", rpID, "login\x00"+brake.KeyOf(ip))
	if err != nil {
		return nil, err
	}
	list, err := s.loadPasskeysLocked()
	if err != nil {
		return nil, err
	}
	var allow []map[string]string
	for _, p := range list {
		if p.RPID == rpID {
			allow = append(allow, map[string]string{"type": "public-key", "id": b64url.EncodeToString(p.ID)})
		}
	}
	return map[string]any{
		"challenge_id": ch,
		"publicKey": map[string]any{
			"challenge": ch, "rpId": rpID, "allowCredentials": allow,
			"userVerification": "preferred", "timeout": int(challengeLife / time.Millisecond),
		},
	}, nil
}

// LoginPasskey checks a passkey's answer and opens a session. Failures
// count against the address like a wrong password: a signature costs
// little to check, but the brake is the one door for guessing.
func (s *Store) LoginPasskey(challengeID, credentialID, origin, rpID, ip string, clientDataJSON, authenticatorData, signature []byte) (string, error) {
	key := brake.KeyOf(ip)
	if blocked, _ := s.limiter.Blocked(key); blocked {
		return "", ErrRateLimited
	}
	fail := func(why string) (string, error) {
		s.limiter.Failed(key)
		s.emit("auth.login_failed", "passkey refused: "+why, map[string]any{"ip": ip})
		return "", ErrPasskeyFailed
	}
	ch, ok := s.spend(challengeID, "login", rpID)
	if !ok {
		return fail("the challenge is unknown or used")
	}
	id, err := b64url.DecodeString(credentialID)
	if err != nil {
		return fail("a malformed credential id")
	}
	s.pkMu.Lock()
	list, err := s.loadPasskeys()
	if err != nil {
		s.pkMu.Unlock()
		return "", err
	}
	idx := -1
	for i, p := range list {
		if p.RPID == rpID && string(p.ID) == string(id) {
			idx = i
		}
	}
	if idx < 0 {
		s.pkMu.Unlock()
		return fail("a passkey this panel does not know")
	}
	count, err := webauthn.Login(list[idx].Credential, clientDataJSON, authenticatorData, signature, webauthn.Expect{Challenge: ch, Origin: origin, RPID: rpID})
	if err != nil {
		s.pkMu.Unlock()
		return fail(err.Error())
	}
	list[idx].SignCount, list[idx].LastUsed = count, s.now()
	err = s.savePasskeys(list)
	name := list[idx].Name
	s.pkMu.Unlock()
	if err != nil {
		return "", err
	}
	s.limiter.Passed(key)
	sid, err := s.NewSession()
	if err != nil {
		return "", err
	}
	s.emit("auth.login", "operator logged in with passkey "+name, map[string]any{"ip": ip})
	return sid, nil
}

// DeletePasskey removes one. The last one cannot go while the password
// is closed, or nobody could get in again but from the shell.
func (s *Store) DeletePasskey(id string) error {
	if s.PanelAccess().PasskeyOnly {
		if list, _ := s.Passkeys(); len(list) <= 1 {
			return ErrLastPasskey
		}
	}
	raw, err := b64url.DecodeString(id)
	if err != nil {
		return ErrNoPasskey
	}
	s.pkMu.Lock()
	defer s.pkMu.Unlock()
	list, err := s.loadPasskeys()
	if err != nil {
		return err
	}
	out := list[:0]
	found := ""
	for _, p := range list {
		if string(p.ID) == string(raw) {
			found = p.Name
			continue
		}
		out = append(out, p)
	}
	if found == "" {
		return ErrNoPasskey
	}
	if err := s.savePasskeys(out); err != nil {
		return err
	}
	s.emit("auth.passkey_removed", "passkey "+found+" removed", nil)
	return nil
}

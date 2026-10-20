package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/brake"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
)

// SessionTTL is how long a cookie session lives. Thirty days matches what
// the panel's users expect from a tool they open every few days; the cookie
// carries the same MaxAge so the browser forgets it when the file does.
const SessionTTL = 30 * 24 * time.Hour

// Errors a caller may branch on.
var (
	ErrNoOperator      = errors.New("no operator password set: run `middleware setup`")
	ErrBadCredentials  = errors.New("wrong password")
	ErrBadSetupCode    = errors.New("wrong or expired setup code")
	ErrRateLimited     = errors.New("too many failed logins, try again later")
	ErrTokenNameNeeded = errors.New("token: a name is required")
)

// Store is data/auth/: the password, the one-time setup code, the sessions
// and the tokens. Every write is atomic through fsx, so a crash mid-write
// cannot lock the operator out or let a half-written token in.
type Store struct {
	dir string
	bus *events.Bus
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]Session
	tokens   []Token
	loaded   bool
	limiter  *brake.Brake
}

// Session is one cookie session.
type Session struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

// Token is an API token as listed; the secret itself is never stored,
// only its SHA-256, so a copy of the file cannot be replayed.
type Token struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`
}

// Public is a token as shown to the operator: everything but the hash.
type Public struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`
}

// loginPolicy: five failures inside a minute lock the address out for a
// minute, and every further lock doubles. Five per minute lets a person
// mistype a few times and makes an online guess of an 8-character
// password take centuries.
var loginPolicy = brake.Policy{Failures: 5, Window: time.Minute, MaxLock: 24 * time.Hour}

// New opens the store under root/data/auth. bus may be nil for tests.
func New(root string, bus *events.Bus) *Store {
	if bus == nil {
		bus = events.New("")
	}
	return &Store{dir: filepath.Join(root, "data", "auth"), bus: bus, now: time.Now, limiter: brake.New(loginPolicy)}
}

func (s *Store) passwordPath() string  { return filepath.Join(s.dir, "password") }
func (s *Store) setupCodePath() string { return filepath.Join(s.dir, "setup-code") }
func (s *Store) sessionsPath() string  { return filepath.Join(s.dir, "sessions.json") }
func (s *Store) tokensPath() string    { return filepath.Join(s.dir, "tokens.json") }

func (s *Store) emit(typ, msg string, data map[string]any) {
	s.bus.Emit(events.Event{Type: typ, Actor: "operator", Message: msg, Data: data})
}

// HasOperator reports whether a password has been set.
func (s *Store) HasOperator() bool {
	v, err := fsx.ReadTrimmed(s.passwordPath())
	return err == nil && v != ""
}

// SetPassword hashes and stores the operator password. Existing sessions
// are dropped: a password change is how an operator throws out whoever
// else had a cookie.
func (s *Store) SetPassword(password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setPassword(password)
}

func (s *Store) setPassword(password string) error {
	phc, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(s.passwordPath(), []byte(phc+"\n"), 0o600); err != nil {
		return err
	}
	if err := s.load(); err != nil {
		return err
	}
	s.sessions = map[string]Session{}
	if err := s.saveSessions(); err != nil {
		return err
	}
	s.emit("auth.password_set", "operator password set", nil)
	return nil
}

// CreateSetupCode writes a fresh one-time code and returns it. The code is
// the proof that whoever calls POST /auth/setup also has a shell on the
// box, which is why only the CLI can make one.
func (s *Store) CreateSetupCode() (string, error) {
	code, err := randomToken(24)
	if err != nil {
		return "", err
	}
	if err := fsx.WriteAtomic(s.setupCodePath(), []byte(code+"\n"), 0o600); err != nil {
		return "", err
	}
	return code, nil
}

// HasSetupCode reports whether a code is waiting to be used.
func (s *Store) HasSetupCode() bool { return fsx.Exists(s.setupCodePath()) }

// Setup consumes the setup code and sets the password. The code file is
// removed before the password is written: if the write fails the operator
// runs `middleware setup` again, whereas the other order would leave a
// code that still works after it was used.
func (s *Store) Setup(code, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want, err := fsx.ReadTrimmed(s.setupCodePath())
	if err != nil || want == "" {
		return ErrBadSetupCode
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(code))) != 1 {
		return ErrBadSetupCode
	}
	if len(password) < MinPasswordLength {
		return ErrWeakPassword
	}
	if err := os.Remove(s.setupCodePath()); err != nil {
		return err
	}
	return s.setPassword(password)
}

// Login verifies the password for a client address and returns a new
// session id. Failures count against the address first, so a flood of
// wrong passwords is rejected before the hash is even computed: PBKDF2 at
// 600k rounds is exactly the kind of work an attacker wants to make us do.
func (s *Store) Login(password, ip string) (string, error) {
	if !s.HasOperator() {
		return "", ErrNoOperator
	}
	key := brake.KeyOf(ip)
	if blocked, _ := s.limiter.Blocked(key); blocked {
		s.emit("auth.rate_limited", "login rate limited", map[string]any{"ip": ip})
		return "", ErrRateLimited
	}
	phc, err := fsx.ReadTrimmed(s.passwordPath())
	if err != nil {
		return "", err
	}
	ok, ran := brake.Derive(context.Background(), func() bool { return VerifyPassword(phc, password) })
	if !ran {
		s.emit("auth.rate_limited", "login refused: every password check slot busy", map[string]any{"ip": ip})
		return "", ErrRateLimited
	}
	if !ok {
		s.limiter.Failed(key)
		s.emit("auth.login_failed", "wrong password", map[string]any{"ip": ip})
		return "", ErrBadCredentials
	}
	s.limiter.Passed(key)
	id, err := s.NewSession()
	if err != nil {
		return "", err
	}
	s.emit("auth.login", "operator logged in", map[string]any{"ip": ip})
	return id, nil
}

// NewSession creates a session without checking anything: the caller
// already did (Login, or a passkey module later).
func (s *Store) NewSession() (string, error) {
	id, err := randomToken(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return "", err
	}
	now := s.now()
	s.sessions[id] = Session{ID: id, Created: now, Expires: now.Add(SessionTTL)}
	return id, s.saveSessions()
}

// ValidSession reports whether id names a live session.
func (s *Store) ValidSession(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return false
	}
	ses, ok := s.sessions[id]
	return ok && s.now().Before(ses.Expires)
}

// DeleteSession ends a session; an unknown id is not an error.
func (s *Store) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	if _, ok := s.sessions[id]; !ok {
		return nil
	}
	delete(s.sessions, id)
	return s.saveSessions()
}

// CreateToken mints a token and returns its record and the secret, which
// is shown once and never recoverable: only its hash is kept.
func (s *Store) CreateToken(name string) (Public, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Public{}, "", ErrTokenNameNeeded
	}
	secret, err := randomToken(32)
	if err != nil {
		return Public{}, "", err
	}
	// Hex, not base64url: the id is typed after `middleware token rm`, and
	// one in 64 base64url ids began with "-" and was read as a flag (CI
	// failed on it on 20 October 2026). Ids made before stay valid and are
	// removed with `token rm -- <id>`.
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Public{}, "", err
	}
	id := hex.EncodeToString(raw[:])
	secret = "mw_" + secret
	t := Token{ID: id, Name: name, Hash: hashToken(secret), Created: s.now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Public{}, "", err
	}
	s.tokens = append(s.tokens, t)
	if err := s.saveTokens(); err != nil {
		return Public{}, "", err
	}
	s.emit("auth.token_created", "api token created", map[string]any{"id": id, "name": name})
	return t.public(), secret, nil
}

// Tokens lists tokens, oldest first.
func (s *Store) Tokens() ([]Public, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	out := make([]Public, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, t.public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// DeleteToken revokes a token by id.
func (s *Store) DeleteToken(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	for i, t := range s.tokens {
		if t.ID == id {
			s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
			if err := s.saveTokens(); err != nil {
				return err
			}
			s.emit("auth.token_deleted", "api token deleted", map[string]any{"id": id, "name": t.Name})
			return nil
		}
	}
	return ErrNotFound
}

// LookupToken resolves a bearer secret to its token id and records the
// use. last_used is written at most once a minute per token: a script
// polling every second must not turn tokens.json into a hot file.
func (s *Store) LookupToken(secret string) (Public, bool) {
	h := hashToken(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Public{}, false
	}
	for i := range s.tokens {
		t := &s.tokens[i]
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) != 1 {
			continue
		}
		now := s.now()
		if now.Sub(t.LastUsed) > time.Minute {
			t.LastUsed = now
			_ = s.saveTokens()
		}
		return t.public(), true
	}
	return Public{}, false
}

func (t Token) public() Public {
	return Public{ID: t.ID, Name: t.Name, Created: t.Created, LastUsed: t.LastUsed}
}

func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// load reads sessions and tokens once; afterwards the in-memory copy is
// the truth and every change goes through save*. Expired sessions are
// dropped on load so the file does not grow forever.
func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	s.sessions = map[string]Session{}
	var list []Session
	if err := fsx.ReadJSON(s.sessionsPath(), &list); err != nil && !fsx.IsNotExist(err) {
		return err
	}
	now := s.now()
	for _, ses := range list {
		if now.Before(ses.Expires) {
			s.sessions[ses.ID] = ses
		}
	}
	s.tokens = nil
	if err := fsx.ReadJSON(s.tokensPath(), &s.tokens); err != nil && !fsx.IsNotExist(err) {
		return err
	}
	s.loaded = true
	return nil
}

func (s *Store) saveSessions() error {
	list := make([]Session, 0, len(s.sessions))
	now := s.now()
	for _, ses := range s.sessions {
		if now.Before(ses.Expires) {
			list = append(list, ses)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	return fsx.WriteJSON(s.sessionsPath(), list, 0o600)
}

func (s *Store) saveTokens() error {
	if s.tokens == nil {
		s.tokens = []Token{}
	}
	return fsx.WriteJSON(s.tokensPath(), s.tokens, 0o600)
}

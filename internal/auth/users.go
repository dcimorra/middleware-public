package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/brake"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// A user is a delegate: someone the operator lets in to manage some sites
// (perms.go says what each grant opens). Users live on the node where the
// operator made them, which is the node they sign in at; a grant names a
// site, not a node, and the API finds the node that holds it (the site
// may move, and the user never learns where it went).
//
// The operator is not a user: the password in data/auth/password, the
// passkeys, the socket and the full tokens stay what they were, and a
// session without a user is theirs.

// User is a delegate as kept in users.json.
type User struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
	Grants  []Grant   `json:"grants"`
}

// PublicUser is a user as listed: everything but the hash.
type PublicUser struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	Grants   []Grant   `json:"grants"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// Errors of the users.
var (
	ErrBadUserName = errors.New("a user name is 2 to 32 of a-z, 0-9, '.', '_' and '-', starting with a letter or a digit")
	ErrUserExists  = errors.New("a user with that name exists")
)

var userNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

func (s *Store) usersPath() string { return filepath.Join(s.dir, "users.json") }

func (s *Store) loadUsers() ([]User, error) {
	var list []User
	if err := fsx.ReadJSON(s.usersPath(), &list); err != nil && !fsx.IsNotExist(err) {
		return nil, err
	}
	return list, nil
}

func (s *Store) saveUsers(list []User) error {
	if list == nil {
		list = []User{}
	}
	return fsx.WriteJSON(s.usersPath(), list, 0o600)
}

func (u User) public() PublicUser {
	g := u.Grants
	if g == nil {
		g = []Grant{}
	}
	return PublicUser{ID: u.ID, Name: u.Name, Created: u.Created, Grants: g}
}

// GrantMap is the grants by site, as a Caller carries them.
func (u User) GrantMap() map[string][]string {
	out := make(map[string][]string, len(u.Grants))
	for _, g := range u.Grants {
		out[g.Site] = g.Perms
	}
	return out
}

// Users lists the users by name, each with when a session of theirs was
// last made.
func (s *Store) Users() ([]PublicUser, error) {
	s.usersMu.Lock()
	list, err := s.loadUsers()
	s.usersMu.Unlock()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	_ = s.load()
	seen := map[string]time.Time{}
	for _, ses := range s.sessions {
		if ses.User != "" && ses.Created.After(seen[ses.User]) {
			seen[ses.User] = ses.Created
		}
	}
	s.mu.Unlock()
	out := make([]PublicUser, 0, len(list))
	for _, u := range list {
		p := u.public()
		p.LastSeen = seen[u.ID]
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// User is one user by id.
func (s *Store) User(id string) (User, error) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	list, err := s.loadUsers()
	if err != nil {
		return User{}, err
	}
	for _, u := range list {
		if u.ID == id {
			return u, nil
		}
	}
	return User{}, ErrNotFound
}

// CreateUser makes a user with a password and the grants given, which
// must have been checked (CheckGrants).
func (s *Store) CreateUser(name, password string, grants []Grant) (PublicUser, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !userNameRE.MatchString(name) {
		return PublicUser{}, ErrBadUserName
	}
	if len(password) < MinPasswordLength {
		return PublicUser{}, ErrWeakPassword
	}
	phc, err := HashPassword(password)
	if err != nil {
		return PublicUser{}, err
	}
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return PublicUser{}, err
	}
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	list, err := s.loadUsers()
	if err != nil {
		return PublicUser{}, err
	}
	for _, u := range list {
		if u.Name == name {
			return PublicUser{}, ErrUserExists
		}
	}
	u := User{ID: hex.EncodeToString(raw[:]), Name: name, Hash: phc, Created: s.now(), Grants: grants}
	if err := s.saveUsers(append(list, u)); err != nil {
		return PublicUser{}, err
	}
	s.emit("auth.user_created", "user "+name+" created", map[string]any{"id": u.ID, "name": name, "grants": grants})
	return u.public(), nil
}

// updateUser applies fn to the user id and saves.
func (s *Store) updateUser(id string, fn func(*User) error) (User, error) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	list, err := s.loadUsers()
	if err != nil {
		return User{}, err
	}
	for i := range list {
		if list[i].ID != id {
			continue
		}
		if err := fn(&list[i]); err != nil {
			return User{}, err
		}
		if err := s.saveUsers(list); err != nil {
			return User{}, err
		}
		return list[i], nil
	}
	return User{}, ErrNotFound
}

// SetUserGrants replaces what a user may do. It takes effect on the
// user's next request: a session carries the user, not the grants.
func (s *Store) SetUserGrants(id string, grants []Grant) (PublicUser, error) {
	u, err := s.updateUser(id, func(u *User) error {
		u.Grants = grants
		return nil
	})
	if err != nil {
		return PublicUser{}, err
	}
	s.emit("auth.user_grants", "what user "+u.Name+" may do changed", map[string]any{"id": id, "name": u.Name, "grants": grants})
	return u.public(), nil
}

// SetUserPassword sets a user's password and ends their sessions, the
// one asking included when it is the user: the next request signs in
// again, which is what a changed password is expected to do.
func (s *Store) SetUserPassword(id, password string) error {
	if len(password) < MinPasswordLength {
		return ErrWeakPassword
	}
	phc, err := HashPassword(password)
	if err != nil {
		return err
	}
	u, err := s.updateUser(id, func(u *User) error {
		u.Hash = phc
		return nil
	})
	if err != nil {
		return err
	}
	s.dropUserSessions(id)
	s.emit("auth.user_password", "user "+u.Name+"'s password changed", map[string]any{"id": id, "name": u.Name})
	return nil
}

// DeleteUser removes a user and every session of theirs.
func (s *Store) DeleteUser(id string) error {
	s.usersMu.Lock()
	list, err := s.loadUsers()
	if err != nil {
		s.usersMu.Unlock()
		return err
	}
	name := ""
	out := list[:0]
	for _, u := range list {
		if u.ID == id {
			name = u.Name
			continue
		}
		out = append(out, u)
	}
	if name == "" {
		s.usersMu.Unlock()
		return ErrNotFound
	}
	err = s.saveUsers(out)
	s.usersMu.Unlock()
	if err != nil {
		return err
	}
	s.dropUserSessions(id)
	s.emit("auth.user_deleted", "user "+name+" deleted", map[string]any{"id": id, "name": name})
	return nil
}

// ForgetSite drops a site from every grant: a site deleted here is not
// left for a site of the same name made later.
func (s *Store) ForgetSite(domain string) error { return s.RenameSite(domain, "") }

// RenameSite moves the grants of a site renamed here to its new name;
// to "" drops them.
func (s *Store) RenameSite(from, to string) error {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	list, err := s.loadUsers()
	if err != nil || len(list) == 0 {
		return err
	}
	changed := false
	for i := range list {
		kept := list[i].Grants[:0]
		for _, g := range list[i].Grants {
			if g.Site == from {
				changed = true
				if to == "" {
					continue
				}
				g.Site = to
			}
			kept = append(kept, g)
		}
		list[i].Grants = kept
	}
	if !changed {
		return nil
	}
	return s.saveUsers(list)
}

func (s *Store) dropUserSessions(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return
	}
	for k, ses := range s.sessions {
		if ses.User == id {
			delete(s.sessions, k)
		}
	}
	_ = s.saveSessions()
}

// dummyHash is checked against when the name is no user's, so a wrong
// name costs what a wrong password does and the time does not tell which
// names exist.
var dummyHash = sync.OnceValue(func() string {
	h, _ := HashPassword("not a password of anyone's")
	return h
})

// LoginUser verifies a user's password for a client address and returns
// a new session of theirs. It goes through the same brakes as the
// operator's password: per address and for the whole house, so a list of
// names is no more guesses than one.
func (s *Store) LoginUser(name, password, ip string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	key := loginKey(ip)
	if blocked, _ := s.limiter.Blocked(key); blocked {
		s.emit("auth.rate_limited", "login rate limited", map[string]any{"ip": ip, "user": name})
		return "", ErrRateLimited
	}
	if blocked, _ := s.house.Blocked(houseKey); blocked {
		s.emit("auth.rate_limited", "login rate limited for every address: too many wrong passwords in the house", map[string]any{"ip": ip, "house": true, "user": name})
		return "", ErrRateLimited
	}
	s.usersMu.Lock()
	list, err := s.loadUsers()
	s.usersMu.Unlock()
	if err != nil {
		return "", err
	}
	var who *User
	for i := range list {
		if list[i].Name == name {
			who = &list[i]
		}
	}
	phc := dummyHash()
	if who != nil {
		phc = who.Hash
	}
	ok, ran := brake.Derive(context.Background(), func() bool { return VerifyPassword(phc, password) })
	if !ran {
		s.emit("auth.rate_limited", "login refused: every password check slot busy", map[string]any{"ip": ip})
		return "", ErrRateLimited
	}
	if !ok || who == nil {
		s.limiter.Failed(key)
		s.house.Failed(houseKey)
		s.emit("auth.login_failed", "wrong user name or password", map[string]any{"ip": ip, "user": name})
		return "", ErrBadCredentials
	}
	s.limiter.Passed(key)
	id, err := s.newSession(who.ID)
	if err != nil {
		return "", err
	}
	s.emit("auth.user_login", "user "+who.Name+" logged in", map[string]any{"ip": ip, "id": who.ID, "name": who.Name})
	return id, nil
}

// sessionUser is the user a live session belongs to: ok false for no
// live session, an empty User for the operator's.
func (s *Store) sessionUser(id string) (User, bool) {
	if id == "" {
		return User{}, false
	}
	s.mu.Lock()
	if err := s.load(); err != nil {
		s.mu.Unlock()
		return User{}, false
	}
	ses, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok || !s.now().Before(ses.Expires) {
		return User{}, false
	}
	if ses.User == "" {
		return User{}, true
	}
	// Read on every request, so a grant taken away or a user deleted is
	// gone on the next click, not when the cookie expires.
	u, err := s.User(ses.User)
	if err != nil {
		return User{}, false
	}
	return u, true
}

// CheckUserPassword is whether password is user id's own, for changing it.
func (s *Store) CheckUserPassword(id, password string) error {
	u, err := s.User(id)
	if err != nil {
		return err
	}
	ok, ran := brake.Derive(context.Background(), func() bool { return VerifyPassword(u.Hash, password) })
	if !ran {
		return ErrRateLimited
	}
	if !ok {
		return fmt.Errorf("%w: the current password is not that", ErrBadCredentials)
	}
	return nil
}

// Package nodes is what lets several installations see each other: the
// other nodes this one is linked to, how to reach each, and the one-time
// invitations that create a link.
//
// A link is two ordinary API tokens, one minted on each side, so nothing
// new decides who may do what: a linked node is the operator of the other,
// exactly as a script with a full token would be, and unlinking is
// revoking that token. What this package adds is the bookkeeping (which
// token belongs to which node, so it can be revoked with the link) and
// the invitation, so no secret is ever copied from one panel to the other
// by hand: the code an operator pastes is single-use and dies in minutes.
package nodes

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// InviteTTL is how long an invitation code works. Long enough to switch
// tabs and paste it, short enough that one left in a chat log is dead.
const InviteTTL = 15 * time.Minute

// Errors the API maps to statuses.
var (
	ErrNotFound   = errors.New("no such node")
	ErrBadInvite  = errors.New("the invitation code is not valid or has expired")
	ErrBadURL     = errors.New("a node's address must be https://, or http:// to a private or VPN address")
	ErrSelf       = errors.New("that code was made by this same node")
	ErrBadName    = errors.New("a node's name is 1 to 40 characters")
	ErrDuplicated = errors.New("this node is already linked")
)

// Self is this installation as the others know it. The id never changes,
// so a node renamed or moved to another address is still the same link.
type Self struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Peer is a linked node.
type Peer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// URL is the peer's panel, scheme and host: where its API answers.
	URL string `json:"url"`
	// Token is what this node sends to the peer. It is the one secret in
	// the file, which is why the file is 0600.
	Token string `json:"token"`
	// TokenID is the token this node minted for the peer: revoked when the
	// link goes, and how a request is recognised as coming from it.
	TokenID string    `json:"token_id"`
	Added   time.Time `json:"added"`
}

// Invite is what a code carries.
type Invite struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

// Store keeps self and the peers under data/nodes.
type Store struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	self    Self
	peers   []Peer
	invites map[string]time.Time // sha256 of the secret -> expiry
}

// Open reads the store, creating this node's identity the first time.
// name is the default name, usually the hostname.
func Open(dir, name string) (*Store, error) {
	s := &Store{dir: dir, now: time.Now, invites: map[string]time.Time{}}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := fsx.ReadJSON(s.selfPath(), &s.self); err != nil && !fsx.IsNotExist(err) {
		return nil, err
	}
	if s.self.ID == "" {
		s.self.ID = random(8)
		s.self.Name = cleanName(name)
		if s.self.Name == "" {
			s.self.Name = "node-" + s.self.ID[:6]
		}
		if err := fsx.WriteJSON(s.selfPath(), s.self, 0o600); err != nil {
			return nil, err
		}
	}
	if err := fsx.ReadJSON(s.peersPath(), &s.peers); err != nil && !fsx.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) selfPath() string  { return filepath.Join(s.dir, "self.json") }
func (s *Store) peersPath() string { return filepath.Join(s.dir, "peers.json") }

// Self is this node.
func (s *Store) Self() Self {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.self
}

// SetName renames this node. The peers learn it the next time they ask
// for its summary; the link does not depend on it.
func (s *Store) SetName(name string) error {
	name = cleanName(name)
	if name == "" {
		return ErrBadName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.self
	next.Name = name
	if err := fsx.WriteJSON(s.selfPath(), next, 0o600); err != nil {
		return err
	}
	s.self = next
	return nil
}

// Peers lists the linked nodes by name.
func (s *Store) Peers() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Peer(nil), s.peers...)
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// Peer is one linked node by id.
func (s *Store) Peer(id string) (Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.ID == id {
			return p, true
		}
	}
	return Peer{}, false
}

// ByToken is the peer that was given the token with this id, if any: how
// a request is known to come from a linked node and not from a script.
func (s *Store) ByToken(tokenID string) (Peer, bool) {
	if tokenID == "" {
		return Peer{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.TokenID == tokenID {
			return p, true
		}
	}
	return Peer{}, false
}

// Add stores a peer, replacing one with the same id: linking again after
// a reinstall of the other side is a new pair of tokens, not two rows.
// It returns the peer replaced, whose token the caller revokes.
func (s *Store) Add(p Peer) (Peer, bool, error) {
	if p.ID == "" || p.Token == "" {
		return Peer{}, false, errors.New("a peer needs an id and a token")
	}
	if err := CheckURL(p.URL); err != nil {
		return Peer{}, false, err
	}
	p.Name = cleanName(p.Name)
	if p.Added.IsZero() {
		p.Added = s.now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]Peer, 0, len(s.peers)+1)
	var old Peer
	var had bool
	for _, q := range s.peers {
		if q.ID == p.ID {
			old, had = q, true
			continue
		}
		next = append(next, q)
	}
	next = append(next, p)
	if err := fsx.WriteJSON(s.peersPath(), next, 0o600); err != nil {
		return Peer{}, false, err
	}
	s.peers = next
	return old, had, nil
}

// Rename keeps a peer's name in step with what it calls itself.
func (s *Store) Rename(id, name string) {
	name = cleanName(name)
	if name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.peers {
		if s.peers[i].ID == id && s.peers[i].Name != name {
			s.peers[i].Name = name
			_ = fsx.WriteJSON(s.peersPath(), s.peers, 0o600)
		}
	}
}

// Remove forgets a peer and returns it, so the caller can revoke its token.
func (s *Store) Remove(id string) (Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.peers {
		if p.ID != id {
			continue
		}
		next := append(append([]Peer(nil), s.peers[:i]...), s.peers[i+1:]...)
		if err := fsx.WriteJSON(s.peersPath(), next, 0o600); err != nil {
			return Peer{}, err
		}
		s.peers = next
		return p, nil
	}
	return Peer{}, ErrNotFound
}

// NewInvite makes a code for another node to link with this one, which
// answers at selfURL. Invitations live in memory only: a restart voids
// them, and that costs making another.
func (s *Store) NewInvite(selfURL string) (string, time.Time, error) {
	selfURL = strings.TrimRight(strings.TrimSpace(selfURL), "/")
	if err := CheckURL(selfURL); err != nil {
		return "", time.Time{}, err
	}
	secret := random(32)
	exp := s.now().Add(InviteTTL)
	s.mu.Lock()
	for k, e := range s.invites {
		if s.now().After(e) {
			delete(s.invites, k)
		}
	}
	s.invites[hash(secret)] = exp
	self := s.self
	s.mu.Unlock()
	raw, _ := json.Marshal(Invite{ID: self.ID, Name: self.Name, URL: selfURL, Secret: secret})
	return "mwnode_" + base64.RawURLEncoding.EncodeToString(raw), exp, nil
}

// Spend uses an invitation: true once, and only before it expires.
func (s *Store) Spend(secret string) bool {
	h := hash(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, exp := range s.invites {
		if subtle.ConstantTimeCompare([]byte(k), []byte(h)) == 1 {
			delete(s.invites, k)
			return !s.now().After(exp)
		}
	}
	return false
}

// Decode reads a code made by NewInvite on another node.
func (s *Store) Decode(code string) (Invite, error) {
	code = strings.TrimSpace(code)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, "mwnode_"))
	if err != nil || !strings.HasPrefix(code, "mwnode_") {
		return Invite{}, ErrBadInvite
	}
	var inv Invite
	if json.Unmarshal(raw, &inv) != nil || inv.ID == "" || inv.Secret == "" {
		return Invite{}, ErrBadInvite
	}
	if inv.ID == s.Self().ID {
		return Invite{}, ErrSelf
	}
	if err := CheckURL(inv.URL); err != nil {
		return Invite{}, err
	}
	return inv, nil
}

// CheckURL accepts a node's address: https anywhere, plain http only to
// an address that does not cross the internet (loopback, private ranges,
// the VPN's), because the token travels in every request.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return ErrBadURL
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip, err := netip.ParseAddr(host); err == nil && (ip.IsLoopback() || ip.IsPrivate() || isCGNAT(ip)) {
			return nil
		}
		return ErrBadURL
	}
	return ErrBadURL
}

// isCGNAT is 100.64.0.0/10, where Tailscale and some VPNs live.
func isCGNAT(ip netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(ip.Unmap())
}

// BaseURL normalises an address typed or sent by a browser.
func BaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		if h, p, err := net.SplitHostPort(u.Host); err == nil && ((u.Scheme == "https" && p == "443") || (u.Scheme == "http" && p == "80")) {
			u.Host = h
		}
		return u.Scheme + "://" + strings.ToLower(u.Host)
	}
	return raw
}

func cleanName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if len([]rune(name)) > 40 {
		return ""
	}
	return name
}

// ValidName says whether a name would be kept.
func ValidName(name string) bool { return cleanName(name) != "" }

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

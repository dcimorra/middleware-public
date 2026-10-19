// Package inbound is the two doors traffic comes in by. The tunnel door is
// the loopback listener the app already opens, which a reverse tunnel
// pushes to; the ports door is 80 and 443 with certificates of our own.
// Either or both are open, the panel answers through any of them, and an
// ACME challenge is answered on all of them, because a domain that moves
// from one door to the other gets its certificate before its DNS changes.
package inbound

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Door names one of the two.
type Door string

// The doors.
const (
	Tunnel Door = "tunnel"
	Ports  Door = "ports"
)

// ParseDoor validates a name from the CLI or the API.
func ParseDoor(s string) (Door, error) {
	switch Door(s) {
	case Tunnel, Ports:
		return Door(s), nil
	}
	return "", fmt.Errorf("inbound: unknown door %q (want tunnel or ports)", s)
}

// State is data/inbound.json. It is the whole decision: which doors are
// open, where the ports door listens, which CA issues, and which door each
// domain was put on. The CLI edits it with the process running and the
// process picks the change up on the next request.
type State struct {
	Tunnel struct {
		Open bool `json:"open"`
	} `json:"tunnel"`
	Ports struct {
		Open  bool   `json:"open"`
		HTTP  string `json:"http"`
		HTTPS string `json:"https"`
	} `json:"ports"`
	ACME struct {
		// Directory is the CA; empty is Let's Encrypt production.
		Directory string `json:"directory,omitempty"`
		// Email is the account contact.
		Email string `json:"email,omitempty"`
	} `json:"acme"`
	// Domains records the door each domain was moved to. A domain absent
	// here is on the ports door if it has a certificate and on the tunnel
	// otherwise, which is what "nobody decided yet" looks like.
	Domains map[string]Door `json:"domains,omitempty"`
}

// DefaultState is a fresh installation: tunnel open, ports closed. The
// ports door needs CAP_NET_BIND_SERVICE and a public address, neither of
// which an installer can assume.
func DefaultState() State {
	var s State
	s.Tunnel.Open = true
	s.Ports.HTTP = ":80"
	s.Ports.HTTPS = ":443"
	return s
}

// Path is where the file lives under a root.
func Path(root string) string { return filepath.Join(root, "data", "inbound.json") }

// LoadState reads the file, returning defaults when it does not exist.
func LoadState(root string) (State, error) {
	s := DefaultState()
	err := fsx.ReadJSON(Path(root), &s)
	if err != nil && !os.IsNotExist(err) {
		return s, err
	}
	if s.Ports.HTTP == "" {
		s.Ports.HTTP = ":80"
	}
	if s.Ports.HTTPS == "" {
		s.Ports.HTTPS = ":443"
	}
	return s, nil
}

// SaveState writes the file atomically after validating it.
func SaveState(root string, s State) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return fsx.WriteJSON(Path(root), s, 0o644)
}

// Validate refuses what the listeners could not use.
func (s State) Validate() error {
	if s.Ports.HTTP == "" || s.Ports.HTTPS == "" {
		return fmt.Errorf("inbound: ports door needs both http and https addresses")
	}
	// Port 0 is "any free port" and two of them never collide; tests use
	// it to pick real listeners without racing for a number.
	if s.Ports.HTTP == s.Ports.HTTPS && !strings.HasSuffix(s.Ports.HTTP, ":0") {
		return fmt.Errorf("inbound: http and https cannot share %q", s.Ports.HTTP)
	}
	for d, door := range s.Domains {
		if !manifest.ValidDomain(d) {
			return fmt.Errorf("inbound: invalid domain %q", d)
		}
		if _, err := ParseDoor(string(door)); err != nil {
			return err
		}
	}
	return nil
}

// DoorOf returns the recorded door for a domain, or "" when nobody
// decided.
func (s State) DoorOf(domain string) Door { return s.Domains[domain] }

// Domains sorted, for stable output.
func (s State) sortedDomains() []string {
	out := make([]string, 0, len(s.Domains))
	for d := range s.Domains {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// SetDoor opens or closes a door in the file. It is what the CLI does; the
// running process sees the new mtime on its next request.
func SetDoor(root string, door Door, open bool) (State, error) {
	s, err := LoadState(root)
	if err != nil {
		return s, err
	}
	switch door {
	case Tunnel:
		s.Tunnel.Open = open
	case Ports:
		s.Ports.Open = open
	default:
		return s, fmt.Errorf("inbound: unknown door %q", door)
	}
	return s, SaveState(root, s)
}

// stateFile re-reads the file when it changed. Stat per request is cheap
// (one syscall, the inode is cached) and means a CLI edit is honoured
// without a signal, a tick, or a restart. Size is compared along with
// mtime because two writes inside one timestamp tick are possible on a
// filesystem with coarse mtimes.
type stateFile struct {
	path  string
	mu    sync.Mutex
	mtime int64
	size  int64
	state State
	ok    bool
}

func (f *stateFile) get() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := os.Stat(f.path)
	if err != nil {
		if f.ok && os.IsNotExist(err) {
			// The file was removed: defaults again, as a fresh install.
			f.state, f.mtime, f.size = DefaultState(), 0, 0
		}
		if !f.ok {
			f.state, f.ok = DefaultState(), true
		}
		return f.state
	}
	if f.ok && st.ModTime().UnixNano() == f.mtime && st.Size() == f.size {
		return f.state
	}
	root := filepath.Dir(filepath.Dir(f.path))
	s, err := LoadState(root)
	if err != nil {
		// A half-typed edit must not close every door: keep the last good
		// state until the file parses again.
		if !f.ok {
			f.state, f.ok = DefaultState(), true
		}
		return f.state
	}
	f.state, f.mtime, f.size, f.ok = s, st.ModTime().UnixNano(), st.Size(), true
	return f.state
}

// set writes a new state and primes the cache so the writer's own next
// request sees it even if the mtime did not move.
func (f *stateFile) set(s State) error {
	root := filepath.Dir(filepath.Dir(f.path))
	if err := SaveState(root, s); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.ok = s, true
	if st, err := os.Stat(f.path); err == nil {
		f.mtime, f.size = st.ModTime().UnixNano(), st.Size()
	}
	return nil
}

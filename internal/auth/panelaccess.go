package auth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// The panel is protected like a site, with two exceptions, and both exist
// because the panel is the only place the panel is edited from:
//
//   - A request from this machine (loopback, with no proxy header saying
//     it came from elsewhere) always gets in. It does not come from the
//     tunnel; it comes from inside, and whoever is inside can stop
//     everything anyway. It is the way back when an allow list was
//     written wrong.
//   - A list that would shut out whoever saves it is refused, not written.
//
// And a fire exit: `middleware panel open` lifts the lists and the
// closed password for a while (half an hour unless told), from the shell,
// without a restart: the file is read on every request with a one-second
// cache. Authentication still applies; what it opens is the way to the
// login, not the panel.

// PanelAccess is data/auth/panel-access.json.
type PanelAccess struct {
	// Allow and Deny are addresses or CIDRs, a firewall's order: deny
	// first, and an allow list with anything in it means only those.
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
	// PasskeyOnly closes the password: the login answers 403 and points
	// to a passkey. It needs a passkey to be set, and the last passkey
	// cannot be removed while it is.
	PasskeyOnly bool `json:"passkey_only,omitempty"`
}

// ErrLocksYouOut is a list that would shut out the caller saving it.
var ErrLocksYouOut = errors.New("this list would shut you out")

func (s *Store) panelAccessPath() string { return filepath.Join(s.dir, "panel-access.json") }
func (s *Store) panelOpenPath() string   { return filepath.Join(s.dir, "panel-open") }

// PanelAccess reads the panel's rules; unreadable is no rules, which the
// caller of SetPanelAccess cannot write.
func (s *Store) PanelAccess() PanelAccess {
	var a PanelAccess
	_ = fsx.ReadJSON(s.panelAccessPath(), &a)
	return a
}

func validAddrs(list []string) error {
	for _, v := range list {
		if _, err := parseAddr(v); err != nil {
			return err
		}
	}
	return nil
}

func parseAddr(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an address or CIDR", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func inList(ip netip.Addr, list []string) bool {
	for _, v := range list {
		if p, err := parseAddr(v); err == nil && p.Contains(ip) {
			return true
		}
	}
	return false
}

// admits applies the lists to one address.
func (a PanelAccess) admits(ip netip.Addr) bool {
	if inList(ip, a.Deny) {
		return false
	}
	return len(a.Allow) == 0 || inList(ip, a.Allow)
}

// SetPanelAccess validates and writes the lists and the passkey switch.
// caller is the address saving it, which the new lists must admit unless
// it is this machine.
func (s *Store) SetPanelAccess(a PanelAccess, caller netip.Addr, fromMachine bool) error {
	if err := validAddrs(a.Allow); err != nil {
		return fmt.Errorf("allow: %w", err)
	}
	if err := validAddrs(a.Deny); err != nil {
		return fmt.Errorf("deny: %w", err)
	}
	if !fromMachine && (!caller.IsValid() || !a.admits(caller.Unmap())) {
		return fmt.Errorf("%w (%s)", ErrLocksYouOut, caller)
	}
	if a.PasskeyOnly && !s.HasPasskeys("") {
		return ErrNeedsAPasskey
	}
	if err := fsx.WriteJSON(s.panelAccessPath(), a, 0o600); err != nil {
		return err
	}
	s.emit("auth.panel_access", "panel access rules changed", map[string]any{"allow": len(a.Allow), "deny": len(a.Deny), "passkey_only": a.PasskeyOnly})
	return nil
}

// OpenUntil is the end of an emergency opening, or zero.
func (s *Store) OpenUntil() time.Time {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	now := s.now()
	if now.Sub(s.openRead) < time.Second {
		if now.Before(s.openUntil) {
			return s.openUntil
		}
		return time.Time{}
	}
	s.openRead = now
	s.openUntil = time.Time{}
	raw, err := fsx.ReadTrimmed(s.panelOpenPath())
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil || !now.Before(t) {
		// Past (or unreadable): the file goes, so what is on disk says
		// what is in force.
		_ = os.Remove(s.panelOpenPath())
		return time.Time{}
	}
	s.openUntil = t
	return t
}

// OpenPanel lifts the lists and the closed password until the deadline.
func (s *Store) OpenPanel(d time.Duration) (time.Time, error) {
	if d <= 0 || d > 24*time.Hour {
		return time.Time{}, errors.New("an opening lasts between a second and a day")
	}
	until := s.now().Add(d).UTC().Truncate(time.Second)
	if err := fsx.WriteAtomic(s.panelOpenPath(), []byte(until.Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return time.Time{}, err
	}
	s.openMu.Lock()
	s.openRead = time.Time{}
	s.openMu.Unlock()
	s.emit("auth.panel_opened", "panel opened from the shell until "+until.Format(time.RFC3339), map[string]any{"until": until})
	return until, nil
}

// ClosePanel ends an opening.
func (s *Store) ClosePanel() error {
	err := os.Remove(s.panelOpenPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s.openMu.Lock()
	s.openRead, s.openUntil = time.Time{}, time.Time{}
	s.openMu.Unlock()
	if err == nil {
		s.emit("auth.panel_closed", "panel emergency opening closed", nil)
	}
	return nil
}

// FromMachine says whether r came from this machine and not through a
// proxy on its behalf: loopback, with none of the headers an edge writes.
func FromMachine(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, h := range []string{"CF-Connecting-IP", "X-Forwarded-For", "X-Real-IP", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

// PanelAdmits says whether r may reach the panel at all.
func (s *Store) PanelAdmits(r *http.Request) bool {
	if FromMachine(r) || !s.OpenUntil().IsZero() {
		return true
	}
	a := s.PanelAccess()
	if len(a.Allow) == 0 && len(a.Deny) == 0 {
		return true
	}
	ip, ok := callerAddr(r)
	return ok && a.admits(ip)
}

// PasswordClosed says whether the password login is closed for r.
func (s *Store) PasswordClosed(r *http.Request) bool {
	return s.PanelAccess().PasskeyOnly && !FromMachine(r) && s.OpenUntil().IsZero()
}

func callerAddr(r *http.Request) (netip.Addr, bool) {
	a, err := netip.ParseAddr(ClientIP(r))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// CallerAddr is the address the lists see for r.
func CallerAddr(r *http.Request) (netip.Addr, bool) { return callerAddr(r) }

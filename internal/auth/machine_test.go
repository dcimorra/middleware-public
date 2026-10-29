package auth

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

// A guesser that writes a new address on every try (from loopback,
// CF-Connecting-IP is believed, for the tunnel's sake) never trips one
// address's brake; the house's budget closes the password for everyone.
func TestWrongPasswordsFromManyAddressesCloseThePasswordForAll(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.limiter.Now, s.house.Now = s.now, s.now
	s.SetPassword("password-1")
	for i := range houseLoginPolicy.Failures {
		if _, err := s.Login("wrong", fmt.Sprintf("198.51.100.%d", i+1)); err != ErrBadCredentials {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := s.Login("password-1", "203.0.113.1"); err != ErrRateLimited {
		t.Fatalf("an address never seen got past the house's budget: %v", err)
	}
	now = now.Add(houseLoginPolicy.Window + time.Second)
	if _, err := s.Login("password-1", "203.0.113.1"); err != nil {
		t.Fatalf("still closed after the window: %v", err)
	}
}

// A routed IPv6 prefix is one guesser: its /64s are not fresh addresses.
func TestTheLoginCountsAnIPv6SlashFortyEightAsOne(t *testing.T) {
	s := newStore(t)
	s.SetPassword("password-1")
	for i := range loginPolicy.Failures {
		s.Login("wrong", fmt.Sprintf("2001:db8:1:%x::1", i))
	}
	if _, err := s.Login("password-1", "2001:db8:1:ffff::1"); err != ErrRateLimited {
		t.Fatalf("another /64 of the same /48: %v", err)
	}
	if _, err := s.Login("password-1", "2001:db8:2::1"); err != nil {
		t.Fatalf("another /48 was locked out too: %v", err)
	}
}

// Loopback is not the house: a site's process, or a site made to fetch a
// URL, connects from there. Only the socket skips the lists and the
// closed password; the shell's opening lifts both for everyone.
func TestOnlyTheSocketIsThisMachine(t *testing.T) {
	s := newStore(t)
	if err := fsx.WriteJSON(s.panelAccessPath(), PanelAccess{Allow: []string{"198.51.100.0/24"}, PasskeyOnly: true}, 0o600); err != nil {
		t.Fatal(err)
	}
	loop := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	loop.RemoteAddr = "127.0.0.1:5555"
	sock := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	sock = sock.WithContext(WithSocket(sock.Context()))
	sock.RemoteAddr = ""
	if FromMachine(loop) || !Direct(loop) {
		t.Fatal("plain loopback must be direct, and not the house")
	}
	if s.PanelAdmits(loop) || !s.PasswordClosed(loop) {
		t.Fatal("plain loopback skipped the allow list or the closed password")
	}
	if !FromMachine(sock) || !s.PanelAdmits(sock) || s.PasswordClosed(sock) {
		t.Fatal("the socket was held to the lists")
	}
	if _, err := s.OpenPanel(time.Minute); err != nil {
		t.Fatal(err)
	}
	if !s.PanelAdmits(loop) || s.PasswordClosed(loop) {
		t.Fatal("the shell's opening did not lift the lists and the password")
	}
}

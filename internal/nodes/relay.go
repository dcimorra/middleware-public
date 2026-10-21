package nodes

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// NetSettings are decided once for the whole network and travel with the
// roster, newest change winning, like a member's entry.
type NetSettings struct {
	// PanelVPNOnly closes every node's panel to the internet: it answers
	// through a VPN (its own, or another node's, relayed) and through the
	// mesh. One node closed while the others stay open would leave the
	// network as open as its most open panel.
	PanelVPNOnly bool      `json:"panel_vpn_only"`
	Updated      time.Time `json:"updated"`
}

// rosterAnswer is GET /mesh/roster. Nodes of alpha.15 answered a bare
// list of members, which still decodes, without settings.
type rosterAnswer struct {
	Members  []Member    `json:"members"`
	Settings NetSettings `json:"settings"`
}

// UnmarshalJSON accepts both shapes.
func (r *rosterAnswer) UnmarshalJSON(b []byte) error {
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '[' {
		return json.Unmarshal(t, &r.Members)
	}
	type plain rosterAnswer
	return json.Unmarshal(b, (*plain)(r))
}

// Settings are the network's settings; zero outside a network.
func (m *Mesh) Settings() NetSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.Settings
}

// SetSettings changes the network's settings here; the other nodes take
// them in the next round.
func (m *Mesh) SetSettings(s NetSettings) error {
	m.mu.Lock()
	if m.st.Network == "" {
		m.mu.Unlock()
		return ErrNoNetwork
	}
	if m.st.Settings.PanelVPNOnly == s.PanelVPNOnly {
		m.mu.Unlock()
		return nil
	}
	s.Updated = m.now()
	m.st.Settings = s
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.emit("mesh.settings", "", "", fmt.Sprintf("network settings changed: panel only through a VPN = %v", s.PanelVPNOnly))
	m.settingsChanged()
	return nil
}

func (m *Mesh) settingsChanged() {
	if m.opts.OnSettings != nil {
		m.opts.OnSettings(m.Settings())
	}
}

// Owner is the live member that answers for host (a name it serves or one
// under it), as of the last round.
func (m *Mesh) Owner(host string) (Member, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return Member{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.liveLocked() {
		for _, n := range m.names[x.ID] {
			if host == n || strings.HasSuffix(host, "."+n) {
				return x, true
			}
		}
	}
	return Member{}, false
}

// relayMagic starts the line a node writes before the bytes it relays.
const relayMagic = "MWRELAY1"

// Relay passes a connection that arrived at this node's VPN door for host
// to the node that answers for it, through the mesh. It never decrypts
// it: the TLS is between the device and that node, with that node's
// certificate. pre are the bytes already read (the ClientHello). device
// is the VPN peer's name here, so a site narrowed to some devices there
// can still tell them apart. It returns when the connection ends.
func (m *Mesh) Relay(c net.Conn, host string, pre []byte, device string) error {
	defer c.Close()
	x, ok := m.Owner(host)
	if !ok {
		return fmt.Errorf("no node answers for %s", host)
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	m.mu.Lock()
	if m.st.IP.IsValid() {
		d.LocalAddr = &net.TCPAddr{IP: net.IP(m.st.IP.AsSlice())}
	}
	secret := m.st.Secret
	m.mu.Unlock()
	up, err := d.Dial("tcp", netip.AddrPortFrom(x.IP, uint16(m.opts.RelayPort)).String())
	if err != nil {
		return fmt.Errorf("relay to %s: %w", x.Name, err)
	}
	defer up.Close()
	if _, err := fmt.Fprintf(up, "%s %s %s\n", relayMagic, secret, cleanDevice(device)); err != nil {
		return err
	}
	if _, err := up.Write(pre); err != nil {
		return err
	}
	splice(c, up)
	return nil
}

func cleanDevice(d string) string {
	var b strings.Builder
	for _, r := range d {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 || b.Len() > 64 {
		return "-"
	}
	return b.String()
}

// splice copies both ways until either side ends.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// openRelay listens for relayed connections on the mesh address.
func (m *Mesh) openRelay(ip netip.Addr) error {
	m.mu.Lock()
	ln := m.relay
	m.mu.Unlock()
	if ln != nil {
		if ap, err := netip.ParseAddrPort(ln.Addr().String()); err == nil && ap.Addr() == ip {
			return nil
		}
	}
	m.closeRelay()
	ln, err := net.Listen("tcp", netip.AddrPortFrom(ip, uint16(m.opts.RelayPort)).String())
	if err != nil {
		return fmt.Errorf("the relay door on %s:%d: %w", ip, m.opts.RelayPort, err)
	}
	m.mu.Lock()
	m.relay = ln
	m.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serveRelayed(c)
		}
	}()
	return nil
}

func (m *Mesh) closeRelay() {
	m.mu.Lock()
	ln := m.relay
	m.relay = nil
	m.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

var errBadRelay = errors.New("not a relay from a node of this network")

// serveRelayed checks a relayed connection and hands it to the house as
// one that came through the VPN: from a live member's tunnel address, with
// the network's secret on its first line.
func (m *Mesh) serveRelayed(c net.Conn) {
	from := netip.Addr{}
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		from = ap.Addr().Unmap()
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(c, 256)
	_ = c.SetReadDeadline(time.Time{})
	device, ok := "", false
	if err == nil {
		f := strings.Fields(line)
		m.mu.Lock()
		secret := m.st.Secret
		member := false
		for _, x := range m.liveLocked() {
			if x.IP == from {
				member = true
			}
		}
		m.mu.Unlock()
		if len(f) == 3 && f[0] == relayMagic && secret != "" && member && subtle.ConstantTimeCompare([]byte(f[1]), []byte(secret)) == 1 {
			ok = true
			if f[2] != "-" {
				device = f[2]
			}
		}
	}
	if !ok || m.opts.OnRelay == nil {
		_ = c.Close()
		return
	}
	m.opts.OnRelay(c, device)
}

// readLine reads up to a newline one byte at a time, so nothing after it
// (the device's TLS) is taken from the connection.
func readLine(r io.Reader, max int) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < max {
		if _, err := io.ReadFull(r, one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
	return "", errBadRelay
}

// Names lists what every other live node answers for, for the VPN's
// resolver; by node name.
func (m *Mesh) Names() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	for _, x := range m.liveLocked() {
		out[x.Name] = append([]string(nil), m.names[x.ID]...)
	}
	return out
}

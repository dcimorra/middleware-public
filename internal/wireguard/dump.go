package wireguard

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Dump is the parsed output of `wg show <iface> dump`: one line for the
// interface, then one per peer, tab-separated. It is the only machine
// readable output wg has and its columns have not moved since 2016.
type Dump struct {
	PrivateKey Key
	PublicKey  Key
	ListenPort int
	Peers      []PeerStatus
}

// PeerStatus is what the kernel knows about one peer.
type PeerStatus struct {
	PublicKey           Key       `json:"public_key"`
	PresharedKey        Key       `json:"-"`
	Endpoint            string    `json:"endpoint,omitempty"`
	AllowedIPs          []string  `json:"allowed_ips"`
	LastHandshake       time.Time `json:"last_handshake"` // zero when there was none
	ReceiveBytes        int64     `json:"rx"`
	TransmitBytes       int64     `json:"tx"`
	PersistentKeepalive int       `json:"persistent_keepalive,omitempty"`
}

// Online reports whether the peer completed a handshake recently. WireGuard
// re-handshakes every two minutes while traffic flows, so three minutes of
// silence means the client went away.
func (p PeerStatus) Online(now time.Time) bool {
	return !p.LastHandshake.IsZero() && now.Sub(p.LastHandshake) < 3*time.Minute
}

// ParseDump parses the text. An empty text is a valid dump with no peers.
func ParseDump(text string) (Dump, error) {
	var d Dump
	sc := bufio.NewScanner(strings.NewReader(text))
	first := true
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if first {
			first = false
			if len(f) < 4 {
				return d, fmt.Errorf("wireguard: interface line has %d fields", len(f))
			}
			d.PrivateKey, _ = parseOptionalKey(f[0])
			d.PublicKey, _ = parseOptionalKey(f[1])
			d.ListenPort, _ = strconv.Atoi(f[2])
			continue
		}
		if len(f) < 8 {
			return d, fmt.Errorf("wireguard: peer line has %d fields", len(f))
		}
		var p PeerStatus
		var err error
		if p.PublicKey, err = ParseKey(f[0]); err != nil {
			return d, err
		}
		p.PresharedKey, _ = parseOptionalKey(f[1])
		if f[2] != "(none)" {
			p.Endpoint = f[2]
		}
		if f[3] != "(none)" {
			p.AllowedIPs = strings.Split(f[3], ",")
		}
		if secs, _ := strconv.ParseInt(f[4], 10, 64); secs > 0 {
			p.LastHandshake = time.Unix(secs, 0)
		}
		p.ReceiveBytes, _ = strconv.ParseInt(f[5], 10, 64)
		p.TransmitBytes, _ = strconv.ParseInt(f[6], 10, 64)
		if f[7] != "off" {
			p.PersistentKeepalive, _ = strconv.Atoi(f[7])
		}
		d.Peers = append(d.Peers, p)
	}
	return d, sc.Err()
}

// parseOptionalKey treats "(none)" as the zero key.
func parseOptionalKey(s string) (Key, error) {
	if s == "(none)" || s == "" {
		return Key{}, nil
	}
	return ParseKey(s)
}

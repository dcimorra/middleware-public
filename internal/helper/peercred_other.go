//go:build !linux

package helper

import (
	"errors"
	"net"
)

// peerCredentials has no portable implementation; the helper is a Linux
// service (nftables, WireGuard, sysctl) and this stub only keeps the package
// compiling on a developer's laptop.
func peerCredentials(net.Conn) (Peer, error) {
	return Peer{}, errors.New("peer credentials are only available on linux")
}

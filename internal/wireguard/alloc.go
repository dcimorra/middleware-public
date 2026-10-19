package wireguard

import (
	"errors"
	"fmt"
	"net/netip"
)

// DefaultSubnet is where the VPN lives unless the operator says otherwise.
// 10.77.0.0/24 was chosen because neither Docker (172.17+) nor the common
// home routers (192.168.x, 10.0.0.x) use it, so a client at home does not
// end up with two routes to the same block.
var DefaultSubnet = netip.MustParsePrefix("10.77.0.0/24")

// ErrSubnetFull is returned when every host address is taken.
var ErrSubnetFull = errors.New("wireguard: no free address in the subnet")

// ServerAddress is the first host of the subnet, which the server keeps.
func ServerAddress(subnet netip.Prefix) netip.Addr {
	return subnet.Masked().Addr().Next()
}

// NextAddress returns the lowest free host address in subnet, skipping the
// network address, the server's and the broadcast. Reusing the lowest free
// one rather than appending keeps the allocation bounded after peers come
// and go; nothing identifies a peer by its address alone, so reuse is safe.
func NextAddress(subnet netip.Prefix, used []netip.Addr) (netip.Addr, error) {
	if !subnet.IsValid() || !subnet.Addr().Is4() {
		return netip.Addr{}, errors.New("wireguard: subnet must be IPv4")
	}
	if subnet.Bits() > 30 {
		return netip.Addr{}, fmt.Errorf("wireguard: subnet /%d has no room for peers", subnet.Bits())
	}
	taken := map[netip.Addr]bool{}
	for _, u := range used {
		taken[u] = true
	}
	for a := ServerAddress(subnet).Next(); subnet.Contains(a); a = a.Next() {
		if !subnet.Contains(a.Next()) {
			break // the last address is the broadcast
		}
		if !taken[a] {
			return a, nil
		}
	}
	return netip.Addr{}, ErrSubnetFull
}

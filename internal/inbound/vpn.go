package inbound

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/dcimorra/middleware-public/internal/acme"
	"github.com/dcimorra/middleware-public/internal/localdns"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// VPN is the door inside the VPN: the server's address in the tunnel, on
// 80, 443 and 53, opened when the VPN is up and closed when it goes down.
// It is not in the state file, because it is not a choice: a VPN that is
// up has a door. WireGuard drops a packet whose source does not belong to
// the key that encrypted it, so inside the tunnel RemoteAddr can be
// believed, and that is what lets a site be narrowed to some devices.
const VPN Door = "vpn"

// reconcileVPNLocked keeps the VPN door's sockets on the address the VPN
// module reports. Called under h.mu.
func (h *Hook) reconcileVPNLocked(covered bool) {
	var addr netip.Addr
	up := false
	if h.VPNAddress != nil {
		addr, up = h.VPNAddress()
	}
	if !up {
		if h.vpnDNS != nil || h.vpnHTTP != nil {
			h.closeVPNLocked()
			h.emit("engine", "inbound.stopped", string(VPN), "", "vpn door closed", nil)
		}
		return
	}
	if h.vpnDNS != nil && h.vpnWant == addr && h.vpnCovered == covered {
		return
	}
	h.closeVPNLocked()
	if err := h.openVPNLocked(addr, covered); err != nil {
		h.closeVPNLocked()
		if err.Error() != h.lastVPNErr {
			h.lastVPNErr = err.Error()
			h.emit("engine", "inbound.failed", string(VPN), "error", err.Error(), map[string]any{"address": addr.String()})
		}
		return
	}
	h.lastVPNErr = ""
	h.emit("engine", "inbound.listening", string(VPN), "", "vpn door listening on "+addr.String(), nil)
}

// VPNPorts are the VPN door's ports: 80, 443 and 53, which is what a
// device expects; tests set "0".
type VPNPorts struct{ HTTP, HTTPS, DNS string }

func (h *Hook) vpnPorts() VPNPorts {
	p := h.VPNPorts
	if p.HTTP == "" {
		p.HTTP = "80"
	}
	if p.HTTPS == "" {
		p.HTTPS = "443"
	}
	if p.DNS == "" {
		p.DNS = "53"
	}
	return p
}

// VPNAddrs are the VPN door's listening addresses (http, https, dns), or
// empty while it is closed.
func (h *Hook) VPNAddrs() (httpAddr, httpsAddr, dnsAddr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.vpnDNS == nil {
		return "", "", ""
	}
	if h.vpnCovered {
		if h.http == nil || h.https == nil {
			return "", "", ""
		}
		return h.http.ln.Addr().String(), h.https.ln.Addr().String(), h.vpnDNS.Addr()
	}
	if h.vpnHTTP == nil || h.vpnHTTPS == nil {
		return "", "", ""
	}
	return h.vpnHTTP.ln.Addr().String(), h.vpnHTTPS.ln.Addr().String(), h.vpnDNS.Addr()
}

func (h *Hook) openVPNLocked(addr netip.Addr, covered bool) error {
	tag := func(ctx context.Context, _ net.Conn) context.Context { return context.WithValue(ctx, ctxKey{}, VPN) }
	host := addr.String()
	ports := h.vpnPorts()
	h.vpnCovered = covered
	a := addr
	h.vpnLocal.Store(&a)
	if covered {
		return h.openVPNDNSLocked(addr)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, ports.HTTP))
	if err != nil {
		return fmt.Errorf("inbound: vpn http: %w", err)
	}
	// Plain HTTP inside the tunnel is already encrypted by WireGuard; it
	// serves directly, without the redirect the public 80 does.
	h.vpnHTTP = &listener{want: host, ln: ln, srv: &http.Server{Handler: h.Handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: IdleTimeout, ConnContext: tag}}
	go h.vpnHTTP.srv.Serve(ln)
	ln, err = net.Listen("tcp", net.JoinHostPort(host, ports.HTTPS))
	if err != nil {
		return fmt.Errorf("inbound: vpn https: %w", err)
	}
	h.vpnHTTPS = &listener{want: host, ln: ln, srv: &http.Server{Handler: h.Handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: IdleTimeout, ConnContext: tag,
		TLSConfig: &tls.Config{GetCertificate: h.Store.GetCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1", acme.ALPNProto}}}}
	go h.vpnHTTPS.srv.ServeTLS(h.relayed(ln, func(net.Conn) bool { return true }), "", "")
	return h.openVPNDNSLocked(addr)
}

func (h *Hook) openVPNDNSLocked(addr netip.Addr) error {
	host := addr.String()
	ports := h.vpnPorts()
	dns := &localdns.Server{Serves: h.vpnServes, V4: net.IP(addr.AsSlice()), NoV6: true, Forward: localdns.SystemResolver()}
	if err := dns.Listen(context.Background(), net.JoinHostPort(host, ports.DNS)); err != nil {
		return fmt.Errorf("inbound: vpn dns: %w", err)
	}
	h.vpnDNS, h.vpnWant = dns, addr
	return nil
}

func (h *Hook) closeVPNLocked() {
	for _, l := range []*listener{h.vpnHTTP, h.vpnHTTPS} {
		if l == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = l.srv.Shutdown(ctx)
		cancel()
		_ = l.ln.Close()
	}
	h.vpnHTTP, h.vpnHTTPS = nil, nil
	if h.vpnDNS != nil {
		h.vpnDNS.Close()
		h.vpnDNS = nil
	}
	h.vpnWant = netip.Addr{}
	h.vpnCovered = false
	h.vpnLocal.Store(nil)
}

// vpnAllows says whether a request may reach host, given the door it came
// by: a VPN-only site exists only through the VPN door, and, with devices
// listed, only for those peers.
func (h *Hook) vpnAllows(r *http.Request, host string, door Door) bool {
	if h.VPNOnly == nil {
		return true
	}
	devices, only := h.VPNOnly(host)
	if !only {
		return true
	}
	if h.VPNPaths != nil {
		if paths := h.VPNPaths(host); len(paths) > 0 && !(&manifest.VPNOnly{Paths: paths}).Covers(r.URL.Path) {
			return true
		}
	}
	if door != VPN {
		return false
	}
	if len(devices) == 0 {
		return true
	}
	name := h.deviceFrom(r)
	for _, d := range devices {
		if d == name && name != "" {
			return true
		}
	}
	return false
}

// panelAllows says whether a request may reach the panel, given the door
// it came by. With the panel inside the VPN, it answers through the VPN
// door and through the machine's own loopback when nothing in front of it
// relayed the request: that is an SSH tunnel, the CLI, a site calling its
// door, never the internet. A tunnel's connector also connects from
// loopback, but it says whom it relays for (CF-Connecting-IP), and then
// the request is the internet's.
func (h *Hook) panelAllows(r *http.Request, door Door) bool {
	if h.PanelVPNOnly == nil || !h.PanelVPNOnly() || door == VPN {
		return true
	}
	if door != Tunnel {
		return false
	}
	ip, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !ip.Addr().Unmap().IsLoopback() {
		return false
	}
	for _, hdr := range []string{"Cf-Connecting-Ip", "X-Forwarded-For", "X-Real-Ip", "True-Client-Ip"} {
		if r.Header.Get(hdr) != "" {
			return false
		}
	}
	return true
}

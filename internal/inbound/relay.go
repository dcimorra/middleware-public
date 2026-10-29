package inbound

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/acme"
)

// A device on this node's VPN reaches the other nodes of the network
// through it: the VPN's resolver answers their names with this node's
// address, and the VPN door passes a connection for one of them, still
// encrypted, to the node that serves it (Relay, through the mesh). There
// it is served as one that came through the VPN: the panel closed to the
// internet opens to it, and a VPN-only site exists for it. So one VPN
// profile, any node's, is a way into every node, even nodes with no VPN
// of their own and on another provider's network.

type deviceKey struct{}

// relayedConn is a connection another node relayed, with the VPN device
// it came from there.
type relayedConn struct {
	net.Conn
	device string
}

// replayConn gives back the bytes read to find the name before the rest.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// recordConn keeps what is read and swallows what is written: the TLS
// handshake below is only there to parse the ClientHello, and the alert
// it would send when it is aborted must not reach the device.
type recordConn struct {
	net.Conn
	buf bytes.Buffer
}

func (c *recordConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.buf.Write(p[:n])
	return n, err
}
func (c *recordConn) Write(p []byte) (int, error) { return len(p), nil }

var errPeeked = errors.New("peeked")

// peekSNI reads the ClientHello and returns the name asked for and the
// bytes read. crypto/tls parses it; the handshake is abandoned there.
func peekSNI(c net.Conn) (string, []byte) {
	rec := &recordConn{Conn: c}
	var name string
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_ = tls.Server(rec, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		name = strings.ToLower(h.ServerName)
		return nil, errPeeked
	}}).Handshake()
	_ = c.SetReadDeadline(time.Time{})
	return name, rec.buf.Bytes()
}

// elsewhere says host is not this node's but another node's.
func (h *Hook) elsewhere(host string) bool {
	return host != "" && !h.knownSite(host) && h.Elsewhere != nil && h.Elsewhere(host)
}

// vpnServes is the VPN resolver's list: this node's names and the other
// nodes'.
func (h *Hook) vpnServes(name string) bool {
	return h.knownSite(name) || (h.Elsewhere != nil && h.Elsewhere(name))
}

// relayListener sits under a TLS server and takes out the VPN's
// connections for another node's names before TLS sees them. isVPN says
// whether a connection came through the VPN; the rest pass untouched.
type relayListener struct {
	net.Listener
	h     *Hook
	isVPN func(net.Conn) bool
	ch    chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (h *Hook) relayed(ln net.Listener, isVPN func(net.Conn) bool) net.Listener {
	rl := &relayListener{Listener: ln, h: h, isVPN: isVPN, ch: make(chan net.Conn), done: make(chan struct{})}
	go rl.loop()
	return rl
}

func (l *relayListener) loop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			l.once.Do(func() { close(l.done) })
			return
		}
		if l.h.Relay == nil || l.h.Elsewhere == nil || !l.isVPN(c) {
			select {
			case l.ch <- c:
			case <-l.done:
				_ = c.Close()
				return
			}
			continue
		}
		go func(c net.Conn) {
			name, pre := peekSNI(c)
			if l.h.elsewhere(name) {
				device := ""
				if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil && l.h.DeviceOf != nil {
					device = l.h.DeviceOf(ap.Addr().Unmap())
				}
				if err := l.h.Relay(c, name, pre, device); err != nil {
					l.h.emit("engine", "inbound.relay_failed", name, "warn", err.Error(), nil)
				}
				return
			}
			select {
			case l.ch <- &replayConn{Conn: c, r: io.MultiReader(bytes.NewReader(pre), c)}:
			case <-l.done:
				_ = c.Close()
			}
		}(c)
	}
}

func (l *relayListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *relayListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// chanListener is a listener fed by hand: the connections other nodes
// relay to this one.
type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

// AcceptRelayed serves a connection another node's VPN door relayed here,
// as one that came through this node's VPN, for device.
func (h *Hook) AcceptRelayed(c net.Conn, device string) {
	h.mu.Lock()
	if h.relayIn == nil {
		ln := &chanListener{ch: make(chan net.Conn), done: make(chan struct{})}
		srv := &http.Server{
			Handler:           h.Handler,
			ReadHeaderTimeout: 15 * time.Second,
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				ctx = context.WithValue(ctx, ctxKey{}, VPN)
				if tc, ok := c.(*tls.Conn); ok {
					c = tc.NetConn()
				}
				if rc, ok := c.(*relayedConn); ok && rc.device != "" {
					ctx = context.WithValue(ctx, deviceKey{}, rc.device)
				}
				return ctx
			},
			TLSConfig: &tls.Config{GetCertificate: h.Store.GetCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1", acme.ALPNProto}},
		}
		go srv.ServeTLS(ln, "", "")
		h.relayIn = ln
	}
	ln := h.relayIn
	h.mu.Unlock()
	select {
	case ln.ch <- &relayedConn{Conn: c, device: device}:
	case <-ln.done:
		_ = c.Close()
	case <-time.After(10 * time.Second):
		_ = c.Close()
	}
}

// deviceFrom is the VPN device a request came from: the peer with its
// address here, or the one another node named when it relayed it.
func (h *Hook) deviceFrom(r *http.Request) string {
	if d, ok := r.Context().Value(deviceKey{}).(string); ok {
		return d
	}
	ip, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || h.DeviceOf == nil {
		return ""
	}
	return h.DeviceOf(ip.Addr().Unmap())
}

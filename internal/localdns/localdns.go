// Package localdns is the resolver behind the local door: a name server
// that answers loopback for the names this installation serves, so a
// browser on the same machine reaches misite.middleware without the
// internet, a hosts file, or a real zone. It speaks enough of RFC 1035 to
// answer A and AAAA over UDP and TCP and to say NXDOMAIN to the rest; it
// never recurses, so pointing a resolver at it for one suffix (Linux
// systemd-resolved, macOS /etc/resolver, Windows NRPT) costs nothing for
// every other name.
package localdns

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Server answers for the names Serves says yes to.
type Server struct {
	// Serves reports whether a name (lower-case, no trailing dot) is one
	// of ours. A name under a served name is ours too, so a site's
	// subdomains resolve without a row each: the proxy decides what to
	// do with them.
	Serves func(name string) bool
	// TTL is what answers carry. Short, because a site that appears gets
	// found on the next query rather than after a cache expires.
	TTL uint32
	// V4 and V6 are the addresses answered; loopback by default.
	V4 net.IP
	V6 net.IP
	// NoV6 answers AAAA with nothing: the VPN's server has an IPv4 inside
	// the tunnel only, and a public IPv6 handed out for a VPN-only name
	// would take the browser to a door that is closed to it.
	NoV6 bool
	// Forward, when set ("host:53"), is asked about every name that is
	// not ours, and its answer relayed: the VPN's resolver is the device's
	// only one, and a phone whose other names stop resolving is a phone
	// with no internet.
	Forward string

	mu   sync.Mutex
	udp  net.PacketConn
	tcp  net.Listener
	done chan struct{}
}

const maxUDP = 512

// Listen binds UDP and TCP on addr (the same port for both, as resolvers
// expect) and serves until ctx ends. It returns once both sockets are
// open, so a caller can read Addr right after.
func (s *Server) Listen(ctx context.Context, addr string) error {
	var udp net.PacketConn
	var tcp net.Listener
	// TCP on the exact port UDP got, which matters when addr ends in :0:
	// the kernel picks a UDP port that may be taken for TCP, so with :0
	// another pick is tried rather than failing on a coincidence.
	for attempt := 0; ; attempt++ {
		var err error
		if udp, err = net.ListenPacket("udp", addr); err != nil {
			return err
		}
		if tcp, err = net.Listen("tcp", udp.LocalAddr().String()); err == nil {
			break
		}
		udp.Close()
		if !strings.HasSuffix(addr, ":0") || attempt == 9 {
			return err
		}
	}
	s.mu.Lock()
	s.udp, s.tcp, s.done = udp, tcp, make(chan struct{})
	s.mu.Unlock()
	go s.serveUDP(udp)
	go s.serveTCP(tcp)
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return nil
}

// Addr is where the server listens, empty when closed.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udp == nil {
		return ""
	}
	return s.udp.LocalAddr().String()
}

// Close stops both sockets. Safe to call twice.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udp != nil {
		s.udp.Close()
		s.udp = nil
	}
	if s.tcp != nil {
		s.tcp.Close()
		s.tcp = nil
	}
}

func (s *Server) serveUDP(pc net.PacketConn) {
	buf := make([]byte, maxUDP)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := s.Answer(buf[:n]); resp != nil {
			if len(resp) > maxUDP {
				// Truncate and set TC so the client retries over TCP; our
				// answers are one record, so this is belt and braces.
				resp = resp[:maxUDP]
				resp[2] |= 0x02
			}
			pc.WriteTo(resp, from)
		}
	}
}

func (s *Server) serveTCP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			var lenBuf [2]byte
			if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
				return
			}
			msg := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
			if _, err := io.ReadFull(c, msg); err != nil {
				return
			}
			resp := s.Answer(msg)
			if resp == nil {
				return
			}
			binary.BigEndian.PutUint16(lenBuf[:], uint16(len(resp)))
			c.Write(append(lenBuf[:], resp...))
		}()
	}
}

const (
	typeA    = 1
	typeAAAA = 28
	classIN  = 1

	rcodeOK       = 0
	rcodeFormErr  = 1
	rcodeNXDomain = 3
	rcodeRefused  = 5
)

// Answer builds the response to one query, or nil when the bytes are not
// a query at all (a response, garbage) and silence is the right reply.
func (s *Server) Answer(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	flags := binary.BigEndian.Uint16(q[2:4])
	if flags&0x8000 != 0 { // QR set: this is a response, not a question
		return nil
	}
	opcode := (flags >> 11) & 0xF
	qd := binary.BigEndian.Uint16(q[4:6])
	resp := make([]byte, 12, 128)
	copy(resp[0:2], q[0:2]) // id
	// QR=1, AA=1, RD copied, RA=0: we are authoritative for what we
	// answer and recurse for nothing.
	rflags := uint16(0x8400) | (flags & 0x0100)
	if opcode != 0 || qd != 1 {
		binary.BigEndian.PutUint16(resp[2:4], rflags|rcodeFormErr)
		return resp
	}
	name, qtype, qclass, end, err := readQuestion(q, 12)
	if err != nil {
		binary.BigEndian.PutUint16(resp[2:4], rflags|rcodeFormErr)
		return resp
	}
	// The question is echoed as it came, bytes included.
	resp = append(resp, q[12:end]...)
	binary.BigEndian.PutUint16(resp[4:6], 1)
	if qclass != classIN {
		binary.BigEndian.PutUint16(resp[2:4], rflags|rcodeRefused)
		return resp
	}
	if !s.serves(name) {
		if s.Forward != "" {
			if out := s.forward(q); out != nil {
				return out
			}
			binary.BigEndian.PutUint16(resp[2:4], rflags|0x0080|2) // RA, SERVFAIL
			return resp
		}
		binary.BigEndian.PutUint16(resp[2:4], rflags|rcodeNXDomain)
		return resp
	}
	var rdata []byte
	switch qtype {
	case typeA:
		ip := s.V4
		if ip == nil {
			ip = net.IPv4(127, 0, 0, 1)
		}
		rdata = ip.To4()
	case typeAAAA:
		if s.NoV6 {
			break
		}
		ip := s.V6
		if ip == nil {
			ip = net.IPv6loopback
		}
		rdata = ip.To16()
	}
	binary.BigEndian.PutUint16(resp[2:4], rflags|rcodeOK)
	if rdata == nil {
		// A name we serve asked for a type we do not have (MX, TXT): the
		// empty NOERROR answer that says "exists, nothing of that".
		return resp
	}
	ttl := s.TTL
	if ttl == 0 {
		ttl = 30
	}
	// Answer: a pointer to the question's name (0xC00C), type, class,
	// TTL, length, data.
	resp = append(resp, 0xC0, 0x0C)
	resp = binary.BigEndian.AppendUint16(resp, qtype)
	resp = binary.BigEndian.AppendUint16(resp, classIN)
	resp = binary.BigEndian.AppendUint32(resp, ttl)
	resp = binary.BigEndian.AppendUint16(resp, uint16(len(rdata)))
	resp = append(resp, rdata...)
	binary.BigEndian.PutUint16(resp[6:8], 1)
	return resp
}

// forward asks the upstream resolver and returns its answer as it came,
// or nil when it does not answer in two seconds.
func (s *Server) forward(q []byte) []byte {
	c, err := net.DialTimeout("udp", s.Forward, 2*time.Second)
	if err != nil {
		return nil
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(q); err != nil {
		return nil
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil || n < 12 || buf[0] != q[0] || buf[1] != q[1] {
		return nil
	}
	return buf[:n]
}

// SystemResolver is the first nameserver of /etc/resolv.conf as host:53,
// or 1.1.1.1:53 when there is none.
func SystemResolver() string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 && f[0] == "nameserver" && net.ParseIP(f[1]) != nil {
				return net.JoinHostPort(f[1], "53")
			}
		}
	}
	return "1.1.1.1:53"
}

func (s *Server) serves(name string) bool {
	if s.Serves == nil {
		return false
	}
	for {
		if s.Serves(name) {
			return true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return false
		}
		name = name[i+1:]
	}
}

var errBadName = errors.New("localdns: malformed question")

// readQuestion decodes the one question at off: labels, type, class.
// Compression pointers are refused in a question (no sane client sends
// them) so a crafted packet cannot loop the parser.
func readQuestion(msg []byte, off int) (name string, qtype, qclass uint16, end int, err error) {
	var labels []string
	for {
		if off >= len(msg) {
			return "", 0, 0, 0, errBadName
		}
		l := int(msg[off])
		off++
		if l == 0 {
			break
		}
		if l&0xC0 != 0 || off+l > len(msg) {
			return "", 0, 0, 0, errBadName
		}
		labels = append(labels, strings.ToLower(string(msg[off:off+l])))
		off += l
	}
	if off+4 > len(msg) {
		return "", 0, 0, 0, errBadName
	}
	qtype = binary.BigEndian.Uint16(msg[off : off+2])
	qclass = binary.BigEndian.Uint16(msg[off+2 : off+4])
	return strings.Join(labels, "."), qtype, qclass, off + 4, nil
}

// Query is a client for tests and the CLI: it asks addr for name's A or
// AAAA over UDP and returns the addresses, or nil with a nil error on
// NXDOMAIN.
func Query(ctx context.Context, addr, name string, v6 bool) ([]net.IP, error) {
	qtype := uint16(typeA)
	if v6 {
		qtype = typeAAAA
	}
	msg := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		msg = append(msg, byte(len(l)))
		msg = append(msg, l...)
	}
	msg = append(msg, 0)
	msg = binary.BigEndian.AppendUint16(msg, qtype)
	msg = binary.BigEndian.AppendUint16(msg, classIN)
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(3 * time.Second))
	}
	if _, err := c.Write(msg); err != nil {
		return nil, err
	}
	buf := make([]byte, maxUDP)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	resp := buf[:n]
	if len(resp) < 12 || resp[0] != msg[0] || resp[1] != msg[1] {
		return nil, errors.New("localdns: bad response")
	}
	if rcode := resp[3] & 0xF; rcode == rcodeNXDomain {
		return nil, nil
	} else if rcode != rcodeOK {
		return nil, errors.New("localdns: server refused the query")
	}
	_, _, _, off, err := readQuestion(resp, 12)
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for i := 0; i < int(binary.BigEndian.Uint16(resp[6:8])); i++ {
		// name (pointer or labels), type, class, ttl, rdlength, rdata
		if off < len(resp) && resp[off]&0xC0 == 0xC0 {
			off += 2
		} else {
			for off < len(resp) && resp[off] != 0 {
				off += int(resp[off]) + 1
			}
			off++
		}
		if off+10 > len(resp) {
			return out, errors.New("localdns: short answer")
		}
		rtype := binary.BigEndian.Uint16(resp[off : off+2])
		rlen := int(binary.BigEndian.Uint16(resp[off+8 : off+10]))
		off += 10
		if off+rlen > len(resp) {
			return out, errors.New("localdns: short answer")
		}
		if rtype == qtype {
			out = append(out, net.IP(append([]byte(nil), resp[off:off+rlen]...)))
		}
		off += rlen
	}
	return out, nil
}

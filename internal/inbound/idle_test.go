package inbound_test

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/inbound"
)

// An idle keep-alive connection to 80 or 443 used to be held for good, and
// enough of them ran the process out of descriptors. One request answered,
// then nothing: the door closes it.
func TestTheDoorsCloseAnIdleKeepAliveConnection(t *testing.T) {
	// Put back by a cleanup registered before the house's, so after its
	// loop has stopped reading it.
	was := inbound.IdleTimeout
	t.Cleanup(func() { inbound.IdleTimeout = was })
	inbound.IdleTimeout = 300 * time.Millisecond
	h := newHouse(t, "a.test")
	plain, err := net.Dial("tcp", h.http)
	if err != nil {
		t.Fatal(err)
	}
	secure, err := tls.Dial("tcp", h.https, &tls.Config{InsecureSkipVerify: true, ServerName: "a.test", NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]net.Conn{"80": plain, "443": secure} {
		defer c.Close()
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: a.test\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(c)
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.Close {
			t.Fatalf("%s: the server closed after the answer, so this proves nothing", name)
		}
		// Well past the idle timeout, well short of the read deadline.
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := r.ReadByte(); err != io.EOF {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("%s: the idle connection is still open after 5s", name)
			}
		}
	}
}

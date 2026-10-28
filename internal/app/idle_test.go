package app

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// An idle keep-alive connection to the loopback listener (where a tunnel
// lands) used to be held for good; enough of them ran the process out of
// descriptors.
func TestTheListenerClosesAnIdleKeepAliveConnection(t *testing.T) {
	defer func(d time.Duration) { IdleTimeout = d }(IdleTimeout)
	IdleTimeout = 300 * time.Millisecond
	t.Setenv("MIDDLEWARE_LISTEN", "127.0.0.1:0")
	a, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	events, unsub := a.Bus.Subscribe()
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()
	defer func() { cancel(); <-ran }()
	var addr string
	for addr == "" {
		select {
		case ev := <-events:
			if ev.Type == "middleware.started" {
				addr, _ = ev.Data["listen"].(string)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the app never started listening")
		}
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET / HTTP/1.1\r\nHost: nobody.test\r\n\r\n")
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Close {
		t.Fatal("the server closed after the answer, so this proves nothing")
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadByte(); err != io.EOF {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("the idle connection is still open after 5s")
		}
	}
}

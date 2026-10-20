package app

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/events"
)

func TestAPanicGivesTheVisitorAPageAndTheOperatorAStack(t *testing.T) {
	bus := events.New("")
	h := recoverer(bus, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boom":
			var m map[string]int
			m["x"] = 1 // a real nil-map panic
		case "/half":
			w.Write([]byte("half a page"))
			panic("after writing")
		case "/gone":
			panic(http.ErrAbortHandler)
		case "/ws":
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\nhello"))
			c.Close()
		}
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatalf("the visitor got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || !strings.Contains(string(body), "Temporarily unavailable") {
		t.Fatalf("panic answered %d:\n%.300s", resp.StatusCode, body)
	}
	var ev *events.Event
	for _, e := range bus.Recent() {
		if e.Type == "middleware.panic" {
			e := e
			ev = &e
		}
	}
	if ev == nil || !strings.Contains(ev.Message, "/boom") || !strings.Contains(ev.Data["stack"].(string), "recover_test.go") {
		t.Fatalf("no event with the stack: %+v", ev)
	}
	// Half a response out: no page glued onto it, the connection ends.
	if resp, err := http.Get(srv.URL + "/half"); err == nil {
		b, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr == nil && strings.Contains(string(b), "unavailable") {
			t.Fatal("a page was glued onto half a response")
		}
	}
	// A client gone is not a panic to report.
	before := len(bus.Recent())
	http.Get(srv.URL + "/gone")
	for _, e := range bus.Recent()[before:] {
		if e.Type == "middleware.panic" {
			t.Fatal("ErrAbortHandler was reported as a panic")
		}
	}
	// Websockets still hijack through it.
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET /ws HTTP/1.1\r\nHost: x\r\n\r\n"))
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(line, "101") {
		t.Fatalf("hijack through the recoverer: %q", line)
	}
}

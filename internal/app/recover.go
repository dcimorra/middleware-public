package app

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/pages"
)

// recoverer turns a panic in any hook or in the proxy into the house's
// "unavailable" page and an event with the stack, instead of what
// net/http does on its own: log the stack and close the connection, so the
// visitor's browser shows its own "connection reset" and nothing says the
// fault was here. The process stays up either way; this is about what the
// visitor sees and what the operator can find.
//
// http.ErrAbortHandler is not a panic to report: it is how net/http says
// "this response is over" when the client went away mid-download, and it
// is passed on untouched.
func recoverer(bus *events.Bus, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &tracked{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			stack := string(debug.Stack())
			if len(stack) > 8000 {
				stack = stack[:8000]
			}
			bus.Emit(events.Event{Type: "middleware.panic", Subject: strings.ToLower(r.Host), Actor: "engine", Level: "error",
				Message: fmt.Sprintf("%v (%s %s)", v, r.Method, r.URL.Path), Data: map[string]any{"stack": stack}})
			if tw.wrote {
				// Half a response is already out; a page now would be
				// glued onto it. Ending the connection is all that is left.
				panic(http.ErrAbortHandler)
			}
			pages.Write(w, r, "unavailable", http.StatusInternalServerError)
		}()
		next.ServeHTTP(tw, r)
	})
}

// tracked remembers whether anything was written, and passes Flush and
// Unwrap through so streaming and http.ResponseController keep working.
type tracked struct {
	http.ResponseWriter
	wrote bool
}

func (t *tracked) WriteHeader(code int) { t.wrote = true; t.ResponseWriter.WriteHeader(code) }
func (t *tracked) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}
func (t *tracked) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		t.wrote = true
		f.Flush()
	}
}
func (t *tracked) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// Hijack is passed through for websockets: the traffic recorder below
// asks for it by type assertion, and without it every websocket broke.
func (t *tracked) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	t.wrote = true
	return h.Hijack()
}

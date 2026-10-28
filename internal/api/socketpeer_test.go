package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dcimorra/middleware-public/internal/chat"
)

// TestSocketPeerHelper is the other end of the socket in the tests below:
// a process of its own, as the middleware CLI run from an agent's Bash
// call is, so that the kernel names it, and not the test, as the peer.
// It sends each "METHOD path [body]" line of MW_PEER_CALLS and prints
// the status of each.
func TestSocketPeerHelper(t *testing.T) {
	sock := os.Getenv("MW_PEER_SOCK")
	if sock == "" {
		t.Skip("only run as the peer of another test")
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	for _, call := range strings.Split(os.Getenv("MW_PEER_CALLS"), "\n") {
		method, rest, _ := strings.Cut(call, " ")
		path, body, _ := strings.Cut(rest, " ")
		req, _ := http.NewRequest(method, "http://middleware"+path, strings.NewReader(body))
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("%s -> error %v\n", call, err)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%s -> %d %s\n", call, resp.StatusCode, bytes.TrimSpace(data))
	}
}

// peer runs the helper as a child of this test with env added, and
// returns each call's status.
func peer(t *testing.T, sock string, env []string, calls ...string) []int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSocketPeerHelper$", "-test.v")
	cmd.Env = append(withoutMark(os.Environ()), "MW_PEER_SOCK="+sock, "MW_PEER_CALLS="+strings.Join(calls, "\n"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("peer: %v\n%s", err, out)
	}
	var codes []int
	for _, call := range calls {
		i := bytes.Index(out, []byte(call+" -> "))
		if i < 0 {
			t.Fatalf("no answer to %q in\n%s", call, out)
		}
		var code int
		fmt.Sscanf(string(out[i+len(call)+4:]), "%d", &code)
		codes = append(codes, code)
	}
	return codes
}

func withoutMark(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, chat.MarkVar+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// A process whose ancestor is a site's agent turn reaches the socket as
// that site and nothing more: no token, no other site, not the house's
// own conversation; its own site reads. The house's turn stays the
// operator, and a peer that cannot be told is not trusted at all.
func TestASiteTurnOnTheSocketIsConfinedToItsSite(t *testing.T) {
	f := newFixture(t).withToken()
	for _, d := range []string{"a.example", "b.example"} {
		if resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": d}); resp.StatusCode != 201 {
			t.Fatalf("create %s: %d %v", d, resp.StatusCode, body)
		}
	}
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	// This test's own process is the "turn": the helper is its child, so
	// the turn is the helper's ancestor, as the agent CLI is of whatever
	// its Bash calls start.
	turns := map[int]string{os.Getpid(): "a.example"}
	unknown := false
	siteOf := func(pid int) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if unknown {
			return "", fmt.Errorf("cannot tell")
		}
		return chat.SiteOfProcess("/proc", pid, turns)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := SocketServer(ctx, f.srv.Panel(), siteOf)
	go srv.Serve(ln)
	defer srv.Close()

	calls := []string{
		`POST /api/v1/auth/tokens {"name":"mine"}`,
		"GET /api/v1/auth/tokens",
		"GET /api/v1/sites/b.example",
		"POST /api/v1/sites/b.example/restart",
		"GET /api/v1/sites",
		"GET /api/v1/settings",
		"GET /api/v1/events",
		`POST /api/v1/sites {"domain":"c.example"}`,
		"GET /api/v1/sites/a.example",
		"GET /api/v1/events?subject=a.example",
	}
	want := []int{403, 403, 403, 403, 403, 403, 403, 403, 200, 200}
	// The helper says it is the house's own conversation: a mark lifts
	// nothing, the turn above it still counts.
	got := peer(t, sock, []string{chat.MarkVar + "=middleware"}, calls...)
	for i := range calls {
		if got[i] != want[i] {
			t.Errorf("site turn: %s -> %d, want %d", calls[i], got[i], want[i])
		}
	}
	if toks, _ := f.store.Tokens(); len(toks) != 1 {
		t.Fatalf("a turn minted a token: %+v", toks)
	}

	// Its mark alone, with no turn known above it (a process detached
	// from its turn), confines it all the same.
	mu.Lock()
	turns = map[int]string{}
	mu.Unlock()
	if got := peer(t, sock, []string{chat.MarkVar + "=a.example#0123abcd"}, `POST /api/v1/auth/tokens {"name":"mine"}`, "GET /api/v1/sites/a.example"); got[0] != 403 || got[1] != 200 {
		t.Fatalf("a door turn's mark: %v", got)
	}

	// The house's conversation is the operator, as it is by design.
	mu.Lock()
	turns = map[int]string{os.Getpid(): chat.Middleware}
	mu.Unlock()
	if got := peer(t, sock, nil, `POST /api/v1/auth/tokens {"name":"house"}`, "GET /api/v1/sites/b.example"); got[0] != 200 || got[1] != 200 {
		t.Fatalf("the house's turn: %v", got)
	}

	// Who cannot be told gets what an anonymous caller gets.
	mu.Lock()
	unknown = true
	mu.Unlock()
	if got := peer(t, sock, nil, "GET /api/v1/sites/a.example"); got[0] != 401 {
		t.Fatalf("an unknown peer: %v", got)
	}
}

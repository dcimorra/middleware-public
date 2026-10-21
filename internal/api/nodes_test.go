package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/nodes"
)

func nodeFixture(t *testing.T, name string) *fixture {
	t.Helper()
	f := newFixture(t).withToken()
	st, err := nodes.Open(filepath.Join(f.root, "data", "nodes"), name)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.RegisterNodes(st)
	return f
}

// Two real servers: one makes a code, the other pastes it, and from then
// on the first's panel creates a site on the second and reads its events
// through the hop, as the panel does.
func TestTwoNodesLinkAndOneWorksOnTheOther(t *testing.T) {
	a, b := nodeFixture(t, "alpha"), nodeFixture(t, "beta")

	res, inv := b.call("POST", "/api/v1/nodes/invite", map[string]string{"url": b.http.URL})
	if res.StatusCode != 200 {
		t.Fatalf("invite: %d %s", res.StatusCode, text(inv))
	}
	code, _ := inv["code"].(string)
	res, linked := a.call("POST", "/api/v1/nodes", map[string]string{"code": code, "url": a.http.URL})
	if res.StatusCode != 200 || linked["name"] != "beta" {
		t.Fatalf("link: %d %s", res.StatusCode, text(linked))
	}
	betaID, _ := linked["id"].(string)

	// The code is spent.
	if res, out := a.call("POST", "/api/v1/nodes", map[string]string{"code": code, "url": a.http.URL}); res.StatusCode == 200 {
		t.Fatalf("a code worked twice: %s", text(out))
	}

	// Both sides know each other.
	_, bl := b.call("GET", "/api/v1/nodes", nil)
	if !strings.Contains(text(bl), `"name": "alpha"`) {
		t.Fatalf("beta does not know alpha: %s", text(bl))
	}

	// The stream of the other node, through this one, carries its events.
	req, _ := http.NewRequest("GET", a.http.URL+"/api/v1/nodes/"+betaID+"/api/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	stream, err := http.DefaultClient.Do(req)
	if err != nil || stream.StatusCode != 200 {
		t.Fatalf("stream: %v %v", err, stream)
	}
	defer stream.Body.Close()
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "made-on-beta") {
				got <- sc.Text()
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)

	res, site := a.call("POST", "/api/v1/nodes/"+betaID+"/api/sites", map[string]any{"domain": "made-on-beta.example", "template": "static", "publish": false})
	if res.StatusCode != 201 {
		t.Fatalf("create through the hop: %d %s", res.StatusCode, text(site))
	}
	if res, _ := b.call("GET", "/api/v1/sites/made-on-beta.example", nil); res.StatusCode != 200 {
		t.Fatal("the site is not on beta")
	}
	if res, _ := a.call("GET", "/api/v1/sites/made-on-beta.example", nil); res.StatusCode != 404 {
		t.Fatal("the site landed on alpha")
	}
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("beta's event did not come through the stream")
	}

	// The overview has both, with beta's sites counted.
	res, _ = a.call("GET", "/api/v1/nodes/overview", nil)
	if res.StatusCode != 200 {
		t.Fatal("overview")
	}
	var cards []NodeCard
	getJSON(t, a, "/api/v1/nodes/overview", &cards)
	if len(cards) != 2 || !cards[0].Self || !cards[1].OK {
		t.Fatalf("overview: %+v", cards)
	}

	// A node's token is not the operator's: no links, no credentials.
	peerTok := tokenOf(t, b, "alpha")
	for _, p := range []string{"/api/v1/auth/tokens", "/api/v1/nodes", "/api/v1/nodes/" + betaID + "/api/sites"} {
		req, _ := http.NewRequest("GET", a.http.URL+p, nil)
		req.Header.Set("Authorization", "Bearer "+peerTok)
		r, _ := http.DefaultClient.Do(req)
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("a node reached %s: %d", p, r.StatusCode)
		}
		r.Body.Close()
	}

	// Unlinking undoes both sides and revokes both tokens.
	if res, out := a.call("DELETE", "/api/v1/nodes/"+betaID, nil); res.StatusCode != 200 || out["remote_error"] != nil {
		t.Fatalf("unlink: %s", text(out))
	}
	_, bl = b.call("GET", "/api/v1/nodes", nil)
	if strings.Contains(text(bl), "alpha") {
		t.Fatalf("beta still knows alpha: %s", text(bl))
	}
	toks, _ := a.store.Tokens()
	for _, tk := range toks {
		if strings.HasPrefix(tk.Name, nodeTokenName) {
			t.Fatalf("a node token survived: %+v", tk)
		}
	}
}

func TestAWrongInvitationIsAFlat401(t *testing.T) {
	b := nodeFixture(t, "beta")
	b.token = ""
	res, _ := b.call("POST", "/api/v1/nodes/join", map[string]string{"secret": "nope", "id": "x", "name": "x", "url": "https://x.example", "token": "t"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("%d", res.StatusCode)
	}
}

// A link undone on the other side must not log the operator out here.
func TestARevokedLinkIsABadGatewayNotALogout(t *testing.T) {
	a, b := nodeFixture(t, "alpha"), nodeFixture(t, "beta")
	_, inv := b.call("POST", "/api/v1/nodes/invite", map[string]string{"url": b.http.URL})
	_, linked := a.call("POST", "/api/v1/nodes", map[string]string{"code": inv["code"].(string), "url": a.http.URL})
	id := linked["id"].(string)
	toks, _ := b.store.Tokens()
	for _, tk := range toks {
		if strings.HasPrefix(tk.Name, nodeTokenName) {
			_ = b.store.DeleteToken(tk.ID)
		}
	}
	res, out := a.call("GET", "/api/v1/nodes/"+id+"/api/sites", nil)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(text(out), "refused") {
		t.Fatalf("%d %s", res.StatusCode, text(out))
	}
}

func TestOnlyPrivateAddressesMayBePlainHTTP(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://panel.example": true, "http://10.77.0.1:8080": true, "http://127.0.0.1:1": true,
		"http://panel.example": false, "http://8.8.8.8": false, "ftp://x": false, "https://x.example/path": false,
	} {
		if got := nodes.CheckURL(u) == nil; got != ok {
			t.Errorf("%s: %v", u, got)
		}
	}
}

func tokenOf(t *testing.T, f *fixture, peer string) string {
	t.Helper()
	st := f.srv.nodes
	for _, p := range st.Peers() {
		if p.Name == peer {
			return p.Token
		}
	}
	t.Fatalf("no peer %s", peer)
	return ""
}

func getJSON(t *testing.T, f *fixture, path string, v any) {
	t.Helper()
	req, _ := http.NewRequest("GET", f.http.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

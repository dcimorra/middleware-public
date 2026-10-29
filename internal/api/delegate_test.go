package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// delegateClient is a delegate's browser: their cookie and the CSRF
// header, never the operator's token.
type delegateClient struct {
	f      *fixture
	cookie string
}

func (f *fixture) makeSite(domain string) {
	f.t.Helper()
	if res, out := f.call("POST", "/api/v1/sites", map[string]any{"domain": domain, "template": "static", "publish": false}); res.StatusCode != http.StatusCreated {
		f.t.Fatalf("create %s: %d %s", domain, res.StatusCode, text(out))
	}
}

func (f *fixture) makeUser(name string, grants ...auth.Grant) {
	f.t.Helper()
	res, out := f.call("POST", "/api/v1/users", map[string]any{"name": name, "password": "a-good-password", "grants": grants})
	if res.StatusCode != http.StatusCreated {
		f.t.Fatalf("create user: %d %s", res.StatusCode, text(out))
	}
}

func (f *fixture) login(name string) *delegateClient {
	f.t.Helper()
	token := f.token
	f.token = ""
	res, out := f.call("POST", "/api/v1/auth/login", map[string]string{"user": name, "password": "a-good-password"})
	f.token = token
	if res.StatusCode != 200 {
		f.t.Fatalf("login %s: %d %s", name, res.StatusCode, text(out))
	}
	for _, c := range res.Cookies() {
		if c.Name == auth.CookieName {
			return &delegateClient{f: f, cookie: auth.CookieName + "=" + c.Value}
		}
	}
	f.t.Fatal("no session cookie")
	return nil
}

func (d *delegateClient) call(method, path string, body any) (*http.Response, map[string]any) {
	d.f.t.Helper()
	token := d.f.token
	d.f.token = ""
	defer func() { d.f.token = token }()
	return d.f.call(method, path, body, "Cookie: "+d.cookie, CSRFHeader+": "+CSRFValue)
}

// passes is a route the gate lets through, whatever the handler then
// says of a fixture without an engine.
const passes = -1

// A delegate signs in with their name and sees their site and nothing
// else of the house: not another site, not the settings, not the users,
// not another site's events. What their grant lacks is refused by name.
func TestADelegateSeesTheirSiteAndNothingElse(t *testing.T) {
	f := newFixture(t).withToken()
	f.makeSite("mine.example")
	f.makeSite("theirs.example")
	f.makeUser("bob", auth.Grant{Site: "mine.example", Perms: []string{"operate"}})
	bob := f.login("bob")

	_, me := bob.call("GET", "/api/v1/auth/me", nil)
	user, _ := me["user"].(map[string]any)
	if me["authenticated"] != true || user["name"] != "bob" || strings.Contains(text(me), "setup_pending") {
		t.Fatalf("me: %s", text(me))
	}
	res, out := bob.call("GET", "/api/v1/sites", nil)
	if res.StatusCode != 200 || !strings.Contains(text(out), "mine.example") || strings.Contains(text(out), "theirs.example") {
		t.Fatalf("sites: %d %s", res.StatusCode, text(out))
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/v1/sites/mine.example", 200},
		{"POST", "/api/v1/sites/mine.example/restart", passes},
		{"GET", "/api/v1/sites/theirs.example", 404},
		{"POST", "/api/v1/sites/theirs.example/restart", 404},
		{"PUT", "/api/v1/sites/mine.example/file?path=index.html", 403},
		{"POST", "/api/v1/sites/mine.example/rename", 403},
		{"DELETE", "/api/v1/sites/mine.example", 403},
		{"GET", "/api/v1/sites/mine.example/download.zip", passes},
		{"POST", "/api/v1/sites/mine.example/deploy", 403},
		{"GET", "/api/v1/settings", 404},
		{"GET", "/api/v1/users", 404},
		{"GET", "/api/v1/tasks", 404},
		{"POST", "/api/v1/sites", 404},
		{"GET", "/api/v1/auth/tokens", 404},
	} {
		var body any
		if c.method != "GET" && c.method != "DELETE" {
			body = map[string]any{}
		}
		res, out := bob.call(c.method, c.path, body)
		if c.want == passes && res.StatusCode != 401 && res.StatusCode != 403 && res.StatusCode != 404 {
			continue
		}
		if res.StatusCode != c.want {
			t.Errorf("%s %s: %d, want %d: %s", c.method, c.path, res.StatusCode, c.want, text(out))
		}
	}

	f.bus.Emit(events.Event{Type: "deploy.done", Subject: "mine.example", Message: "mine-deployed"})
	f.bus.Emit(events.Event{Type: "deploy.done", Subject: "theirs.example", Message: "theirs-deployed"})
	f.bus.Emit(events.Event{Type: "move.done", Subject: "mine.example", Message: "moved-to-somewhere"})
	_, out = bob.call("GET", "/api/v1/events", nil)
	if b := text(out); !strings.Contains(b, "mine-deployed") || strings.Contains(b, "theirs-deployed") || strings.Contains(b, "moved-to-somewhere") {
		t.Fatalf("events: %s", b)
	}

	// Taken away, it is gone on the next request; the session stays.
	var users []auth.PublicUser
	getJSON(t, f, "/api/v1/users", &users)
	if res, out := f.call("PUT", "/api/v1/users/"+users[0].ID, map[string]any{"grants": []auth.Grant{}}); res.StatusCode != 200 {
		t.Fatalf("revoke: %d %s", res.StatusCode, text(out))
	}
	if res, _ := bob.call("GET", "/api/v1/sites/mine.example", nil); res.StatusCode != 404 {
		t.Fatalf("a revoked site still answers: %d", res.StatusCode)
	}
	// Deleted, the session is no one's.
	f.call("DELETE", "/api/v1/users/"+users[0].ID, nil)
	if res, _ := bob.call("GET", "/api/v1/sites", nil); res.StatusCode != 401 {
		t.Fatalf("a deleted user's session: %d", res.StatusCode)
	}
}

// A delegate with code writes their manifest but not the keys that reach
// past their site, through either door to it.
func TestADelegateCannotChangeWhatTheOwnerDecides(t *testing.T) {
	f := newFixture(t).withToken()
	f.makeSite("mine.example")
	f.makeUser("bob", auth.Grant{Site: "mine.example", Perms: []string{"operate", "code"}})
	bob := f.login("bob")

	if res, out := bob.call("PATCH", "/api/v1/sites/mine.example/manifest", map[string]any{"title": "Mine"}); res.StatusCode != 200 {
		t.Fatalf("title: %d %s", res.StatusCode, text(out))
	}
	if res, out := bob.call("PATCH", "/api/v1/sites/mine.example/manifest", map[string]any{"aliases": []string{"theirs.example"}}); res.StatusCode != 403 || !strings.Contains(text(out), "aliases") {
		t.Fatalf("aliases: %d %s", res.StatusCode, text(out))
	}
	_, site := bob.call("GET", "/api/v1/sites/mine.example", nil)
	mf, _ := json.Marshal(site["manifest"])
	var m map[string]any
	_ = json.Unmarshal(mf, &m)
	m["upstream"] = "http://127.0.0.1:46100"
	raw, _ := json.Marshal(m)
	if res, out := bob.call("PUT", "/api/v1/sites/mine.example/file?path=site.json", raw); res.StatusCode != 403 || !strings.Contains(text(out), "upstream") {
		t.Fatalf("site.json through the files: %d %s", res.StatusCode, text(out))
	}
	// The operator is not held to it.
	if res, out := f.call("PATCH", "/api/v1/sites/mine.example/manifest", map[string]any{"aliases": []string{"also.example"}}); res.StatusCode != 200 {
		t.Fatalf("operator: %d %s", res.StatusCode, text(out))
	}
}

// The case delegation is for: the delegate signs in at alpha, their site
// lives on gamma, whose panel is nowhere they can reach. Alpha relays
// every request through the mesh and nothing that comes back names
// gamma, its address or that there was a hop; gamma holds the delegate
// to the same lists, which is how the manifest's locked keys, that only
// gamma can read, are kept.
func TestADelegateWorksOnASiteOfAnotherNodeWithoutSeeingIt(t *testing.T) {
	port := meshPort(t)
	a, am := nodeFixture(t, "alpha", port)
	c, cm := nodeFixture(t, "gamma", port)
	_, inv := a.call("POST", "/api/v1/nodes/invite", map[string]string{"endpoint": "198.51.100.1:51821"})
	if res, out := c.call("POST", "/api/v1/nodes/join", map[string]string{"code": inv["code"].(string)}); res.StatusCode != 200 {
		t.Fatalf("join: %d %s", res.StatusCode, text(out))
	}
	am.Gossip(context.Background())
	// Every node of a network runs the same release, so the same routes:
	// alpha needs them to know what it relays.
	for _, f := range []*fixture{a, c} {
		f.srv.RegisterTasks(TasksOptions{Scheduler: tasks.NewScheduler(tasks.Options{Root: f.root, Bus: f.bus}), Root: f.root})
	}
	gm, _ := am.Member(cm.Self().ID)
	c.makeSite("far.example")
	c.makeSite("other.example")
	a.makeSite("near.example")
	a.makeUser("bob", auth.Grant{Site: "far.example", Perms: []string{"operate", "code"}}, auth.Grant{Site: "near.example"})
	bob := a.login("bob")

	leaks := func(where, body string) {
		t.Helper()
		for _, s := range []string{"gamma", cm.Self().ID, gm.IP.String(), "node"} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(s)) {
				t.Fatalf("%s names %q: %s", where, s, body)
			}
		}
	}
	res, out := bob.call("GET", "/api/v1/sites", nil)
	if res.StatusCode != 200 || !strings.Contains(text(out), "far.example") || !strings.Contains(text(out), "near.example") || strings.Contains(text(out), "other.example") {
		t.Fatalf("sites: %d %s", res.StatusCode, text(out))
	}
	res, out = bob.call("GET", "/api/v1/sites/far.example", nil)
	if res.StatusCode != 200 || out["domain"] != "far.example" {
		t.Fatalf("far site: %d %s", res.StatusCode, text(out))
	}
	leaks("the site", text(out))
	if res, out := bob.call("GET", "/api/v1/sites/other.example", nil); res.StatusCode != 404 {
		t.Fatalf("a site of gamma's not granted: %d %s", res.StatusCode, text(out))
	}

	// The live stream at alpha carries what happens to the site on gamma.
	req, _ := http.NewRequest("GET", a.http.URL+"/api/v1/events/stream", nil)
	req.Header.Set("Cookie", bob.cookie)
	stream, err := http.DefaultClient.Do(req)
	if err != nil || stream.StatusCode != 200 {
		t.Fatalf("stream: %v %v", err, stream)
	}
	defer stream.Body.Close()
	got := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				got <- sc.Text()
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	c.bus.Emit(events.Event{Type: "move.done", Subject: "far.example", Message: "moved-from-gamma"})
	c.bus.Emit(events.Event{Type: "deploy.done", Subject: "other.example", Message: "not-bobs"})
	c.bus.Emit(events.Event{Type: "deploy.done", Subject: "far.example", Message: "far-deployed"})
	select {
	case line := <-got:
		if !strings.Contains(line, "far-deployed") {
			t.Fatalf("the stream carried %s", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gamma's event did not reach the delegate at alpha")
	}

	// Writing goes through, signed by the delegate on gamma; a locked key
	// is refused by gamma, in words that do not name it.
	if res, out := bob.call("PUT", "/api/v1/sites/far.example/tasks/nightly", map[string]any{"version": 2, "schedule": "0 3 * * *", "command": []string{"true"}}); res.StatusCode != 200 {
		t.Fatalf("task on gamma: %d %s", res.StatusCode, text(out))
	}
	if res, out := bob.call("PATCH", "/api/v1/sites/far.example/manifest", map[string]any{"upstream": "http://127.0.0.1:1"}); res.StatusCode != 403 {
		t.Fatalf("locked key on gamma: %d %s", res.StatusCode, text(out))
	} else {
		leaks("the refusal", text(out))
	}
	var evs []events.Event
	getJSON(t, c, "/api/v1/events?type=task.saved", &evs)
	if len(evs) == 0 || evs[len(evs)-1].Actor != "user:bob" {
		t.Fatalf("gamma's audit trail: %+v", evs)
	}

	// Gamma gone: the site cannot be reached, and that is all it says.
	c.http.Close()
	_ = cm.Leave(context.Background())
	res, out = bob.call("GET", "/api/v1/sites/far.example", nil)
	if res.StatusCode == 200 {
		t.Fatalf("gamma left and the site still answers: %s", text(out))
	}
	leaks("the failure", text(out))
}

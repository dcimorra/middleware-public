package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/auth"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/sites"
)

type fixture struct {
	t     *testing.T
	root  string
	srv   *Server
	store *auth.Store
	bus   *events.Bus
	http  *httptest.Server
	token string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites"), 0o755)
	bus := events.New("")
	store := auth.New(root, bus)
	srv := New(Options{Root: root, Version: "test", Auth: store, Sites: sites.New(root, nil, bus), Bus: bus, Restart: func() {}})
	srv.Heartbeat = 100 * time.Millisecond
	ts := httptest.NewServer(srv.Panel())
	t.Cleanup(ts.Close)
	return &fixture{t: t, root: root, srv: srv, store: store, bus: bus, http: ts}
}

// call makes a request; headers come as "Key: value" strings.
func (f *fixture) call(method, path string, body any, headers ...string) (*http.Response, map[string]any) {
	f.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, f.http.URL+path, rd)
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	for _, h := range headers {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out map[string]any
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") && len(data) > 0 && data[0] == '{' {
		_ = json.Unmarshal(data, &out)
	}
	if out == nil {
		out = map[string]any{}
	}
	out["_body"] = string(data)
	return resp, out
}

// text returns the raw body a call kept for non-object answers.
func text(body map[string]any) string {
	s, _ := body["_body"].(string)
	return s
}

// withToken sets the operator up and mints a token for the rest of the
// test, so sites tests do not repeat the login dance.
func (f *fixture) withToken() *fixture {
	f.t.Helper()
	if err := f.store.SetPassword("password-1"); err != nil {
		f.t.Fatal(err)
	}
	_, secret, err := f.store.CreateToken("test")
	if err != nil {
		f.t.Fatal(err)
	}
	f.token = secret
	return f
}

// TestMain lowers the password hash cost: the flow is what is under test
// here, the hash strength is internal/auth's business.
func TestMain(m *testing.M) {
	auth.SetTestCost(1000)
	os.Exit(m.Run())
}

func TestTheWholeAuthFlowFromSetupCodeToTokenUse(t *testing.T) {
	f := newFixture(t)

	resp, _ := f.call("GET", "/api/v1/sites", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous list: %d", resp.StatusCode)
	}
	resp, me := f.call("GET", "/api/v1/auth/me", nil)
	if resp.StatusCode != 200 || me["authenticated"] != false || me["setup_pending"] != true {
		t.Fatalf("me before setup: %d %v", resp.StatusCode, me)
	}
	resp, _ = f.call("POST", "/api/v1/auth/login", map[string]string{"password": "password-1"})
	if resp.StatusCode != 403 {
		t.Fatalf("login before setup: %d", resp.StatusCode)
	}

	code, _ := f.store.CreateSetupCode()
	resp, body := f.call("POST", "/api/v1/auth/setup", map[string]string{"code": "wrong", "password": "password-1"})
	if resp.StatusCode != 401 {
		t.Fatalf("wrong code: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/auth/setup", map[string]string{"code": code, "password": "password-1"})
	if resp.StatusCode != 200 || body["ok"] != true {
		t.Fatalf("setup: %d %v", resp.StatusCode, body)
	}

	resp, body = f.call("POST", "/api/v1/auth/login", map[string]string{"password": "nope"})
	if resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("POST", "/api/v1/auth/login", map[string]string{"password": "password-1"})
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatalf("no session cookie: %v", resp.Cookies())
	}
	ck := "Cookie: " + auth.CookieName + "=" + cookie.Value

	resp, me = f.call("GET", "/api/v1/auth/me", nil, ck)
	if me["authenticated"] != true || me["via"] != "cookie" {
		t.Fatalf("me with cookie: %v", me)
	}
	resp, body = f.call("POST", "/api/v1/auth/tokens", map[string]string{"name": "ci"}, ck)
	if resp.StatusCode != 403 {
		t.Fatalf("cookie POST without the CSRF header: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/auth/tokens", map[string]string{"name": "ci"}, ck, CSRFHeader+": "+CSRFValue)
	if resp.StatusCode != 200 || body["token"] == nil {
		t.Fatalf("token create: %d %v", resp.StatusCode, body)
	}
	secret := body["token"].(string)
	id := body["id"].(string)

	f.token = secret
	resp, me = f.call("GET", "/api/v1/auth/me", nil)
	if me["via"] != "token" || me["token"].(map[string]any)["name"] != "ci" {
		t.Fatalf("me with token: %v", me)
	}
	resp, _ = f.call("GET", "/api/v1/sites", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("token list: %d", resp.StatusCode)
	}
	resp, body = f.call("GET", "/api/v1/auth/tokens", nil)
	if resp.StatusCode != 200 || !strings.Contains(text(body), `"name": "ci"`) || strings.Contains(text(body), secret) {
		t.Fatalf("token list: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("DELETE", "/api/v1/auth/tokens/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("token delete: %d", resp.StatusCode)
	}
	resp, _ = f.call("GET", "/api/v1/sites", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("revoked token still works: %d", resp.StatusCode)
	}
	f.token = ""
	resp, _ = f.call("POST", "/api/v1/auth/logout", nil, ck, CSRFHeader+": "+CSRFValue)
	if resp.StatusCode != 200 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	_, me = f.call("GET", "/api/v1/auth/me", nil, ck)
	if me["authenticated"] != false {
		t.Fatal("logged out cookie still valid")
	}
}

func TestSixWrongPasswordsGetA429(t *testing.T) {
	f := newFixture(t)
	f.store.SetPassword("password-1")
	for i := 0; i < 5; i++ {
		f.call("POST", "/api/v1/auth/login", map[string]string{"password": "nope"}, "X-Forwarded-For: 203.0.113.7")
	}
	resp, _ := f.call("POST", "/api/v1/auth/login", map[string]string{"password": "password-1"}, "X-Forwarded-For: 203.0.113.7")
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d", resp.StatusCode)
	}
	resp, _ = f.call("POST", "/api/v1/auth/login", map[string]string{"password": "password-1"}, "X-Forwarded-For: 203.0.113.8")
	if resp.StatusCode != 200 {
		t.Fatalf("another address: %d", resp.StatusCode)
	}
}

func TestSitesCRUDOnATempRoot(t *testing.T) {
	f := newFixture(t).withToken()
	resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": "Hello.Test", "template": "node"})
	if resp.StatusCode != 201 || body["domain"] != "hello.test" || body["mode"] != "run" || body["managed"] != true {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/sites", map[string]any{"domain": "hello.test", "template": "node"})
	if resp.StatusCode != 409 {
		t.Fatalf("duplicate: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/sites", map[string]any{"domain": "bad_name", "template": "node"})
	if resp.StatusCode != 400 {
		t.Fatalf("bad domain: %d %v", resp.StatusCode, body)
	}
	// A name without a dot is a project: it gets no process to serve with.
	resp, body = f.call("POST", "/api/v1/sites", map[string]any{"domain": "tools", "template": "node"})
	if resp.StatusCode != 400 {
		t.Fatalf("a project with a run section: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/sites", map[string]any{"domain": "tools"})
	if resp.StatusCode != 201 {
		t.Fatalf("a project: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/sites", map[string]any{"domain": "x.test", "manifest": map[string]any{"version": 2, "comand": []string{}}})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown manifest key: %d %v", resp.StatusCode, body)
	}

	resp, body = f.call("GET", "/api/v1/sites", nil)
	if resp.StatusCode != 200 || !strings.Contains(text(body), `"hello.test"`) {
		t.Fatalf("list: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/sites/hello.test", nil)
	if resp.StatusCode != 200 || body["manifest"] == nil {
		t.Fatalf("get: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("GET", "/api/v1/sites/nope.test", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("missing: %d", resp.StatusCode)
	}

	resp, body = f.call("PATCH", "/api/v1/sites/hello.test/manifest", `{"headers":{"X-Frame-Options":"DENY"}}`)
	if resp.StatusCode != 200 || body["manifest"].(map[string]any)["headers"].(map[string]any)["X-Frame-Options"] != "DENY" {
		t.Fatalf("patch: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("PUT", "/api/v1/sites/hello.test/manifest", `{"version":2,"static":{"root":"public"}}`)
	if resp.StatusCode != 200 || body["mode"] != "static" {
		t.Fatalf("put: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("PUT", "/api/v1/sites/hello.test/manifest", `{"version":1}`)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid put: %d %v", resp.StatusCode, body)
	}

	resp, body = f.call("POST", "/api/v1/sites/hello.test/stop", nil)
	if resp.StatusCode != 200 || body["mode"] != "disabled" {
		t.Fatalf("stop: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/sites/hello.test/start", nil)
	if resp.StatusCode != 200 || body["mode"] != "static" {
		t.Fatalf("start: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/sites/hello.test/hold", `{"enabled":true,"title":"Soon"}`)
	if resp.StatusCode != 200 || body["mode"] != "hold" {
		t.Fatalf("hold: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/sites/hello.test/env", nil)
	if resp.StatusCode != 200 || body["resolved"] == nil {
		t.Fatalf("env: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/sites/hello.test/logs?lines=5", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("logs: %d %v", resp.StatusCode, body)
	}

	resp, body = f.call("DELETE", "/api/v1/sites/hello.test?archive=true", nil)
	if resp.StatusCode != 200 || body["archived"] != true {
		t.Fatalf("archive: %d %v", resp.StatusCode, body)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, "data", "archive")); len(entries) != 1 {
		t.Fatalf("archive folder: %v", entries)
	}
	resp, _ = f.call("GET", "/api/v1/sites/hello.test", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("after archive: %d", resp.StatusCode)
	}
}

func TestFilesStayInsideTheSiteFolder(t *testing.T) {
	f := newFixture(t).withToken()
	f.call("POST", "/api/v1/sites", map[string]any{"domain": "a.test", "template": "static"})
	os.WriteFile(filepath.Join(f.root, "outside.txt"), []byte("secret"), 0o644)
	os.Symlink(filepath.Join(f.root, "outside.txt"), filepath.Join(f.root, "sites", "a.test", "link.txt"))

	for _, p := range []string{"../outside.txt", "link.txt", "public/../../outside.txt"} {
		resp, body := f.call("GET", "/api/v1/sites/a.test/file?path="+p, nil)
		if resp.StatusCode != 403 {
			t.Fatalf("read %s: %d %v", p, resp.StatusCode, body)
		}
		resp, body = f.call("PUT", "/api/v1/sites/a.test/file?path="+p, "pwned")
		if resp.StatusCode != 403 {
			t.Fatalf("write %s: %d %v", p, resp.StatusCode, body)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(f.root, "outside.txt")); string(data) != "secret" {
		t.Fatal("the outside file was changed")
	}
	resp, body := f.call("PUT", "/api/v1/sites/a.test/file?path=public/new.txt", "hello")
	if resp.StatusCode != 200 {
		t.Fatalf("write: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/sites/a.test/file?path=public/new.txt", nil)
	if resp.StatusCode != 200 || text(body) != "hello" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("read: %d %v %s", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
	resp, body = f.call("GET", "/api/v1/sites/a.test/files?path=/public", nil)
	if resp.StatusCode != 200 || !strings.Contains(text(body), `"new.txt"`) {
		t.Fatalf("list: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("GET", "/api/v1/sites/a.test/file", nil)
	if resp.StatusCode != 400 {
		t.Fatalf("no path: %d", resp.StatusCode)
	}
}

func TestTheStreamDeliversAnEventAndCatchesUpWithSince(t *testing.T) {
	f := newFixture(t).withToken()
	f.bus.Emit(events.Event{Type: "old.event", Subject: "a.test"})
	time.Sleep(10 * time.Millisecond)
	since := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", f.http.URL+"/api/v1/events/stream?since="+since, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %s", resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	wait := func(prefix string) string {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream closed waiting for %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-deadline:
				t.Fatalf("no line starting with %q", prefix)
			}
		}
	}
	wait("event: old.event")
	f.bus.Emit(events.Event{Type: "live.event", Subject: "b.test", Message: "hello"})
	if l := wait("event: "); l != "event: live.event" {
		t.Fatalf("live: %s", l)
	}
	if l := wait("data: "); !strings.Contains(l, `"message":"hello"`) {
		t.Fatalf("data: %s", l)
	}
	wait(": ping")

	resp2, body := f.call("GET", "/api/v1/events?type=live.", nil)
	if resp2.StatusCode != 200 || !strings.Contains(text(body), "live.event") || strings.Contains(text(body), "old.event") {
		t.Fatalf("events list: %d %v", resp2.StatusCode, body)
	}
}

func TestSystemAndOpenAPIDescribeTheInstallation(t *testing.T) {
	f := newFixture(t).withToken()
	resp, body := f.call("GET", "/api/v1/system", nil)
	if resp.StatusCode != 200 || body["go"] == nil || body["load"] == nil || body["memory"] == nil || body["disk"] == nil || body["uptime"] == nil {
		t.Fatalf("system: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/system/ports", nil)
	if resp.StatusCode != 200 || body["from"].(float64) != 33000 {
		t.Fatalf("ports: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/system/processes", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("processes: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/api/v1/system/restart", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("restart: %d %v", resp.StatusCode, body)
	}
	f.token = ""
	resp, body = f.call("GET", "/api/v1/openapi.json", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("openapi: %d %v", resp.StatusCode, body)
	}
	paths := body["paths"].(map[string]any)
	op := paths["/api/v1/sites/{domain}"].(map[string]any)["get"].(map[string]any)
	if op["summary"] != "One site" || op["operationId"] != "getSitesDomain" {
		t.Fatalf("openapi op: %v", op)
	}
	if _, ok := paths["/api/v1/events/stream"]; !ok {
		t.Fatal("stream route missing from openapi")
	}
	resp, body = f.call("GET", "/", nil)
	if resp.StatusCode != 200 || !strings.Contains(text(body), "middleware api") {
		t.Fatalf("ui fallback: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("GET", "/api/v1/nothing", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown route: %d", resp.StatusCode)
	}
}

func TestMountedHandlersSitBehindAuthentication(t *testing.T) {
	f := newFixture(t)
	f.srv.Mount("/api/v1/modules/demo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]string{"caller": string(auth.FromContext(r.Context()).Via), "path": r.URL.Path})
	}))
	f.srv.Handle("GET", "/api/v1/demo/{id}", "Demo", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if r.PathValue("id") == "boom" {
			return nil, Errorf(418, "teapot")
		}
		return map[string]string{"id": r.PathValue("id")}, nil
	})
	resp, _ := f.call("GET", "/api/v1/modules/demo/state", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous mounted: %d", resp.StatusCode)
	}
	f.withToken()
	resp, body := f.call("GET", "/api/v1/modules/demo/state", nil)
	if resp.StatusCode != 200 || body["caller"] != "token" || body["path"] != "/api/v1/modules/demo/state" {
		t.Fatalf("mounted: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/demo/boom", nil)
	if resp.StatusCode != 418 || body["error"] != "teapot" {
		t.Fatalf("typed error: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("GET", "/api/v1/demo/x", nil)
	if resp.StatusCode != 200 || body["id"] != "x" {
		t.Fatalf("handle: %d %v", resp.StatusCode, body)
	}
	found := false
	for _, rt := range f.srv.Routes() {
		if rt.Mounted && rt.Path == "/api/v1/modules/demo/…" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mount not listed: %+v", f.srv.Routes())
	}
}

func TestTheUnixSocketIsTrustedWithoutCredentials(t *testing.T) {
	f := newFixture(t)
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(sock); st.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode %o", st.Mode().Perm())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &http.Server{Handler: f.srv.Panel(), BaseContext: func(net.Listener) context.Context { return auth.WithSocket(ctx) }}
	go srv.Serve(ln)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", sock)
	}}}
	resp, err := client.Get("http://middleware/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	var me map[string]any
	json.NewDecoder(resp.Body).Decode(&me)
	if me["authenticated"] != true || me["via"] != "socket" {
		t.Fatalf("socket me: %v", me)
	}
	resp, _ = client.Get("http://middleware/api/v1/sites")
	if resp.StatusCode != 200 {
		t.Fatalf("socket list: %d", resp.StatusCode)
	}
	if _, err := ListenSocket(sock); err == nil {
		t.Fatal("a live socket was replaced")
	}
}

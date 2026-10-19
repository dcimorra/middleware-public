package inbound_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/acme"
	"github.com/dcimorra/middleware-public/internal/acme/acmetest"
	"github.com/dcimorra/middleware-public/internal/certs"
	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/inbound"
)

type house struct {
	root  string
	ca    *acmetest.Server
	store *certs.Store
	hook  *inbound.Hook
	bus   *events.Bus
	http  string
	https string
}

// newHouse opens the ports door on two free loopback ports with a fake CA
// that validates HTTP-01 by knocking on the real port-80 listener.
func newHouse(t *testing.T, sites ...string) *house {
	t.Helper()
	root := t.TempDir()
	ca := acmetest.New(t)
	solver := acme.NewHTTP01()
	store := certs.New(filepath.Join(root, "data", "certs"), &certs.ACMEIssuer{
		Dir: filepath.Join(root, "data", "certs"), Directory: ca.URL, HTTP01: solver,
	})
	isSite := func(d string) bool {
		if d == "panel.test" {
			return true
		}
		for _, s := range sites {
			if s == d {
				return true
			}
		}
		return false
	}
	store.Allowed = isSite
	bus := events.New("")
	store.Bus = bus
	st := inbound.DefaultState()
	st.Ports.Open = true
	st.Ports.HTTP = "127.0.0.1:0"
	st.Ports.HTTPS = "127.0.0.1:0"
	if err := inbound.SaveState(root, st); err != nil {
		t.Fatal(err)
	}
	hook := inbound.New(root, store, solver)
	hook.PanelDomain = "panel.test"
	hook.Sites = func() []string { return sites }
	hook.Bus = bus
	hook.Tick = 50 * time.Millisecond
	// The app handler, as the app would build it: the hook wrapped around
	// a site that echoes what it was told.
	hook.Handler = hook.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "site="+r.Host+" proto="+r.Header.Get("X-Forwarded-Proto"))
	}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := hook.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	h := &house{root: root, ca: ca, store: store, hook: hook, bus: bus}
	h.http, h.https = hook.Addrs()
	if h.http == "" || h.https == "" {
		t.Fatalf("ports door not open: %+v", hook.Status())
	}
	ca.ValidateAddr = h.http
	return h
}

// client talks to the house's 443 for whatever host the URL names.
func (h *house) client(pool bool) *http.Client {
	cfg := &tls.Config{InsecureSkipVerify: !pool}
	if pool {
		cfg.RootCAs = h.ca.Pool()
	}
	return &http.Client{
		Timeout: 40 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   cfg,
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, h.https)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func eventTypes(bus *events.Bus) []string {
	var out []string
	for _, e := range bus.Recent() {
		out = append(out, e.Type)
	}
	return out
}

func TestTheHTTPDoorAnswersTheChallengeAndRedirectsEverythingElse(t *testing.T) {
	h := newHouse(t, "a.test")
	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", "http://"+h.http+"/some/path?q=1", nil)
	req.Host = "a.test"
	resp, err := plain.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://a.test/some/path?q=1" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	h.hook.Solver.Present(context.Background(), "a.test", "tok", "tok.thumb")
	resp, body := get(t, plain, "http://"+h.http+acme.ChallengePath+"tok")
	if resp.StatusCode != 200 || body != "tok.thumb" {
		t.Fatalf("challenge: %d %q", resp.StatusCode, body)
	}
}

func TestTheHTTPSDoorIssuesOnFirstVisitAndSpeaksHTTP2(t *testing.T) {
	h := newHouse(t, "a.test")
	resp, body := get(t, h.client(true), "https://a.test/hello")
	if resp.StatusCode != 200 || body != "site=a.test proto=https" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated %s, want HTTP/2", resp.Proto)
	}
	if h.ca.Issued() != 1 || !h.store.Has("a.test") {
		t.Fatalf("issued %d, has %v", h.ca.Issued(), h.store.Has("a.test"))
	}
	st := h.hook.Status()
	if len(st.Domains) != 2 || st.Domains[0].Domain != "a.test" || st.Domains[0].Door != inbound.Ports || st.Domains[0].CertExpires == nil {
		t.Fatalf("status %+v", st.Domains)
	}
	if st.Domains[1].Domain != "panel.test" || st.Domains[1].Door != inbound.Tunnel || st.Domains[1].CertExpires != nil {
		t.Fatalf("panel row %+v", st.Domains[1])
	}
	if !st.Doors.Ports.Listening || st.Doors.Ports.Error != "" {
		t.Fatalf("doors %+v", st.Doors)
	}
}

func TestANameWithoutACertificateGetsTheNoCertPageNotTheSite(t *testing.T) {
	h := newHouse(t, "a.test")
	resp, body := get(t, h.client(false), "https://stranger.test/")
	if resp.StatusCode != 503 || !strings.Contains(body, "No certificate yet") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if h.ca.Issued() != 0 {
		t.Fatal("a stranger made us order a certificate")
	}
}

func TestAClosedTunnelShowsTheDoorClosedPageButThePanelAndTheCAStillGetThrough(t *testing.T) {
	h := newHouse(t, "a.test")
	if _, err := inbound.SetDoor(h.root, inbound.Tunnel, false); err != nil {
		t.Fatal(err)
	}
	// Requests with no door in their context are the loopback listener.
	do := func(host, path string) (int, string) {
		r := httptest.NewRequest("GET", "http://"+host+path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.hook.Handler.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := do("a.test", "/"); code != 503 || !strings.Contains(body, "This entrance is closed") {
		t.Fatalf("site through closed tunnel: %d %q", code, body)
	}
	if code, body := do("panel.test", "/"); code != 200 || body != "site=panel.test proto=http" {
		t.Fatalf("panel through closed tunnel: %d %q", code, body)
	}
	h.hook.Solver.Present(context.Background(), "a.test", "tok", "tok.thumb")
	if code, body := do("a.test", acme.ChallengePath+"tok"); code != 200 || body != "tok.thumb" {
		t.Fatalf("challenge through closed tunnel: %d %q", code, body)
	}
	// A tunnel that set the scheme keeps it.
	r := httptest.NewRequest("GET", "http://panel.test/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.hook.Handler.ServeHTTP(w, r)
	if w.Body.String() != "site=panel.test proto=https" {
		t.Fatalf("%q", w.Body.String())
	}
	// Reopening from the file is seen without a restart.
	inbound.SetDoor(h.root, inbound.Tunnel, true)
	if code, _ := do("a.test", "/"); code != 200 {
		t.Fatalf("after reopening: %d", code)
	}
}

func TestClosingThePortsDoorInTheFileStopsTheListeners(t *testing.T) {
	h := newHouse(t, "a.test")
	if _, err := inbound.SetDoor(h.root, inbound.Ports, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if a, b := h.hook.Addrs(); a == "" && b == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listeners still up after the file closed the door")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := net.DialTimeout("tcp", h.https, time.Second); err == nil {
		t.Fatal("443 still accepts")
	}
	if st := h.hook.Status(); st.Doors.Ports.Open || st.Doors.Ports.Listening {
		t.Fatalf("%+v", st.Doors)
	}
	if err := h.hook.SetDoor(inbound.Ports, true); err != nil {
		t.Fatal(err)
	}
	if a, b := h.hook.Addrs(); a == "" || b == "" {
		t.Fatal("not reopened")
	}
	got := strings.Join(eventTypes(h.bus), " ")
	for _, want := range []string{"inbound.listening", "inbound.stopped", "inbound.opened"} {
		if !strings.Contains(got, want) {
			t.Fatalf("events %q lack %s", got, want)
		}
	}
	st, _ := inbound.LoadState(h.root)
	if !st.Ports.Open {
		t.Fatal("SetDoor did not persist")
	}
}

func TestAPortHeldBySomeoneElseIsReportedOnceNotEveryTick(t *testing.T) {
	h := newHouse(t, "a.test")
	taken, _ := net.Listen("tcp", "127.0.0.1:0")
	defer taken.Close()
	st, _ := inbound.LoadState(h.root)
	st.Ports.HTTP = taken.Addr().String()
	inbound.SaveState(h.root, st)
	deadline := time.Now().Add(5 * time.Second)
	for h.hook.Status().Doors.Ports.Error == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // a few ticks more
	failed := 0
	for _, e := range h.bus.Recent() {
		if e.Type == "inbound.failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("inbound.failed emitted %d times", failed)
	}
	if s := h.hook.Status(); s.Doors.Ports.Listening || !strings.Contains(s.Doors.Ports.Error, "address already in use") {
		t.Fatalf("%+v", s.Doors)
	}
}

func TestAMoveToPortsObtainsTheCertificateFirstAndOnlyThenMarks(t *testing.T) {
	h := newHouse(t, "a.test", "b.test")
	h.ca.SetRateLimited(true)
	lines := h.hook.Move(context.Background(), []string{"a.test"}, inbound.Ports)
	if len(lines) != 1 || lines[0].Status != "fail" || !strings.Contains(lines[0].Text, "rateLimited") {
		t.Fatalf("%+v", lines)
	}
	if st, _ := inbound.LoadState(h.root); st.DoorOf("a.test") != "" {
		t.Fatal("marked without a certificate")
	}
	h.ca.SetRateLimited(false)
	h.store.Backoff = 0
	lines = h.hook.Move(context.Background(), []string{"a.test", "nobody.test", "bad domain"}, inbound.Ports)
	want := []string{"ok", "ok", "fail", "fail"}
	for i, l := range lines {
		if l.Status != want[i] {
			t.Fatalf("line %d: %+v", i, lines)
		}
	}
	st, _ := inbound.LoadState(h.root)
	if st.DoorOf("a.test") != inbound.Ports || !h.store.Has("a.test") {
		t.Fatalf("state %+v", st.Domains)
	}
	// Back to the tunnel: marked, certificate kept for the way back.
	lines = h.hook.Move(context.Background(), []string{"a.test"}, inbound.Tunnel)
	if len(lines) != 1 || lines[0].Status != "ok" {
		t.Fatalf("%+v", lines)
	}
	st, _ = inbound.LoadState(h.root)
	if st.DoorOf("a.test") != inbound.Tunnel || !h.store.Has("a.test") {
		t.Fatalf("state %+v has %v", st.Domains, h.store.Has("a.test"))
	}
	row := h.hook.Status().Domains[0]
	if row.Domain != "a.test" || row.Door != inbound.Tunnel || row.CertExpires == nil {
		t.Fatalf("%+v", row)
	}
	moved := 0
	for _, e := range h.bus.Recent() {
		if e.Type == "inbound.moved" {
			moved++
		}
	}
	if moved != 2 {
		t.Fatalf("inbound.moved emitted %d times", moved)
	}
}

func TestTheStateFileDefaultsAndRefusesWhatCannotListen(t *testing.T) {
	root := t.TempDir()
	st, err := inbound.LoadState(root)
	if err != nil || !st.Tunnel.Open || st.Ports.Open || st.Ports.HTTP != ":80" || st.Ports.HTTPS != ":443" {
		t.Fatalf("%+v %v", st, err)
	}
	st.Ports.HTTPS = ":80"
	if err := inbound.SaveState(root, st); err == nil {
		t.Fatal("same address for both accepted")
	}
	st.Ports.HTTPS = ":443"
	st.Domains = map[string]inbound.Door{"a.test": "garage"}
	if err := inbound.SaveState(root, st); err == nil {
		t.Fatal("unknown door accepted")
	}
	if _, err := inbound.ParseDoor("garage"); err == nil {
		t.Fatal("garage is not a door")
	}
	os.MkdirAll(filepath.Dir(inbound.Path(root)), 0o755)
	os.WriteFile(inbound.Path(root), []byte("{not json"), 0o644)
	if _, err := inbound.LoadState(root); err == nil {
		t.Fatal("broken file read as fine")
	}
}

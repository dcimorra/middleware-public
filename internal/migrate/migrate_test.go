package migrate

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/access"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// v1Hash writes a password the way V1's panel did.
func v1Hash(t *testing.T, pw string) string {
	salt := []byte("0123456789abcdef")
	key, err := pbkdf2.Key(sha256.New, pw, salt, 1000, 32)
	if err != nil {
		t.Fatal(err)
	}
	return "pbkdf2$1000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
}

func TestAProxySiteKeepsEverythingItHad(t *testing.T) {
	in := `{
	  "title": "Shop", "kind": "proxy", "aliases": ["shop.example.org"],
	  "command": ["node", "server.js"], "dir": "app", "port": 36666,
	  "env": {"X_PORT": "$PORT"}, "env_files": [".env", ".env.local"],
	  "env_from_file": {"NEXT_DIST_DIR": ".next-current"},
	  "health": {"path": "/up", "interval": "30s", "timeout": "8s"},
	  "auth": {"hash": "` + v1Hash(t, "site pw") + `", "message": "ask Ana"},
	  "ip": {"deny": ["203.0.113.0/24"]},
	  "routes": [{"path": "/admin", "note": "back office", "auth": {"password": "zone pw"}, "ip": {"allow": ["10.0.0.0/8"]}}],
	  "workers": [{"name": "api", "command": ["./api"], "dir": "backend", "port": 8090, "note": "fixed"},
	              {"name": "queue", "command": ["./q"], "env_files": [".env"], "disabled": true}],
	  "attack": {"bits": 20}
	}`
	r := Convert("shop.example.com", []byte(in))
	if !r.Ready() {
		t.Fatalf("blocked: %v", r.Blockers)
	}
	m := r.Manifest
	if m.Title != "Shop" || len(m.Aliases) != 1 || m.Run == nil || m.Run.Cwd != "app" || m.Run.Port != 36666 {
		t.Fatalf("run: %+v", m.Run)
	}
	if strings.Join(m.Run.EnvFiles, ",") != ".env,.env.local" || m.Run.EnvFromFile["NEXT_DIST_DIR"] != ".next-current" || m.Run.Env["X_PORT"] != "$PORT" {
		t.Fatalf("env: %+v", m.Run)
	}
	if m.Run.Health.Path != "/up" || m.Run.Health.Interval.Or(0).String() != "30s" || m.Run.Health.Timeout != 0 {
		t.Fatalf("health: %+v", m.Run.Health)
	}
	if !access.CheckPassword(m.Access.Password, "site pw") || m.Access.Message != "ask Ana" {
		t.Fatal("the V1 site password does not open the V2 site")
	}
	zone := m.Access.Routes["/admin"]
	if zone == nil || !access.CheckPassword(zone.Password, "zone pw") || zone.Note != "back office" || zone.Allow[0] != "10.0.0.0/8" {
		t.Fatalf("zone: %+v", zone)
	}
	if strings.Contains(zone.Password, "zone pw") {
		t.Fatal("a clear password was carried in the clear")
	}
	if m.Access.Deny[0] != "203.0.113.0/24" {
		t.Fatalf("deny: %+v", m.Access)
	}
	if w := m.Workers["api"]; w == nil || w.Port != 8090 || w.Cwd != "backend" || w.Note != "fixed" {
		t.Fatalf("worker api: %+v", w)
	}
	if w := m.Workers["queue"]; w == nil || !w.Disabled || w.Port != 0 {
		t.Fatalf("worker queue: %+v", w)
	}
	notes := strings.Join(r.Notes, "\n")
	for _, want := range []string{"health.timeout", "attack: written but off", "in the clear"} {
		if !strings.Contains(notes, want) {
			t.Errorf("no note about %q in:\n%s", want, notes)
		}
	}
}

func TestWhatV2CannotDoYetBlocksTheSite(t *testing.T) {
	cases := map[string]string{
		`{"kind":"static","vpn":{"devices":["phone"]}}`:                      "vpn",
		`{"kind":"proxy","command":["x"],"container":{"image":"node:22"}}`:   "container",
		`{"kind":"proxy","command":["x"],"claude_api":{"daily_usd":1}}`:      "claude_api",
		`{"kind":"static","attack":{"on":true}}`:                             "attack mode on",
		`{"kind":"static","cache":{"rules":[{"path":"/","mode":"origen"}]}}`: "cache rules",
		`{"kind":"static","hold":{"mode":"410"}}`:                            "hold mode",
		`{"kind":"static","comand":["typo"]}`:                                "does not read",
		`{"kind":"cgi"}`:                                                     "kind",
	}
	for in, want := range cases {
		r := Convert("a.example", []byte(in))
		if r.Ready() || r.Manifest != nil {
			t.Errorf("%s converted", in)
			continue
		}
		if !strings.Contains(strings.Join(r.Blockers, "\n"), want) {
			t.Errorf("%s: blockers %v do not mention %q", in, r.Blockers, want)
		}
	}
}

func TestAStaticSiteServesPublicLikeV1(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","title":"T"}`))
	if !r.Ready() || r.Manifest.Static == nil || r.Manifest.Static.Root != "public" {
		t.Fatalf("%+v", r)
	}
}

func TestAProjectKeepsItsWorkersAndServesNothing(t *testing.T) {
	r := Convert("tools", []byte(`{"kind":"static","root":"public","workers":[{"name":"w","command":["./w"]}]}`))
	if !r.Ready() || r.Manifest.Static != nil || r.Manifest.Workers["w"] == nil {
		t.Fatalf("%+v", r)
	}
	if r := Convert("tools", []byte(`{"kind":"proxy","command":["x"]}`)); r.Ready() {
		t.Fatal("a project with a process of its own was converted")
	}
}

func TestApplyKeepsTheOriginalAndOnlyTouchesReadySites(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		os.MkdirAll(filepath.Join(dir, name, "public"), 0o755)
		os.WriteFile(filepath.Join(dir, name, manifest.FileName), []byte(body), 0o644)
	}
	write("ok.example", `{"kind":"static","title":"ok"}`)
	write("vpn.example", `{"kind":"static","vpn":{"devices":["p"]}}`)
	write("v2.example", `{"version":2}`)
	write("_retirado-x.example-20260101-0000", `{"kind":"static"}`)
	write("out.example", `{"kind":"static","root":"../ok.example"}`)
	results, err := Plan(dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if _, listed := byName["v2.example"]; listed {
		t.Fatal("a site already on V2 was planned")
	}
	if r := byName["_retirado-x.example-20260101-0000"]; r.Skipped == "" {
		t.Fatalf("a retired folder was not reported as skipped: %+v", r)
	}
	if r := byName["out.example"]; r.Ready() {
		t.Fatal("a root outside the site was converted")
	}
	original, _ := os.ReadFile(filepath.Join(dir, "ok.example", manifest.FileName))
	for i := 0; i < 2; i++ {
		written, err := Apply(dir, results)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (len(written) != 1 || written[0] != "ok.example") {
			t.Fatalf("written %v", written)
		}
	}
	backup, _ := os.ReadFile(filepath.Join(dir, "ok.example", BackupName))
	if string(backup) != string(original) {
		t.Fatalf("the V1 manifest was not kept as it was: %s", backup)
	}
	m, err := manifest.Load(filepath.Join(dir, "ok.example"))
	if err != nil || m.Title != "ok" {
		t.Fatalf("the converted manifest does not load: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vpn.example", BackupName)); err == nil {
		t.Fatal("a blocked site was touched")
	}
}

func TestWwwFalseIsCarried(t *testing.T) {
	r := Convert("a.example", []byte(`{"kind":"static","www":false}`))
	if !r.Ready() || r.Manifest.WWW == nil || *r.Manifest.WWW {
		t.Fatalf("%+v", r)
	}
	if h := r.Manifest.Hosts("a.example"); len(h) != 1 {
		t.Fatalf("hosts %v", h)
	}
}

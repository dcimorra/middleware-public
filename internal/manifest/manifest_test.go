package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnUnknownFieldIsRejected(t *testing.T) {
	_, err := Parse([]byte(`{"version":2,"run":{"comand":["x"]}}`))
	if err == nil {
		t.Fatal("a typo in a field name must not parse")
	}
}

func TestDurationsReadAsStringsAndSeconds(t *testing.T) {
	m, err := Parse([]byte(`{"version":2,"run":{"command":["x"],"drain":"3s","health":{"timeout":10}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(m.Run.Drain) != 3*time.Second || time.Duration(m.Run.Health.Timeout) != 10*time.Second {
		t.Fatalf("drain=%v timeout=%v", time.Duration(m.Run.Drain), time.Duration(m.Run.Health.Timeout))
	}
}

func TestModePrecedence(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{`{"version":2,"disabled":true,"hold":{"enabled":true},"run":{"command":["x"]}}`, ModeDisabled},
		{`{"version":2,"hold":{"enabled":true},"run":{"command":["x"]}}`, ModeHold},
		{`{"version":2,"run":{"command":["x"]},"upstream":"http://127.0.0.1:1"}`, ModeRun},
		{`{"version":2,"upstream":"http://127.0.0.1:1"}`, ModeUpstream},
		{`{"version":2}`, ModeStatic},
	}
	for _, c := range cases {
		m, err := Parse([]byte(c.in))
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Mode(); got != c.want {
			t.Errorf("%s: got %s want %s", c.in, got, c.want)
		}
	}
}

func TestAWorkerPortIsOptionalButMustBeAPort(t *testing.T) {
	if _, err := Parse([]byte(`{"version":2,"workers":{"w":{"command":["x"]}}}`)); err != nil {
		t.Fatalf("a worker without a port gets one assigned: %v", err)
	}
	if _, err := Parse([]byte(`{"version":2,"workers":{"w":{"command":["x"],"port":70000}}}`)); err == nil {
		t.Fatal("port 70000 validated")
	}
}

func TestRevisionChangesWhenThePointerFileChanges(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".current"), []byte("build-1\n"), 0o644)
	m, _ := Parse([]byte(`{"version":2,"run":{"command":["./s","--port","$PORT"],"env_from_file":{"DIST":".current"}}}`))
	a, err := Resolve("a.test", dir, m)
	if err != nil {
		t.Fatal(err)
	}
	if a.Env["DIST"] != "build-1" {
		t.Fatalf("pointer not read: %q", a.Env["DIST"])
	}
	os.WriteFile(filepath.Join(dir, ".current"), []byte("build-2\n"), 0o644)
	b, _ := Resolve("a.test", dir, m)
	if a.Revision == b.Revision {
		t.Fatal("revision did not change with the pointer")
	}
	c, _ := Resolve("a.test", dir, m)
	if b.Revision != c.Revision {
		t.Fatal("revision is not stable")
	}
}

func TestThePortIsNotPartOfTheRevision(t *testing.T) {
	m, _ := Parse([]byte(`{"version":2,"run":{"command":["./s","--port","$PORT"],"env":{"URL":"http://127.0.0.1:$PORT"}}}`))
	s, _ := Resolve("a.test", "/tmp/x", m)
	if s.Command[2] != "$PORT" {
		t.Fatalf("$PORT expanded too early: %v", s.Command)
	}
	cmd, env := s.WithPort(4000)
	if cmd[2] != "4000" || env["URL"] != "http://127.0.0.1:4000" {
		t.Fatalf("cmd=%v env=%v", cmd, env)
	}
}

func TestUnknownVariablesAreLeftAlone(t *testing.T) {
	if got := Expand("$HOME/$DOMAIN", map[string]string{"DOMAIN": "a.test"}); got != "$HOME/a.test" {
		t.Fatalf("got %q", got)
	}
}

func TestEnvFileSupportsQuotesCommentsAndExport(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("# c\nexport A=1\nB=\"two words\"\nC='x'\nbad line\n"), 0o644)
	got, err := readEnvFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if got["A"] != "1" || got["B"] != "two words" || got["C"] != "x" || len(got) != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestValidDomain(t *testing.T) {
	for _, ok := range []string{"a.test", "sub.example.co.uk", "x-y.example.com"} {
		if !ValidDomain(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"Example.com", "nodot", "-a.test", "a..test", "a.test/"} {
		if ValidDomain(bad) {
			t.Errorf("%s should be invalid", bad)
		}
	}
}

func TestAPathOutsideTheSiteFolderIsRefused(t *testing.T) {
	dir := t.TempDir()
	site := filepath.Join(dir, "a.example")
	other := filepath.Join(dir, "b.example")
	os.MkdirAll(site, 0o755)
	os.MkdirAll(other, 0o755)
	os.Symlink(other, filepath.Join(site, "public"))
	bad := []string{
		`{"version":2,"static":{"root":"../b.example"}}`,
		`{"version":2,"static":{"root":"public"}}`,
		`{"version":2,"static":{"root":"/etc"}}`,
		`{"version":2,"run":{"command":["x"],"cwd":"../b.example"}}`,
		`{"version":2,"run":{"command":["x"],"env_files":["../b.example/.env"]}}`,
		`{"version":2,"run":{"command":["x"],"env_from_file":{"K":"$SITE_DIR/../b.example/.env"}}}`,
		`{"version":2,"run":{"command":["x"],"runtime":"docker","docker":{"image":"i","volumes":["/:/host"]}}}`,
		`{"version":2,"run":{"command":["x"],"runtime":"docker","docker":{"image":"i","volumes":["../b.example:/x"]}}}`,
		`{"version":2,"run":{"command":["x"],"runtime":"docker","docker":{"image":"i","build":"../b.example/Dockerfile"}}}`,
		`{"version":2,"workers":{"w":{"command":["x"],"port":4000,"env_files":["../b.example/.env"]}}}`,
	}
	for _, m := range bad {
		os.WriteFile(filepath.Join(site, FileName), []byte(m), 0o644)
		if _, err := Load(site); err == nil {
			t.Errorf("accepted %s", m)
		}
	}
	good := []string{
		`{"version":2,"static":{"root":"dist"}}`,
		`{"version":2,"run":{"command":["x"],"cwd":"app","env_files":[".env"],"env_from_file":{"K":"$SITE_DIR/.current"}}}`,
		`{"version":2,"run":{"command":["x"],"runtime":"docker","docker":{"image":"i","volumes":["data:/app/data","./uploads:/u:ro","/cache"]}}}`,
	}
	for _, m := range good {
		os.WriteFile(filepath.Join(site, FileName), []byte(m), 0o644)
		if _, err := Load(site); err != nil {
			t.Errorf("refused %s: %v", m, err)
		}
	}
}

func TestHostsCarryTheAliasesAndTheImplicitWww(t *testing.T) {
	m, _ := Parse([]byte(`{"version":2,"aliases":["shop.example.org","a.example"]}`))
	if got := strings.Join(m.Hosts("a.example"), ","); got != "a.example,shop.example.org,www.a.example" {
		t.Fatalf("apex: %s", got)
	}
	// An extra domain that is an apex brings its www too.
	if got := strings.Join(m.Hosts("sub.a.example"), ","); got != "sub.a.example,shop.example.org,a.example,www.a.example" {
		t.Fatalf("subdomain: %s", got)
	}
	m2, _ := Parse([]byte(`{"version":2,"aliases":["patata.es","www.patata.org"]}`))
	if got := strings.Join(m2.Hosts("patata.com"), ","); got != "patata.com,patata.es,www.patata.org,www.patata.com,www.patata.es" {
		t.Fatalf("apex aliases: %s", got)
	}
	no := false
	m.WWW = &no
	if got := strings.Join(m.Hosts("a.example"), ","); got != "a.example,shop.example.org" {
		t.Fatalf("www false: %s", got)
	}
	if m.Hosts("tools") != nil {
		t.Fatal("a project has names")
	}
}

func TestAnExpectedStatusIsTheOnlyHealthyOne(t *testing.T) {
	if !(Health{}).Healthy(404) || (Health{}).Healthy(502) {
		t.Fatal("default: below 500 is healthy")
	}
	h := Health{ExpectStatus: 204}
	if h.Healthy(200) || !h.Healthy(204) {
		t.Fatal("expect_status")
	}
}

func TestAnUnreadableAddressRejectsTheManifest(t *testing.T) {
	good := []string{
		`{"version":2,"access":{"allow":["10.0.0.0/8"," 192.168.1.4 ","::1","2001:db8::/32"]}}`,
		`{"version":2,"access":{"routes":{"/admin":{"allow":["10.1.0.0/16"],"deny":["10.1.2.3"]}}}}`,
	}
	for _, in := range good {
		if _, err := Parse([]byte(in)); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
	// Each of these used to be skipped by the door. The first one is the
	// dangerous shape: an allow list whose only entry is skipped is an
	// empty allow list, and that admits everyone.
	bad := []string{
		`{"version":2,"access":{"allow":["10.0.0/8"]}}`,
		`{"version":2,"access":{"deny":["10.9.9.9","nope"]}}`,
		`{"version":2,"access":{"allow":[""]}}`,
		`{"version":2,"access":{"routes":{"/admin":{"allow":["10.1.0.0/33"]}}}}`,
		`{"version":2,"access":{"routes":{"/admin":{"deny":["300.1.1.1"]}}}}`,
	}
	for _, in := range bad {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s parsed", in)
		}
	}
}

func TestAMisspeltSecurityRuleRejectsTheManifest(t *testing.T) {
	if _, err := Parse([]byte(`{"version":2,"security":{"bots":{"deny":["ai","seo"]},"paths":["/wp-login.php"]}}`)); err != nil {
		t.Fatal(err)
	}
	_, err := Parse([]byte(`{"version":2,"security":{"bots":{"deny":["ia"]}}}`))
	if err == nil || !strings.Contains(err.Error(), "security.bots.deny") {
		t.Fatalf("a family that does not exist: %v", err)
	}
}

func TestAnAppIsNotAFieldOfV2(t *testing.T) {
	if _, err := Parse([]byte(`{"version":2,"app":"mask"}`)); err == nil {
		t.Fatal("app was accepted: a field nothing reads would do nothing and say nothing")
	}
}

func TestTheGatewaysPermissionsAndAgentAreChecked(t *testing.T) {
	for raw, ok := range map[string]bool{
		`{"version":2,"claude_api":{"provider":"openai","permissions":["read","shell"],"daily_turns":50}}`: true,
		`{"version":2,"claude_api":{"permissions":["read","root"]}}`:                                       false,
		`{"version":2,"claude_api":{"provider":"Not An Agent"}}`:                                           false,
		`{"version":2,"claude_api":{"daily_turns":-1}}`:                                                    false,
		`{"version":2,"claude_api":{"models":["haiku","sonnet"],"model":"sonnet"}}`:                        true,
		`{"version":2,"claude_api":{"models":["haiku"],"model":"opus"}}`:                                   false,
	} {
		var m Manifest
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if err := m.Validate(); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// A worker in a container is checked as a site's run is, and its move is
// both halves or none: an export nobody imports carries the data and
// leaves the site on an empty database.
func TestADockerWorkerAndItsMoveAreChecked(t *testing.T) {
	ok := `{"version":2,"static":{},"workers":{"db":{"command":["postgres"],"runtime":"docker","docker":{"image":"postgres:16","volumes":["pg:/data"]},"move":{"export":["./x"],"import":["./y"]}}}}`
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("a valid docker worker was refused: %v", err)
	}
	for want, body := range map[string]string{
		"needs image or build":    `{"version":2,"workers":{"db":{"command":["postgres"],"runtime":"docker"}}}`,
		"not process or docker":   `{"version":2,"workers":{"db":{"command":["x"],"runtime":"vm"}}}`,
		"needs export and import": `{"version":2,"workers":{"db":{"command":["x"],"move":{"export":["./x"]}}}}`,
	} {
		if _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	m, err := Parse([]byte(`{"version":2,"workers":{"db":{"command":["x"],"runtime":"docker","docker":{"image":"i","volumes":["../../etc:/etc"]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CheckInside(t.TempDir()); err == nil || !strings.Contains(err.Error(), "workers.db.docker.volumes") {
		t.Fatalf("a worker's volume out of the folder: %v", err)
	}
}

package audit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/middleware-public/internal/events"
)

// fakeNPM writes an npm that prints what npm audit --json prints (the
// shape of npm 10) and exits 1, as the real one does when it finds things.
func fakeNPM(t *testing.T, critical string) string {
	t.Helper()
	dir := t.TempDir()
	doc := `{"auditReportVersion":2,"vulnerabilities":{` +
		`"minimist":{"name":"minimist","severity":"critical","isDirect":true,"via":[{"source":1097678,"name":"minimist","title":"Prototype Pollution in minimist","url":"https://github.com/advisories/GHSA-xvch-5gv4-984h","severity":"critical","range":"` + critical + `"}],"range":"` + critical + `"},` +
		`"lodash":{"name":"lodash","severity":"high","via":[{"title":"Command Injection in lodash","url":"https://github.com/advisories/GHSA-35jh-r3h4-6jhm","severity":"high"}],"range":"<4.17.21"},` +
		`"semver":{"name":"semver","severity":"moderate","via":["x"],"range":"<7.5.2"}},` +
		`"metadata":{"vulnerabilities":{"info":0,"low":0,"moderate":1,"high":1,"critical":1,"total":3}}}`
	os.WriteFile(filepath.Join(dir, "doc.json"), []byte(doc), 0o644)
	script := "#!/bin/sh\ncase \"$*\" in *fix*) echo FIX >> " + filepath.Join(dir, "fixed") + ";; esac\ncat " + filepath.Join(dir, "doc.json") + "\nexit 1\n"
	npm := filepath.Join(dir, "npm")
	os.WriteFile(npm, []byte(script), 0o755)
	return npm
}

func TestOnlyWhatIsCriticalAndNewIsAnnounced(t *testing.T) {
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "package.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(site, "package-lock.json"), []byte(`{}`), 0o644)
	bus := events.New("")
	npm := fakeNPM(t, "<0.2.4")
	a := New(Options{DataDir: t.TempDir(), Bus: bus, NPM: func() string { return npm },
		Sites: func() []Site { return []Site{{Domain: "a.example", Dir: site}} }})
	count := func() int {
		n := 0
		for _, e := range bus.Recent() {
			if e.Type == "deps.critical" {
				n++
			}
		}
		return n
	}
	r := a.Check(context.Background(), Site{Domain: "a.example", Dir: site})
	if r.Error != "" || r.Counts["critical"] != 1 || len(r.Critical) != 1 || len(r.High) != 1 || r.Critical[0].Title != "Prototype Pollution in minimist" {
		t.Fatalf("report %+v", r)
	}
	if count() != 1 {
		t.Fatalf("%d deps.critical after the first audit", count())
	}
	a.Check(context.Background(), Site{Domain: "a.example", Dir: site})
	if count() != 1 {
		t.Fatal("the same critical was announced twice")
	}
	// npm widens the range: news again.
	npm2 := fakeNPM(t, "<1.2.6")
	a.opts.NPM = func() string { return npm2 }
	a.Check(context.Background(), Site{Domain: "a.example", Dir: site})
	if count() != 2 {
		t.Fatal("a widened range was not announced")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(npm), "fixed")); err == nil {
		t.Fatal("npm audit fix was run")
	}
	if rs := a.Reports(); len(rs) != 1 || rs[0].Domain != "a.example" {
		t.Fatalf("reports %+v", rs)
	}
}

func TestASiteWithoutALockfileIsSaidSoAndOneWithoutNodeIsLeftAlone(t *testing.T) {
	noLock, noNode := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(noLock, "package.json"), []byte(`{}`), 0o644)
	a := New(Options{DataDir: t.TempDir(), NPM: func() string { return "/bin/false" }})
	if r := a.Check(context.Background(), Site{Domain: "x.example", Dir: noLock}); !strings.Contains(r.Skipped, "package-lock") {
		t.Fatalf("no lockfile: %+v", r)
	}
	a.Check(context.Background(), Site{Domain: "y.example", Dir: noNode})
	if _, ok := a.Report("y.example"); ok {
		t.Fatal("a site that is not a Node project got a report")
	}
}

// TestTheRealNPM audits a real project with the real npm:
// AUDIT_REAL=/path/to/project go test ./internal/audit -run Real -v
func TestTheRealNPM(t *testing.T) {
	dir := os.Getenv("AUDIT_REAL")
	if dir == "" {
		t.Skip("AUDIT_REAL=<a project with a package-lock.json>")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("no npm")
	}
	a := New(Options{DataDir: t.TempDir(), NPM: func() string { return npm }})
	r := a.Check(context.Background(), Site{Domain: "real.example", Dir: dir})
	t.Logf("%+v", r)
	if r.Error != "" {
		t.Fatal(r.Error)
	}
}

// npm runs in the site's folder and expands ${VAR} from its environment
// into the site's .npmrc, so whatever the supervisor's environment holds
// must not reach it.
func TestNPMDoesNotGetTheSupervisorsSecrets(t *testing.T) {
	t.Setenv("MIDDLEWARE_SECRET_KEY", "the-house-key")
	dir := t.TempDir()
	seen := filepath.Join(dir, "env")
	npm := filepath.Join(dir, "npm")
	os.WriteFile(npm, []byte("#!/bin/sh\nenv > "+seen+"\necho '{}'\n"), 0o755)
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "package.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(site, "package-lock.json"), []byte(`{}`), 0o644)
	a := New(Options{DataDir: t.TempDir(), NPM: func() string { return npm }})
	a.Check(context.Background(), Site{Domain: "a.example", Dir: site})
	env, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "the-house-key") {
		t.Fatalf("npm was given the supervisor's secret:\n%s", env)
	}
	if !strings.Contains(string(env), "PATH=") {
		t.Fatalf("npm lost its PATH:\n%s", env)
	}
}

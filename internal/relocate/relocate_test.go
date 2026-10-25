package relocate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// localTarget is a target that is another root on this machine, through
// the real Receiver.
type localTarget struct {
	rc       Receiver
	ready    bool
	removed  bool
	publish  string
	failSend bool
	noDNS    bool
	noDocker bool
}

func (t *localTarget) Name() string { return "node-b" }
func (t *localTarget) Send(ctx context.Context, d string, r io.Reader) error {
	if t.failSend {
		io.Copy(io.Discard, r)
		return errors.New("no room")
	}
	_, err := t.rc.Receive(ctx, d, r)
	return err
}
func (t *localTarget) SendDelta(ctx context.Context, d string, r io.Reader) error {
	_, err := t.rc.ReceiveDelta(ctx, d, r)
	return err
}
func (t *localTarget) Commit(_ context.Context, d string, m []byte) error { return t.rc.Commit(d, m) }
func (t *localTarget) Discard(_ context.Context, d string) error          { return t.rc.Discard(d) }
func (t *localTarget) Remove(_ context.Context, d string) error {
	t.removed = true
	return os.RemoveAll(filepath.Join(t.rc.Root, "sites", d))
}
func (t *localTarget) Ready(_ context.Context, d string) (bool, string, error) {
	_, err := os.Stat(filepath.Join(t.rc.Root, "sites", d, "site.json"))
	return err == nil && t.ready, "not up", nil
}
func (t *localTarget) Publish(_ context.Context, d, door string) error { t.publish = door; return nil }
func (t *localTarget) Check(_ context.Context, d string, need Needs) error {
	if t.noDNS && len(need.Names) > 0 {
		return errors.New("node-b cannot publish " + strings.Join(need.Names, ", "))
	}
	if t.noDocker && need.Docker {
		return errors.New("node-b has no docker")
	}
	return nil
}

// localSource is this root, with the site's changes during the move.
type localSource struct {
	root        string
	duringStop  func()
	started     []byte
	unpublished bool
	archived    bool
}

func (s *localSource) Stop(_ context.Context, d string) ([]byte, error) {
	p := filepath.Join(s.root, "sites", d, "site.json")
	orig, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(`{"version":2,"disabled":true}`), 0o644)
	if s.duringStop != nil {
		s.duringStop()
	}
	return orig, nil
}
func (s *localSource) Start(d string, m []byte) error {
	s.started = m
	return os.WriteFile(filepath.Join(s.root, "sites", d, "site.json"), m, 0o644)
}
func (s *localSource) Published(string) (string, bool)   { return "tunnel", true }
func (s *localSource) Names(d string) []string           { return []string{d, "www." + d} }
func (s *localSource) Unpublish(context.Context, string) { s.unpublished = true }
func (s *localSource) Archive(_ context.Context, d string, _ []byte) (string, error) {
	s.archived = true
	return "archived.tar.gz", os.RemoveAll(filepath.Join(s.root, "sites", d))
}

func seed(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		os.Chtimes(p, old, old)
	}
	write("sites/a.test/site.json", `{"version":2,"static":{"root":"public"}}`)
	write("sites/a.test/public/index.html", "hello")
	write("sites/a.test/public/big.bin", strings.Repeat("x", 200000))
	write("sites/a.test/data/db.json", `{"n":1}`)
	write("sites/a.test/gone.txt", "to be deleted")
	write("data/homes/a.test/.profile", "home")
	write("data/chat/a.test.json", `{"turns":[1]}`)
	write("data/agents/cost/a.test.json", `{"total":1}`)
}

func TestASiteMovesWithWhatChangedWhileItWasCopied(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	s := &localSource{root: src, duringStop: func() {
		// Written and deleted after the first copy went.
		os.WriteFile(filepath.Join(src, "sites/a.test/data/db.json"), []byte(`{"n":2}`), 0o644)
		os.Remove(filepath.Join(src, "sites/a.test/gone.txt"))
	}}
	tg := &localTarget{rc: Receiver{Root: dst}, ready: true}
	var steps []string
	m := &Mover{Root: src, Source: s, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
	rep, err := m.Move(context.Background(), "a.test", tg, func(step, _ string) { steps = append(steps, step) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(steps, " "); got != "check copy stop final commit start dns cleanup done" {
		t.Fatalf("steps: %s", got)
	}
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(dst, rel)); return string(b) }
	if read("sites/a.test/data/db.json") != `{"n":2}` || read("sites/a.test/public/index.html") != "hello" || len(read("sites/a.test/public/big.bin")) != 200000 {
		t.Fatal("the site did not arrive whole and current")
	}
	if _, err := os.Stat(filepath.Join(dst, "sites/a.test/gone.txt")); err == nil {
		t.Fatal("a file deleted during the move came back")
	}
	// The manifest it had, not the stop written to get there.
	if !strings.Contains(read("sites/a.test/site.json"), `"static"`) {
		t.Fatalf("manifest: %s", read("sites/a.test/site.json"))
	}
	if read("data/homes/a.test/.profile") != "home" || read("data/chat/a.test.json") != `{"turns":[1]}` || read("data/agents/cost/a.test.json") != `{"total":1}` {
		t.Fatal("what the house keeps of it did not come along")
	}
	// The last copy carried what changed, not the site again.
	if rep.Last >= rep.First/2 {
		t.Fatalf("last copy %d bytes of a first of %d", rep.Last, rep.First)
	}
	if tg.publish != "tunnel" || !s.unpublished || !s.archived || rep.DNS != "moved" {
		t.Fatalf("dns %q unpublished %v archived %v", tg.publish, s.unpublished, s.archived)
	}
	if _, err := os.Stat(filepath.Join(dst, "data/relocate/a.test")); err == nil {
		t.Fatal("the staging folder was left behind")
	}
}

func TestATargetThatDoesNotComeUpLeavesTheSiteRunningHere(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	s := &localSource{root: src}
	tg := &localTarget{rc: Receiver{Root: dst}, ready: false}
	m := &Mover{Root: src, Source: s, Poll: 10 * time.Millisecond, ReadyWait: 50 * time.Millisecond}
	var steps []string
	_, err := m.Move(context.Background(), "a.test", tg, func(step, _ string) { steps = append(steps, step) })
	if err == nil || !strings.Contains(err.Error(), "runs here again") {
		t.Fatalf("err: %v", err)
	}
	// The way back is an operation of its own, read after the one that failed.
	if got := strings.Join(steps, " "); !strings.HasSuffix(got, "start restore") {
		t.Fatalf("steps: %s", got)
	}
	if !tg.removed || !strings.Contains(string(s.started), `"static"`) || s.archived || s.unpublished {
		t.Fatalf("removed %v started %q archived %v unpublished %v", tg.removed, s.started, s.archived, s.unpublished)
	}
}

func TestAFirstCopyThatFailsTouchesNothing(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	s := &localSource{root: src}
	tg := &localTarget{rc: Receiver{Root: dst}, failSend: true}
	m := &Mover{Root: src, Source: s}
	if _, err := m.Move(context.Background(), "a.test", tg, nil); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("err: %v", err)
	}
	if s.started != nil || s.archived {
		t.Fatal("the source was touched")
	}
	if b, _ := os.ReadFile(filepath.Join(src, "sites/a.test/site.json")); strings.Contains(string(b), "disabled") {
		t.Fatal("the source was stopped")
	}
}

func TestTheReceiverRefusesANameItAlreadyServes(t *testing.T) {
	dst := t.TempDir()
	os.MkdirAll(filepath.Join(dst, "sites", "a.test"), 0o755)
	if _, err := (Receiver{Root: dst}).Receive(context.Background(), "a.test", strings.NewReader("")); !errors.Is(err, ErrExists) {
		t.Fatalf("err: %v", err)
	}
	if _, err := (Receiver{Root: dst}).Receive(context.Background(), "../etc", strings.NewReader("")); !errors.Is(err, ErrBadName) {
		t.Fatalf("err: %v", err)
	}
}

func TestATargetThatCannotPublishTheNamesIsRefusedBeforeAnythingMoves(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	s := &localSource{root: src}
	tg := &localTarget{rc: Receiver{Root: dst}, ready: true, noDNS: true}
	m := &Mover{Root: src, Source: s}
	if _, err := m.Move(context.Background(), "a.test", tg, nil); err == nil || !strings.Contains(err.Error(), "cannot publish") {
		t.Fatalf("err: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(src, "sites/a.test/site.json")); strings.Contains(string(b), "disabled") || s.archived {
		t.Fatal("the source was touched")
	}
	if _, err := os.Stat(filepath.Join(dst, "data/relocate/a.test")); err == nil {
		t.Fatal("something was sent")
	}
}

func TestAContainerSiteDoesNotMoveToANodeWithoutDocker(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	os.WriteFile(filepath.Join(src, "sites/a.test/site.json"), []byte(`{"version":2,"run":{"command":["node","s.js"],"runtime":"docker","docker":{"image":"node:22"}}}`), 0o644)
	s := &localSource{root: src}
	tg := &localTarget{rc: Receiver{Root: dst}, ready: true, noDocker: true}
	if _, err := (&Mover{Root: src, Source: s}).Move(context.Background(), "a.test", tg, nil); err == nil || !strings.Contains(err.Error(), "no docker") {
		t.Fatalf("err: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(src, "sites/a.test/site.json")); strings.Contains(string(b), "disabled") {
		t.Fatal("the source was stopped")
	}
}

// preparingTarget installs on its side what was left behind.
type preparingTarget struct {
	*localTarget
	sawModules bool
	prepared   bool
	dirs       []string
}

func (t *preparingTarget) Prepare(_ context.Context, d string, dirs []string) (string, error) {
	t.dirs = dirs
	mods := filepath.Join(t.rc.StagingSite(d), "node_modules")
	_, err := os.Stat(mods)
	t.sawModules = err == nil
	t.prepared = true
	// What an install leaves: it must still be there when the site starts.
	os.MkdirAll(filepath.Join(mods, ".bin"), 0o755)
	os.WriteFile(filepath.Join(mods, ".bin", "tailwindcss"), []byte("built here"), 0o755)
	return "added 3 packages", nil
}

// A Node site's node_modules is built for this machine's Node: it is left
// behind and installed on the target, after the first copy and before the
// site stops here, so the install is not time it is down.
func TestANodeSitesModulesAreInstalledThereNotCarried(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	os.WriteFile(filepath.Join(src, "sites", "a.test", "package.json"), []byte(`{"name":"a"}`), 0o644)
	os.MkdirAll(filepath.Join(src, "sites", "a.test", "node_modules", "better-sqlite3"), 0o755)
	os.WriteFile(filepath.Join(src, "sites", "a.test", "node_modules", "better-sqlite3", "x.node"), []byte("built for node 24"), 0o644)
	tg := &preparingTarget{localTarget: &localTarget{rc: Receiver{Root: dst}, ready: true}}
	m := &Mover{Root: src, Source: &localSource{root: src}, Parts: MoveParts, Install: true, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
	var steps []string
	if _, err := m.Move(context.Background(), "a.test", tg, func(step, _ string) { steps = append(steps, step) }); err != nil {
		t.Fatal(err)
	}
	if !tg.prepared || tg.sawModules || strings.Join(tg.dirs, ",") != "." {
		t.Fatalf("prepared %v (in %v), node_modules carried %v", tg.prepared, tg.dirs, tg.sawModules)
	}
	if got := strings.Join(steps, " "); !strings.HasPrefix(got, "check copy install install stop") {
		t.Fatalf("steps: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "sites", "a.test", "public", "index.html")); err != nil {
		t.Fatal("the site did not arrive")
	}
	// The last copy removes what was deleted at the source, and node_modules
	// is not that: it was installed here after the first copy.
	if got, _ := os.ReadFile(filepath.Join(dst, "sites", "a.test", "node_modules", ".bin", "tailwindcss")); string(got) != "built here" {
		t.Fatal("what the install put in node_modules was gone when the site started")
	}
}

// A site that does not come up leaves the target as it was: its folder,
// and its HOME and conversations under data/, gone; an earlier life of the
// name that the commit set aside, put back. Before, the data stayed, and
// each retry set the last copy aside as ".before-…".
func TestAFailedMoveLeavesTheTargetAsItWas(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	os.MkdirAll(filepath.Join(dst, "data", "homes", "a.test"), 0o755)
	os.WriteFile(filepath.Join(dst, "data", "homes", "a.test", "earlier"), []byte("an earlier life"), 0o644)
	tg := &localTarget{rc: Receiver{Root: dst}, ready: false}
	m := &Mover{Root: src, Source: &localSource{root: src}, Poll: 10 * time.Millisecond, ReadyWait: 50 * time.Millisecond}
	if _, err := m.Move(context.Background(), "a.test", tg, nil); err == nil || !strings.Contains(err.Error(), "runs here again") {
		t.Fatalf("err: %v", err)
	}
	for _, rel := range []string{"sites/a.test", "data/chat/a.test.json", "data/agents/cost/a.test.json", "data/relocate/a.test", "data/relocate/placed/a.test.json"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err == nil {
			t.Errorf("%s was left on the target", rel)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "data", "homes", "a.test", "earlier")); string(got) != "an earlier life" {
		t.Error("the HOME that was there before was not put back")
	}
	if m, _ := filepath.Glob(filepath.Join(dst, "data", "homes", "a.test.before-*")); len(m) != 0 {
		t.Errorf("set-aside copies left: %v", m)
	}
	// Never under a site that is there.
	os.MkdirAll(filepath.Join(dst, "sites", "b.test"), 0o755)
	os.MkdirAll(filepath.Join(dst, "data", "relocate", "placed"), 0o755)
	os.WriteFile(filepath.Join(dst, "data", "relocate", "placed", "b.test.json"), []byte(`{"domain":"b.test","parts":["data/homes/b.test"]}`), 0o644)
	os.MkdirAll(filepath.Join(dst, "data", "homes", "b.test"), 0o755)
	if err := (Receiver{Root: dst}).Discard("b.test"); err == nil {
		t.Fatal("a site that is here had its data taken out")
	}
	if _, err := os.Stat(filepath.Join(dst, "data", "homes", "b.test")); err != nil {
		t.Fatal("the HOME of a site that is here was removed")
	}
}

// Only a package's node_modules is left for the target to install: a
// build's own (Next's .next/node_modules, links named "@prisma/client-…"
// that the compiled code resolves by) travels, or the moved site fails
// every page that touches its database.
func TestABuildsOwnNodeModulesTravelsWithTheSite(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	site := filepath.Join(src, "sites", "a.test")
	os.WriteFile(filepath.Join(site, "package.json"), []byte(`{"name":"a"}`), 0o644)
	os.MkdirAll(filepath.Join(site, "node_modules", "@prisma", "client"), 0o755)
	os.WriteFile(filepath.Join(site, "node_modules", "@prisma", "client", "index.js"), []byte("built here"), 0o644)
	os.MkdirAll(filepath.Join(site, ".next-blue", "node_modules", "@prisma"), 0o755)
	os.Symlink("../../../node_modules/@prisma/client", filepath.Join(site, ".next-blue", "node_modules", "@prisma", "client-2c3a283f"))
	tg := &preparingTarget{localTarget: &localTarget{rc: Receiver{Root: dst}, ready: true}}
	m := &Mover{Root: src, Source: &localSource{root: src}, Parts: MoveParts, Install: true, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
	if _, err := m.Move(context.Background(), "a.test", tg, nil); err != nil {
		t.Fatal(err)
	}
	if tg.sawModules {
		t.Fatal("the package's node_modules was carried")
	}
	link := filepath.Join(dst, "sites", "a.test", ".next-blue", "node_modules", "@prisma", "client-2c3a283f")
	if target, err := os.Readlink(link); err != nil || target != "../../../node_modules/@prisma/client" {
		t.Fatalf("the build's link: %q %v", target, err)
	}
}

// The folders whose dependencies are left behind are the ones with a
// package.json beside node_modules, wherever they are in the site.
func TestTheFoldersWhoseDependenciesAreLeftBehindAreFound(t *testing.T) {
	site := t.TempDir()
	for _, d := range []string{".", "frontend", "packages/api"} {
		os.MkdirAll(filepath.Join(site, d, "node_modules", "x"), 0o755)
		os.WriteFile(filepath.Join(site, d, "package.json"), []byte("{}"), 0o644)
	}
	os.MkdirAll(filepath.Join(site, ".next", "node_modules", "@prisma"), 0o755) // a build's own
	os.MkdirAll(filepath.Join(site, "docs"), 0o755)
	os.WriteFile(filepath.Join(site, "docs", "package.json"), []byte("{}"), 0o644) // never installed: nothing to put back
	if got := strings.Join(LeftBehind(site), ","); got != ".,frontend,packages/api" {
		t.Fatalf("left behind: %s", got)
	}
}

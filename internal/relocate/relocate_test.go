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
	if got := strings.Join(steps, " "); got != "copy stop final commit start dns cleanup done" {
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
	_, err := m.Move(context.Background(), "a.test", tg, nil)
	if err == nil || !strings.Contains(err.Error(), "runs here again") {
		t.Fatalf("err: %v", err)
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

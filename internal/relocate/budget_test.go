package relocate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cuttingTarget answers ready and, in the same breath, the move's time
// runs out.
type cuttingTarget struct {
	*localTarget
	cut        func()
	publishErr error
}

func (t *cuttingTarget) Ready(ctx context.Context, d string) (bool, string, error) {
	ok, why, err := t.localTarget.Ready(ctx, d)
	if ok {
		t.cut()
	}
	return ok, why, err
}

func (t *cuttingTarget) Publish(ctx context.Context, d, door string) error {
	t.publishErr = ctx.Err()
	return t.localTarget.Publish(ctx, d, door)
}

// ctxSource archives only with time left to do it.
type ctxSource struct{ *localSource }

func (s ctxSource) Archive(ctx context.Context, d string, m []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.localSource.Archive(ctx, d, m)
}

// A move whose time runs out once the site answers there still moves its
// names and archives the copy here: those steps have their own time. Cut
// with the move's, the site ran there with its names pointing here and
// its folder never archived.
func TestTheLastStepsOfAMoveDoNotDependOnItsTime(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &localSource{root: src}
	tg := &cuttingTarget{localTarget: &localTarget{rc: Receiver{Root: dst}, ready: true}, cut: cancel}
	m := &Mover{Root: src, Source: ctxSource{s}, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
	rep, err := m.Move(ctx, "a.test", tg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tg.publishErr != nil || rep.DNS != "moved" || !s.archived || rep.Archive == "" {
		t.Fatalf("publish %v dns %q archived %v", tg.publishErr, rep.DNS, s.archived)
	}
}

// A move is given what its steps may take by its manifest: a database
// whose export and import take two hours each is not cut at one.
func TestAMovesTimeIsWhatItsStepsMayTake(t *testing.T) {
	src := t.TempDir()
	seed(t, src)
	m := &Mover{Root: src, Source: &localSource{root: src}}
	plain := m.Budget("a.test")
	if plain < time.Hour {
		t.Fatalf("a plain site: %s", plain)
	}
	os.WriteFile(filepath.Join(src, "sites/a.test/site.json"), []byte(`{"version":2,"static":{"root":"public"},
 "workers":{"db":{"command":["x"],"move":{"export":["e"],"import":["i"],"timeout":"2h"}}}}`), 0o644)
	if got := m.Budget("a.test"); got < plain+6*time.Hour {
		t.Fatalf("with a worker of two hours a command: %s", got)
	}
	m.Install = true
	if got := m.Budget("a.test"); got < plain+6*time.Hour+30*time.Minute {
		t.Fatalf("with an install there too: %s", got)
	}
}

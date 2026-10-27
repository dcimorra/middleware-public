package relocate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/tasks"
)

func seedV1(t *testing.T, root string, taskFiles map[string]string) {
	t.Helper()
	os.MkdirAll(filepath.Join(root, "sites", "old.test", "public"), 0o755)
	os.WriteFile(filepath.Join(root, "sites", "old.test", "site.json"), []byte(`{"title":"old","root":"public"}`), 0o644)
	os.MkdirAll(filepath.Join(root, "tasks"), 0o755)
	for name, body := range taskFiles {
		os.WriteFile(filepath.Join(root, "tasks", name+".json"), []byte(body), 0o644)
	}
}

// A move that is undone gives the site its tasks back as they were, word
// for word, and takes out of the target the ones it had placed there.
func TestAnUndoneMoveTurnsTheTasksOnAgainAndLeavesNoneThere(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	on := `{"title":"Sync","group":"old","schedule":"50 0 * * *","command":["/bin/true"],"site":"old.test","timezone":"Europe/Madrid"}`
	seedV1(t, src, map[string]string{"old-sync": on})
	s := &V1Source{Root: src, Running: func(string) bool { return false }}
	if _, err := s.Convert("old.test"); err != nil {
		t.Fatal(err)
	}
	tg := &localTarget{rc: Receiver{Root: dst}, ready: false}
	m := &Mover{Root: src, Source: s, Parts: ImportParts, Poll: 10 * time.Millisecond, ReadyWait: 50 * time.Millisecond}
	if _, err := m.Move(context.Background(), "old.test", tg, nil); err == nil || !strings.Contains(err.Error(), "runs here again") {
		t.Fatalf("err: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(src, "tasks", "old-sync.json")); string(raw) != on {
		t.Fatalf("the V1 task was not put back as it was: %s", raw)
	}
	if _, err := os.Stat(tasks.Path(dst, "old-sync")); err == nil {
		t.Fatal("the task stayed on the target of a move that was undone")
	}
}

// A task whose name the target already has is a refusal before the site
// stops, not a site that arrives with part of its jobs.
func TestATaskNameTakenThereStopsTheMoveBeforeAnythingMoves(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	on := `{"title":"Sync","group":"old","schedule":"50 0 * * *","command":["/bin/true"],"site":"old.test"}`
	seedV1(t, src, map[string]string{"old-sync": on})
	os.MkdirAll(filepath.Join(dst, "tasks"), 0o755)
	os.WriteFile(tasks.Path(dst, "old-sync"), []byte(`{"version":2,"schedule":"* * * * *","command":["x"]}`), 0o644)
	s := &V1Source{Root: src, Running: func(string) bool { return false }}
	if _, err := s.Convert("old.test"); err != nil {
		t.Fatal(err)
	}
	tg := &checkingTarget{localTarget: localTarget{rc: Receiver{Root: dst}, ready: true}}
	m := &Mover{Root: src, Source: s, Parts: ImportParts, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
	if _, err := m.Move(context.Background(), "old.test", tg, nil); err == nil || !strings.Contains(err.Error(), "already here") {
		t.Fatalf("err: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(src, "sites", "old.test", "site.json")); strings.Contains(string(raw), "disabled") {
		t.Fatal("the site was stopped for a move refused at the check")
	}
}

// checkingTarget checks tasks the way the API's check does.
type checkingTarget struct{ localTarget }

func (t *checkingTarget) Check(ctx context.Context, d string, need Needs) error {
	for _, name := range need.Tasks {
		if err := t.rc.CheckTask(d, name, nil); err != nil {
			return err
		}
	}
	return t.localTarget.Check(ctx, d, need)
}

func TestAV1TaskConvertsWithWhatV1GaveItWithoutSaying(t *testing.T) {
	dir := t.TempDir()
	out, notes, err := ConvertV1Task([]byte(`{"title":"T","group":"g","schedule":"0 3 * * *","command":["tsx","a.ts"],"site":"a.test","overlap":"paralelo"}`), dir, "Europe/Madrid")
	if err != nil {
		t.Fatal(err)
	}
	got, err := tasks.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Timezone != "Europe/Madrid" || time.Duration(got.Timeout) != 30*time.Minute || got.Overlap != "queue" || got.Command[0] != "tsx" {
		t.Fatalf("converted: %s", out)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "from the machine's PATH") {
		t.Fatalf("a bare program not said: %v", notes)
	}
	if _, _, err := ConvertV1Task([]byte(`{"schedule":"0 3 * * *","command":["x"],"site":"a.test","retries":3}`), dir, ""); err == nil {
		t.Fatal("a field V2 does not carry was dropped in silence")
	}
}

// Between two nodes nothing runs a worker's move yet: the move is refused
// before the site stops, not finished on an empty database.
func TestAMoveWhoseDataMovesByCommandIsRefusedWhereNothingRunsIt(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	os.WriteFile(filepath.Join(src, "sites/a.test/site.json"), []byte(`{"version":2,"static":{"root":"public"},"workers":{"db":{"command":["x"],"move":{"export":["./e"],"import":["./i"]}}}}`), 0o644)
	s := &localSource{root: src}
	tg := &localTarget{rc: Receiver{Root: dst}, ready: true}
	m := &Mover{Root: src, Source: s, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
	var steps []string
	if _, err := m.Move(context.Background(), "a.test", tg, func(step, _ string) { steps = append(steps, step) }); err == nil || !strings.Contains(err.Error(), "moves by command") {
		t.Fatalf("err: %v", err)
	}
	if strings.Join(steps, " ") != "check" {
		t.Fatalf("it got as far as %v", steps)
	}
}

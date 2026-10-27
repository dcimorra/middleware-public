package relocate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
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

// A move command that fails says why in its error, not only in a log on
// the other machine.
func TestAFailedMoveCommandSaysTheEndOfItsOutput(t *testing.T) {
	dir := t.TempDir()
	spec := &manifest.Spec{Domain: "a.test/db/move", SiteDir: dir, Cwd: dir, Env: map[string]string{}, Runtime: "process",
		Command: []string{"sh", "-c", "echo restoring; echo 'rm: cannot remove x: Permission denied' >&2; exit 1"}}
	log := filepath.Join(dir, "move.log")
	err := runCommand(context.Background(), runtime.Docker{}, "", spec, 0, log, filepath.Join(dir, "home"), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err: %v", err)
	}
}

// What move.exclude names stays behind, folders and files alike, and the
// plan shown first says so; a folder whose contents are excluded arrives
// empty, for the site to fill again; nothing is installed inside what is
// left out.
func TestAMoveLeavesBehindWhatTheManifestExcludes(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	seed(t, src)
	site := filepath.Join(src, "sites/a.test")
	write := func(rel, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(site, rel)), 0o755)
		os.WriteFile(filepath.Join(site, rel), []byte(body), 0o644)
	}
	os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"version":2,"static":{"root":"public"},"move":{"exclude":["storage/products/*",".next-*/cache","*.log"]}}`), 0o644)
	write("storage/products/ab/1.webp", strings.Repeat("i", 50000))
	write("storage/uploads/u.png", "upload")
	write(".next-1/cache/images/x", strings.Repeat("c", 50000))
	write(".next-1/cache/tool/package.json", `{"dependencies":{"x":"1"}}`)
	os.MkdirAll(filepath.Join(site, ".next-1/cache/tool/node_modules"), 0o755)
	write(".next-1/server/page.js", "page")
	write("debug.log", "noise")

	m := &Mover{Root: src, Source: &localSource{root: src}, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
	plan := Analyze(site, m.PartsFor("a.test"), true, nil)
	for _, in := range plan.Installs {
		if strings.Contains(in.Dir, "cache") {
			t.Fatalf("an install inside what is left out: %+v", plan.Installs)
		}
	}
	if plan.Bytes > 250000+100000 {
		t.Fatalf("the plan counts what is left out: %d bytes", plan.Bytes)
	}
	if _, err := m.Move(context.Background(), "a.test", &localTarget{rc: Receiver{Root: dst}, ready: true}, nil); err != nil {
		t.Fatal(err)
	}
	got := filepath.Join(dst, "sites/a.test")
	for rel, want := range map[string]bool{
		"storage/uploads/u.png":      true,
		".next-1/server/page.js":     true,
		"storage/products":           true,
		"storage/products/ab/1.webp": false,
		".next-1/cache":              false,
		"debug.log":                  false,
	} {
		if _, err := os.Stat(filepath.Join(got, rel)); (err == nil) != want {
			t.Errorf("%s on the target: %v, want %v", rel, err == nil, want)
		}
	}
}

// --tasks-off: a site that stays on the target for a while, on a machine
// too small for its nightly jobs, gets its tasks there turned off; here
// they are turned off as always, and an undone move puts back the ones
// that were on.
func TestTasksOffHasEveryTaskArriveTurnedOff(t *testing.T) {
	src := t.TempDir()
	on := `{"title":"Sync","group":"old","schedule":"50 0 * * *","command":["/bin/true"],"site":"old.test"}`
	seedV1(t, src, map[string]string{"old-sync": on})
	s := &V1Source{Root: src, TasksOff: true, Running: func(string) bool { return false }}
	if _, err := s.Convert("old.test"); err != nil {
		t.Fatal(err)
	}
	files, err := s.Tasks("old.test")
	if err != nil {
		t.Fatal(err)
	}
	if tk, err := tasks.Parse(files["old-sync"]); err != nil || !tk.Disabled || tk.Title != "Sync" {
		t.Fatalf("the task arrives as %+v (%v)", tk, err)
	}
	notes, _ := s.TaskNotes("old.test")
	if !strings.Contains(strings.Join(notes, " "), "--tasks-off") {
		t.Fatalf("the plan does not say it: %v", notes)
	}
	held, err := s.HoldTasks("old.test")
	if err != nil || len(held) != 1 {
		t.Fatalf("held %v %v", held, err)
	}
	if err := s.ReleaseTasks(held); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(src, "tasks", "old-sync.json")); string(raw) != on {
		t.Fatalf("not put back as it was: %s", raw)
	}
}

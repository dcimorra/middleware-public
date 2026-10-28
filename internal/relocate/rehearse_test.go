package relocate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// rehearsingSource is a site of a house whose workers' exports run where
// the house runs them, and are tried before the stop.
type rehearsingSource struct {
	*localSource
	docker runtime.Docker
}

func (s *rehearsingSource) Export(ctx context.Context, d string, raw []byte, workers []string) (string, error) {
	m, err := manifest.Parse(raw)
	if err != nil {
		return "", err
	}
	return ExportHere(ctx, s.root, filepath.Join(s.root, "sites", d), d, m, workers)
}

func (s *rehearsingSource) Rehearse(ctx context.Context, d string, raw []byte, workers []string, trial bool) (string, error) {
	m, err := manifest.Parse(raw)
	if err != nil {
		return "", err
	}
	return RehearseHere(ctx, s.root, filepath.Join(s.root, "sites", d), d, m, workers, trial, s.docker)
}

// exportSite is a.test with a worker db whose export is export, running
// here as the engine records it: in container when it is set.
func exportSite(t *testing.T, root, export, container string) {
	t.Helper()
	seed(t, root)
	rt := ""
	if container != "" {
		rt = `"runtime":"docker","docker":{"image":"postgres:16-alpine"},`
	}
	os.WriteFile(filepath.Join(root, "sites/a.test/site.json"), []byte(`{"version":2,"static":{"root":"public"},
 "workers":{"db":{"command":["x"],`+rt+`"move":{"export":`+export+`,"output":".move/db.dump","import":["true"]}}}}`), 0o644)
	st := runtime.State{Domain: "a.test/db", Runtime: "process", PID: os.Getpid()}
	if container != "" {
		st = runtime.State{Domain: "a.test/db", Runtime: "docker", Container: container}
	}
	os.MkdirAll(filepath.Join(root, "data", "state"), 0o755)
	fsx.WriteJSON(runtime.StatePath(filepath.Join(root, "data"), "a.test/db"), st, 0o644)
}

// An export that cannot work is found while the site serves, with the
// command's own words, and nothing is stopped: it was found once the site
// was down for it. One written for the machine (docker exec, a client the
// machine has) runs inside the worker's container and is not there.
func TestAnExportThatCannotWorkIsFoundBeforeTheSiteStops(t *testing.T) {
	noProgram := filepath.Join(t.TempDir(), "docker")
	os.WriteFile(noProgram, []byte("#!/bin/sh\n[ \"$1\" = exec ] && exit 3\nexit 1\n"), 0o755)
	for _, c := range []struct {
		name, export, container, want string
	}{
		{"fails", `["sh","-c","echo 'pg_dump: error: connection refused' >&2; exit 2"]`, "", "connection refused"},
		{"no program", `["no-such-program-anywhere","--dump"]`, "", "is not on this machine"},
		{"not in its container", `["docker","exec","db","pg_dump"]`, "mw-a-test-db-5432", "is not inside its container"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			exportSite(t, src, c.export, c.container)
			s := &rehearsingSource{localSource: &localSource{root: src}, docker: runtime.Docker{Bin: noProgram}}
			tg := &noImportTarget{localTarget{rc: Receiver{Root: dst}, ready: true}}
			m := &Mover{Root: src, Source: s, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
			var steps []string
			_, err := m.Move(context.Background(), "a.test", tg, func(step, _ string) { steps = append(steps, step) })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err: %v", err)
			}
			if strings.Join(steps, " ") != "check" {
				t.Fatalf("it went as far as %v", steps)
			}
			if raw, _ := os.ReadFile(filepath.Join(src, "sites/a.test/site.json")); strings.Contains(string(raw), "disabled") {
				t.Fatal("the site was stopped")
			}
			if _, err := os.Stat(filepath.Join(dst, "data/relocate/a.test")); err == nil {
				t.Fatal("something was sent")
			}
			for _, p := range []string{"sites/a.test/.move/db.dump", "data/relocate/.rehearsal/a.test-db", "data/relocate/.rehearsal/a.test-db.part"} {
				if _, err := os.Stat(filepath.Join(src, p)); err == nil {
					t.Fatalf("%s left behind", p)
				}
			}
		})
	}
}

// An export that worked is not tried again for the next move of the same
// command in the same image; and the plan, which has seconds, only looks
// for its program and says whether it will be tried.
func TestAnExportThatWorkedIsNotTriedAgain(t *testing.T) {
	src := t.TempDir()
	runs := filepath.Join(t.TempDir(), "runs")
	exportSite(t, src, `["sh","-c","echo rows; echo ran >> `+runs+`"]`, "")
	s := &rehearsingSource{localSource: &localSource{root: src}}
	tg := &noImportTarget{localTarget{rc: Receiver{Root: t.TempDir()}, ready: true}}
	var said []string
	m := &Mover{Root: src, Source: s, NoTrial: true, Note: func(n string) { said = append(said, n) }}
	if _, err := m.Check(context.Background(), "a.test", tg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runs); err == nil || len(said) != 1 || !strings.Contains(said[0], "will be tried") {
		t.Fatalf("the plan ran it, or did not say it would be: %v", said)
	}
	m.NoTrial = false
	for i := 0; i < 2; i++ {
		if _, err := m.Check(context.Background(), "a.test", tg); err != nil {
			t.Fatal(err)
		}
	}
	if raw, _ := os.ReadFile(runs); strings.Count(string(raw), "ran") != 1 {
		t.Fatalf("tried %d times", strings.Count(string(raw), "ran"))
	}
	if !strings.Contains(said[len(said)-1], "worked here before") {
		t.Fatalf("said %v", said)
	}
	if _, err := os.Stat(filepath.Join(src, "sites/a.test/.move/db.dump")); err == nil {
		t.Fatal("the trial's dump went into the folder")
	}
}

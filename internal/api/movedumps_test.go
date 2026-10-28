package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/relocate"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// dumpSite is a site on f whose worker's data moves by command, its
// export's output a dump in the folder, and whose folder is a git
// repository. The import copies the dump to store, or fails.
func dumpSite(t *testing.T, f *fixture, store string, importOK bool) string {
	t.Helper()
	dir := filepath.Join(f.root, "sites", "d.test")
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755)
	os.WriteFile(filepath.Join(dir, "rows.txt"), []byte("the people's rows"), 0o644)
	imp := `["sh","-c","cp .move/db.dump ` + store + `"]`
	if !importOK {
		imp = `["sh","-c","echo the import failed >&2; exit 1"]`
	}
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{"root":"public"},
 "workers":{"db":{"command":["python3","-m","http.server","$PORT","--bind","127.0.0.1"],
  "move":{"export":["cat","rows.txt"],"output":".move/db.dump","import":`+imp+`}}}}`), 0o644)
	os.MkdirAll(filepath.Join(f.root, "data", "state"), 0o755)
	fsx.WriteJSON(runtime.StatePath(filepath.Join(f.root, "data"), "d.test/db"), runtime.State{Domain: "d.test/db", Runtime: "process", PID: os.Getpid()}, 0o644)
	return dir
}

// A database's dump is not left in a site's folder: there, once imported,
// it was put in place with the site and went with it on every later move;
// here, after a move that failed, it stayed. And while it is in a folder
// that is a git repository, git does not see it: the github module pushes
// that folder.
func TestADumpIsNotLeftInTheSitesFolder(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 stands in for the worker")
	}
	t.Run("moved", func(t *testing.T) {
		a, b := newFixture(t).withToken(), newFixture(t).withToken()
		store := filepath.Join(t.TempDir(), "volume")
		dumpSite(t, a, store, true)
		mgr, _ := a.srv.sitesManager()
		m := &relocate.Mover{Root: a.root, Source: siteSource{s: a.srv, mgr: mgr}, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
		rep, err := m.Move(context.Background(), "d.test", httpTarget{f: b}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(store); string(got) != "the people's rows" {
			t.Fatalf("imported %q", got)
		}
		there := filepath.Join(b.root, "sites", "d.test")
		if _, err := os.Stat(filepath.Join(there, ".move", "db.dump")); err == nil {
			t.Fatal("the dump was put in place with the site")
		}
		if ex, _ := os.ReadFile(filepath.Join(there, ".git", "info", "exclude")); !strings.Contains(string(ex), "/.move/db.dump\n") {
			t.Fatalf("not kept out of git there: %q", ex)
		}
		// Here, the archive keeps it: what the volume had, once the site is gone.
		if rep.Archive == "" {
			t.Fatal("not archived")
		}
	})
	t.Run("undone", func(t *testing.T) {
		a, b := newFixture(t).withToken(), newFixture(t).withToken()
		dir := dumpSite(t, a, "", false)
		mgr, _ := a.srv.sitesManager()
		m := &relocate.Mover{Root: a.root, Source: siteSource{s: a.srv, mgr: mgr}, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
		if _, err := m.Move(context.Background(), "d.test", httpTarget{f: b}, nil); err == nil || !strings.Contains(err.Error(), "runs here again") {
			t.Fatalf("err: %v", err)
		}
		for _, p := range []string{".move/db.dump", ".move/db.dump.part"} {
			if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
				t.Fatalf("%s left in the folder", p)
			}
		}
		if ex, _ := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude")); !strings.Contains(string(ex), "/.move/db.dump\n") || !strings.Contains(string(ex), "/.move/db.dump.part\n") {
			t.Fatalf("not kept out of git here: %q", ex)
		}
	})
}

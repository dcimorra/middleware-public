package relocate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// migratingTarget is a local target that runs the workers' imports for
// real, through the Receiver.
type migratingTarget struct{ localTarget }

func (t *migratingTarget) Migrate(ctx context.Context, d string, m []byte, workers []string) (string, error) {
	return t.rc.Migrate(ctx, d, m, workers)
}

const liveImage = "postgres:16-alpine"

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func waitPostgres(t *testing.T, container string, port int) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	// Over TCP: while it initialises, the image runs a temporary server on
	// the socket alone and then restarts it, and a dump started on the
	// socket's first answer fell in that restart.
	for exec.Command("docker", "exec", container, "pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-p", fmt.Sprint(port)).Run() != nil {
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
			t.Fatalf("%s never answered: %s", container, logs)
		}
		time.Sleep(time.Second)
	}
}

// v2Manifest is a static site with a PostgreSQL worker whose data moves by
// pg_dump where it is and psql where it goes.
func v2Manifest(srcContainer, importCmd string) string {
	return `{"version":2,"static":{"root":"public"},
 "workers":{"db":{"command":["postgres","-p","$PORT"],"runtime":"docker",
  "docker":{"image":"` + liveImage + `","user":"70:70","volumes":["pg:/var/lib/postgresql/data"]},
  "env":{"POSTGRES_HOST_AUTH_METHOD":"trust","PGDATA":"/var/lib/postgresql/data/pgdata"},
  "move":{
   "export":["sh","-c","mkdir -p .move && docker exec ` + srcContainer + ` pg_dump -U postgres > .move/db.sql"],
   "ready":["pg_isready","-h","127.0.0.1","-p","$PORT","-U","postgres"],
   "import":["sh","-c",` + fmt.Sprintf("%q", importCmd) + `]}}}}`
}

// The whole of it with a real docker and a real PostgreSQL: the data
// written where the site was is there on the target before the site is
// put in place, and an import that fails leaves no volume behind and the
// site where it was. DOCKER=1 runs it; it pulls postgres:16-alpine.
func TestAWorkersDatabaseMovesByItsOwnCommandsBeforeTheSiteStartsThere(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs it against the machine's docker")
	}
	stamp := fmt.Sprint(time.Now().UnixNano() % 1e9)
	src := "mwtest-src-" + stamp
	dockerOut(t, "run", "-d", "--name", src, "-e", "POSTGRES_HOST_AUTH_METHOD=trust", liveImage)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", src).Run() })
	waitPostgres(t, src, 5432)
	dockerOut(t, "exec", src, "psql", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", "create table t (v text); insert into t values ('from v1');")

	move := func(domain, importCmd string) (srcRoot, dstRoot string, err error) {
		srcRoot, dstRoot = t.TempDir(), t.TempDir()
		site := filepath.Join(srcRoot, "sites", domain)
		os.MkdirAll(filepath.Join(site, "public"), 0o755)
		os.WriteFile(filepath.Join(site, "public", "index.html"), []byte("hi"), 0o644)
		os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"title":"old","root":"public"}`), 0o644)
		os.WriteFile(filepath.Join(site, V2ManifestName), []byte(v2Manifest(src, importCmd)), 0o644)
		s := &V1Source{Root: srcRoot, Running: func(string) bool { return false }}
		if _, err := s.Convert(domain); err != nil {
			t.Fatal(err)
		}
		tg := &migratingTarget{localTarget{rc: Receiver{Root: dstRoot}, ready: true}}
		m := &Mover{Root: srcRoot, Source: s, Parts: ImportParts, Poll: 50 * time.Millisecond, ReadyWait: 5 * time.Second}
		var steps []string
		_, err = m.Move(context.Background(), domain, tg, func(step, msg string) { steps = append(steps, step) })
		t.Logf("%s: %s", domain, strings.Join(steps, " "))
		return srcRoot, dstRoot, err
	}

	vol := runtime.NamedVolume("ok.test", "pg")
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	_, dst, err := move("ok.test", "psql -U postgres -p $PORT -v ON_ERROR_STOP=1 < .move/db.sql")
	if err != nil {
		logs, _ := os.ReadFile(filepath.Join(dst, "data", "logs", "ok.test", "move-db.log"))
		t.Fatalf("%v\n%s", err, logs)
	}
	// What the site will find when its worker comes up on that volume.
	check := "mwtest-check-" + stamp
	dockerOut(t, "run", "-d", "--name", check, "--user", "70:70", "-e", "PGDATA=/var/lib/postgresql/data/pgdata", "-v", vol+":/var/lib/postgresql/data", liveImage)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", check).Run() })
	waitPostgres(t, check, 5432)
	if got := dockerOut(t, "exec", check, "psql", "-U", "postgres", "-tAc", "select v from t"); got != "from v1" {
		t.Fatalf("the target's database has %q", got)
	}
	if out, _ := exec.Command("docker", "ps", "-a", "--filter", "name=mw-ok-test-db", "--format", "{{.Names}}").Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the container brought up for the import was left: %s", out)
	}

	failed := runtime.NamedVolume("bad.test", "pg")
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", failed).Run() })
	srcRoot, _, err := move("bad.test", "exit 3")
	if err == nil || !strings.Contains(err.Error(), "runs here again") {
		t.Fatalf("a failed import: %v", err)
	}
	if exec.Command("docker", "volume", "inspect", failed).Run() == nil {
		t.Fatal("the volume of an import that failed was left behind")
	}
	if raw, _ := os.ReadFile(filepath.Join(srcRoot, "sites", "bad.test", "site.json")); strings.Contains(string(raw), "disabled") {
		t.Fatalf("the V1 site was not started again: %s", raw)
	}
}

// Between two V2 houses: the source's export runs inside the worker's own
// container as the database's user, who cannot write in the site's folder,
// so the house writes the dump (move.output) and the target imports it.
// DOCKER=1 runs it.
func TestAV2WorkersDatabaseIsExportedInsideItsContainer(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs it against the machine's docker")
	}
	src, dst := t.TempDir(), t.TempDir()
	site := filepath.Join(src, "sites", "v2.test")
	os.MkdirAll(site, 0o755)
	raw := []byte(`{"version":2,"static":{},"workers":{"db":{"command":["postgres","-p","$PORT"],"runtime":"docker","port":41979,
 "docker":{"image":"` + liveImage + `","user":"70:70","volumes":["pg:/var/lib/postgresql/data"]},
 "env":{"POSTGRES_HOST_AUTH_METHOD":"trust","PGDATA":"/var/lib/postgresql/data/pgdata"},
 "move":{"export":["pg_dump","-Fc","-U","postgres","-p","$PORT"],"output":".move/db.dump",
  "ready":["pg_isready","-h","127.0.0.1","-p","$PORT","-U","postgres"],
  "import":["pg_restore","-U","postgres","-p","$PORT","-d","postgres","--exit-on-error",".move/db.dump"]}}}}`)
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	vol := runtime.NamedVolume("v2.test", "pg")
	t.Cleanup(func() { exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	spec := manifest.WorkerSpec("v2.test", site, "db", m.Workers["db"])
	inst, err := runtime.Docker{}.Start(context.Background(), spec, 41979, filepath.Join(src, "data", "logs", "v2.test", "worker-db.log"), filepath.Join(src, "data", "homes", "v2.test"))
	if err != nil {
		t.Fatal(err)
	}
	name := inst.State().Container
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	fsx.WriteJSON(runtime.StatePath(filepath.Join(src, "data"), "v2.test/db"), inst.State(), 0o644)
	waitPostgres(t, name, 41979)
	dockerOut(t, "exec", name, "psql", "-U", "postgres", "-p", "41979", "-v", "ON_ERROR_STOP=1", "-c", "create table t (v text); insert into t values ('from the source node');")

	if _, err := ExportHere(context.Background(), src, site, "v2.test", m, []string{"db"}); err != nil {
		logs, _ := os.ReadFile(filepath.Join(src, "data", "logs", "v2.test", "move-db.log"))
		t.Fatalf("%v\n%s", err, logs)
	}
	dump := filepath.Join(site, ".move", "db.dump")
	if st, err := os.Stat(dump); err != nil || st.Size() < 100 {
		t.Fatalf("no dump: %v", err)
	}
	// The source's container goes, as the move archives the site, and the
	// target brings its own up on the same port.
	exec.Command("docker", "rm", "-f", name).Run()
	exec.Command("docker", "volume", "rm", "-f", vol).Run()

	r := Receiver{Root: dst}
	staged := r.StagingSite("v2.test")
	os.MkdirAll(filepath.Join(staged, ".move"), 0o755)
	b, _ := os.ReadFile(dump)
	os.WriteFile(filepath.Join(staged, ".move", "db.dump"), b, 0o644)
	if _, err := r.Migrate(context.Background(), "v2.test", raw, nil); err != nil {
		logs, _ := os.ReadFile(filepath.Join(dst, "data", "logs", "v2.test", "move-db.log"))
		t.Fatalf("%v\n%s", err, logs)
	}
	check := "mwtest-v2check"
	dockerOut(t, "run", "-d", "--name", check, "--user", "70:70", "-e", "PGDATA=/var/lib/postgresql/data/pgdata", "-v", vol+":/var/lib/postgresql/data", liveImage)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", check).Run() })
	waitPostgres(t, check, 5432)
	if got := dockerOut(t, "exec", check, "psql", "-U", "postgres", "-tAc", "select v from t"); got != "from the source node" {
		t.Fatalf("the target's database has %q", got)
	}
}

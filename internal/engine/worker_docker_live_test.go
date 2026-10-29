package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/ports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// A worker in a container, for real: it comes up behind the site's walls
// with a name docker accepts (a worker's key has a slash), a supervisor
// that comes back adopts it instead of reaping it as a leftover, and
// disabling it takes the container away. DOCKER=1 runs it; it pulls
// postgres:16-alpine.
func TestADockerWorkerRunsIsAdoptedAndIsNotReaped(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs it against the machine's docker")
	}
	root := t.TempDir()
	sites := filepath.Join(root, "sites")
	data := filepath.Join(root, "data")
	dir := filepath.Join(sites, "pg.test")
	os.MkdirAll(dir, 0o755)
	m := `{"version":2,"static":{},"workers":{"db":{"command":["postgres","-p","$PORT"],"runtime":"docker","port":41977,
 "docker":{"image":"postgres:16-alpine","user":"70:70","volumes":["pg:/var/lib/postgresql/data"]},
 "env":{"POSTGRES_HOST_AUTH_METHOD":"trust","PGDATA":"/var/lib/postgresql/data/pgdata"}}}}`
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644)
	container := runtime.ContainerName("pg.test/db", 41977)
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", container).Run()
		exec.Command("docker", "volume", "rm", "-f", runtime.NamedVolume("pg.test", "pg")).Run()
	})
	newEngine := func() *Engine {
		return New(Options{SitesDir: sites, DataDir: data, Ports: ports.New(41000, 41100), Bus: events.New(""), Tick: 200 * time.Millisecond,
			Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}, "docker": runtime.Docker{House: sites}}})
	}
	answers := func() bool {
		return exec.Command("docker", "exec", container, "pg_isready", "-U", "postgres", "-p", strconv.Itoa(41977)).Run() == nil
	}
	e := newEngine()
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	deadline := time.Now().Add(90 * time.Second)
	for !answers() {
		if time.Now().After(deadline) {
			sn, _ := e.Lookup("pg.test")
			logs, _ := os.ReadFile(filepath.Join(data, "logs", "pg.test", "worker-db.log"))
			t.Fatalf("the worker never answered: %+v\n%s", sn.Workers["db"], logs)
		}
		time.Sleep(time.Second)
	}
	// Its output is in the worker's log, where the panel reads it.
	deadline = time.Now().Add(10 * time.Second)
	for {
		logs, _ := os.ReadFile(filepath.Join(data, "logs", "pg.test", "worker-db.log"))
		if strings.Contains(string(logs), "ready to accept connections") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker's log does not have its output: %q", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// And its state names the container's id, which the guard measures by.
	var st runtime.State
	if err := fsx.ReadJSON(filepath.Join(data, "state", "pg.test@db.json"), &st); err != nil || len(st.ContainerID) < 12 {
		t.Fatalf("the worker's state has no container id: %+v %v", st, err)
	}
	cancel()
	time.Sleep(300 * time.Millisecond)

	e2 := newEngine()
	t.Cleanup(func() { stopEverything(e2) })
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go e2.Run(ctx2)
	time.Sleep(3 * time.Second) // adoption, the reaper and a few ticks
	if !answers() {
		t.Fatal("the worker's container did not survive the supervisor coming back: reaped or started again")
	}
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(strings.Replace(m, `"db":{`, `"db":{"disabled":true,`, 1)), 0o644)
	deadline = time.Now().Add(30 * time.Second)
	for {
		out, _ := exec.Command("docker", "ps", "-q", "--filter", "name="+container).Output()
		if strings.TrimSpace(string(out)) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a disabled docker worker is still running")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

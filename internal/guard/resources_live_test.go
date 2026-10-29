package guard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// A real container worker, read from the real /proc: it has memory and the
// guard finds it, whatever the daemon lets be read of its processes'
// working directory. DOCKER=1 runs it; it uses postgres:16-alpine.
func TestARealContainerWorkerIsMeasured(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs a real container")
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	site := filepath.Join(root, "sites", "m.test")
	os.MkdirAll(site, 0o755)
	m, err := manifest.Parse([]byte(`{"version":2,"workers":{"db":{"command":["postgres","-p","$PORT"],"runtime":"docker",
 "docker":{"image":"postgres:16-alpine","user":"70:70"},"env":{"POSTGRES_HOST_AUTH_METHOD":"trust","PGDATA":"/tmp/pgdata"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	spec := manifest.WorkerSpec("m.test", site, "db", m.Workers["db"])
	inst, err := runtime.Docker{}.Start(context.Background(), spec, 41988, filepath.Join(data, "logs", "m.test", "worker-db.log"), filepath.Join(data, "homes", "m.test"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", inst.State().Container).Run() })
	if err := fsx.WriteJSON(filepath.Join(data, "state", "m.test@db.json"), inst.State(), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	g := New(Options{DataDir: data, SitesDir: filepath.Join(root, "sites"), PortRange: [2]int{33000, 47000}, Bus: events.New(""),
		Sites: func() []SiteInfo { return []SiteInfo{{Domain: "m.test", Dir: site}} },
		Kill:  func(int) error { return nil }, Now: func() time.Time { return now }})
	g.now = func() time.Time { return now }
	time.Sleep(3 * time.Second) // postgres up and holding memory
	g.Sweep()
	now = now.Add(time.Minute)
	g.Sweep()
	w := g.Resources("m.test/db")
	if len(w.Samples) != 1 || w.Samples[0].RSS < 1<<20 {
		t.Fatalf("the container worker measured %+v", w.Samples)
	}
	t.Logf("db: %.1f%% cpu, %d MB", w.Samples[0].CPU, w.Samples[0].RSS>>20)
}

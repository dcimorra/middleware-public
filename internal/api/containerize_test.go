package api

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/containerize"
	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/ports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// containFixture is a house whose "docker" runtime is a plain process, so
// the trial runs here without a daemon; what goes in the manifest is the
// same either way.
func containFixture(t *testing.T, provider agent.Provider) (*fixture, *chat.Manager) {
	f := newFixture(t).withToken()
	e := engine.New(engine.Options{SitesDir: filepath.Join(f.root, "sites"), DataDir: filepath.Join(f.root, "data"), Ports: ports.New(41200, 41300), Bus: f.bus,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}, "docker": runtime.Process{}}})
	f.srv.opts.Engine = e
	reg := agent.NewRegistry()
	reg.Register(provider)
	m := chat.New(chat.Options{Root: f.root, Bus: f.bus, Registry: reg})
	probe := containerize.Probe{PythonVersion: func(context.Context) string { return "3.12.3" }}
	f.srv.RegisterContainerize(ContainOptions{Chat: m, Probe: &probe, Docker: func(context.Context) error { return nil }})
	return f, m
}

func waitContain(t *testing.T, f *fixture, domain string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, job := f.call("GET", "/api/v1/sites/"+domain+"/containerize", nil)
		if job["done"] == true {
			return job
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the containerization never ended")
	return nil
}

// What the house works out is tried before it is written, and written
// only when it answered.
func TestASiteGoesIntoAContainerOnlyAfterItAnsweredInOne(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3 to serve")
	}
	f, _ := containFixture(t, fakeProvider{key: "claude"})
	dir := filepath.Join(f.root, "sites", "c.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"run":{"command":["python3","-m","http.server","$PORT","--bind","127.0.0.1"]}}`), 0o644)
	resp, out := f.call("POST", "/api/v1/sites/c.test/containerize", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	job := waitContain(t, f, "c.test")
	if job["step"] != "done" {
		t.Fatalf("job: %v", job)
	}
	var m struct {
		Run struct {
			Command []string
			Runtime string
			Docker  struct{ Image string }
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "site.json"))
	json.Unmarshal(raw, &m)
	if m.Run.Runtime != "docker" || m.Run.Docker.Image != "python:3.12-slim" || len(m.Run.Command) != 6 {
		t.Fatalf("manifest: %s", raw)
	}
	resp, _ = f.call("POST", "/api/v1/sites/c.test/containerize", nil)
	if resp.StatusCode != 409 {
		t.Fatalf("a site already in a container: %d", resp.StatusCode)
	}
}

// When the house cannot, the agent picked is asked, with what was tried
// and how it failed, and the manifest is left as it was.
func TestWhatTheHouseCannotContainItAsksTheAgentPicked(t *testing.T) {
	release := make(chan struct{})
	f, m := containFixture(t, holdingProvider{key: "claude", release: release})
	var asked string
	// The agent's turn ends before the folders go: it writes into them.
	t.Cleanup(func() {
		close(release)
		for i := 0; i < 100; i++ {
			if c, err := m.History(asked); err != nil || !c.Running {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	dir := filepath.Join(f.root, "sites", "r.test")
	os.MkdirAll(dir, 0o755)
	manifestText := `{"version":2,"run":{"command":["ruby","app.rb"]}}`
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(manifestText), 0o644)
	if resp, out := f.call("POST", "/api/v1/sites/r.test/containerize", map[string]string{"provider": "claude"}); resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	job := waitContain(t, f, "r.test")
	id, _ := job["chat"].(string)
	if job["step"] != "asking" || id == "" || !strings.Contains(job["error"].(string), "ruby") {
		t.Fatalf("job: %v", job)
	}
	asked = chat.Name("r.test", id)
	c, err := m.History(asked)
	if err != nil || len(c.Turns) == 0 || !strings.Contains(c.Turns[0].Text, "Dockerfile") || !strings.Contains(c.Turns[0].Text, "ruby") {
		t.Fatalf("the agent's brief: %+v %v", c, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "site.json")); string(raw) != manifestText {
		t.Fatalf("the manifest was touched: %s", raw)
	}
	// Without an agent picked, it only says why.
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(manifestText), 0o644)
	f.call("POST", "/api/v1/sites/r.test/containerize", nil)
	if job := waitContain(t, f, "r.test"); job["step"] != "failed" || job["chat"] != nil {
		t.Fatalf("without an agent: %v", job)
	}
}

// With DOCKER=1, the whole of it against a real daemon: a Node site, the
// image worked out from the Node it runs on, pulled, started beside it on
// a spare port, answered, and only then written.
func TestALiveNodeSiteIsContainedWithItsOwnNodeMajor(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs a real container")
	}
	f := newFixture(t).withToken()
	sitesDir := filepath.Join(f.root, "sites")
	e := engine.New(engine.Options{SitesDir: sitesDir, DataDir: filepath.Join(f.root, "data"), Ports: ports.New(41200, 41300), Bus: f.bus})
	f.srv.opts.Engine = e
	f.srv.RegisterContainerize(ContainOptions{})
	dir := filepath.Join(sitesDir, "live.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "server.js"), []byte(`require("http").createServer((q, s) => s.end("inside")).listen(process.env.PORT, process.env.HOST || "0.0.0.0")`), 0o644)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"run":{"command":["node","server.js"],"health":{"path":"/"}}}`), 0o644)
	if resp, out := f.call("POST", "/api/v1/sites/live.test/containerize", nil); resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	deadline := time.Now().Add(5 * time.Minute)
	var job map[string]any
	for time.Now().Before(deadline) {
		_, job = f.call("GET", "/api/v1/sites/live.test/containerize", nil)
		if job["done"] == true {
			break
		}
		time.Sleep(time.Second)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "site.json"))
	if job["step"] != "done" || !strings.Contains(string(raw), `"runtime": "docker"`) || !strings.Contains(string(raw), `"image": "node:`) {
		t.Fatalf("job %v, manifest %s", job, raw)
	}
	t.Logf("%s; %s", job["how"], raw)
}

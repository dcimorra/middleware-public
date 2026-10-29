package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/runtime"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/relocate"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// httpTarget is the target node as the mesh would reach it, minus the
// mesh: the real routes of another server, over HTTP.
type httpTarget struct {
	f *fixture
}

func (t httpTarget) Name() string { return "node-b" }

func (t httpTarget) do(method, path, ctype string, body io.Reader, out any) error {
	req, _ := http.NewRequest(method, t.f.http.URL+"/api/v1"+path, body)
	req.Header.Set("Authorization", "Bearer "+t.f.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("%d %s", res.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (t httpTarget) Send(_ context.Context, d string, r io.Reader) error {
	return t.do("PUT", "/relocate/"+d, "application/gzip", r, nil)
}
func (t httpTarget) SendDelta(_ context.Context, d string, r io.Reader) error {
	return t.do("PUT", "/relocate/"+d+"/delta", "application/gzip", r, nil)
}
func (t httpTarget) Commit(_ context.Context, d string, m []byte, ts map[string][]byte) error {
	body, _ := json.Marshal(map[string]any{"manifest": base64.StdEncoding.EncodeToString(m), "tasks": relocate.EncodeTasks(ts)})
	return t.do("POST", "/relocate/"+d+"/commit", "application/json", bytes.NewReader(body), nil)
}
func (t httpTarget) Discard(_ context.Context, d string) error {
	return t.do("DELETE", "/relocate/"+d, "", nil, nil)
}
func (t httpTarget) Remove(_ context.Context, d string) error {
	return t.do("DELETE", "/sites/"+d, "", nil, nil)
}
func (t httpTarget) Ready(_ context.Context, d string) (bool, string, error) {
	var s sites.Site
	if err := t.do("GET", "/sites/"+d, "", nil, &s); err != nil {
		return false, "", err
	}
	ok, why := SiteReady(s)
	return ok, why, nil
}
func (t httpTarget) Publish(context.Context, string, string) error { return nil }
func (t httpTarget) Check(_ context.Context, d string, need relocate.Needs) error {
	body, _ := json.Marshal(need)
	return t.do("POST", "/relocate/"+d+"/check", "application/json", bytes.NewReader(body), nil)
}

func TestASiteMovesBetweenTwoServersThroughTheirOwnRoutes(t *testing.T) {
	a, b := newFixture(t).withToken(), newFixture(t).withToken()
	dir := filepath.Join(a.root, "sites", "m.test")
	os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	os.WriteFile(filepath.Join(dir, "public", "index.html"), []byte("moved"), 0o644)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{"root":"public"}}`), 0o644)
	os.MkdirAll(filepath.Join(a.root, "data", "chat"), 0o700)
	os.WriteFile(filepath.Join(a.root, "data", "chat", "m.test.json"), []byte(`{"turns":[]}`), 0o600)

	mgr, _ := a.srv.sitesManager()
	m := &relocate.Mover{Root: a.root, Source: siteSource{s: a.srv, mgr: mgr}, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
	rep, err := m.Move(context.Background(), "m.test", httpTarget{f: b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(b.root, "sites", "m.test", "public", "index.html")); string(got) != "moved" {
		t.Fatalf("index: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(b.root, "sites", "m.test", "site.json")); strings.Contains(string(got), "disabled") {
		t.Fatalf("it arrived stopped: %s", got)
	}
	if _, err := os.Stat(filepath.Join(b.root, "data", "chat", "m.test.json")); err != nil {
		t.Fatal("its conversation did not come along")
	}
	// Gone from here, kept in the archive.
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the source folder is still there")
	}
	if rep.Archive == "" {
		t.Fatal("nothing was archived here")
	}
	if _, err := os.Stat(rep.Archive); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// A name the target already has is refused before anything moves.
	os.MkdirAll(filepath.Join(a.root, "sites", "m.test"), 0o755)
	os.WriteFile(filepath.Join(a.root, "sites", "m.test", "site.json"), []byte(`{"version":2}`), 0o644)
	if _, err := m.Move(context.Background(), "m.test", httpTarget{f: b}, nil); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("moving onto a site of the same name: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(a.root, "sites", "m.test", "site.json")); strings.Contains(string(got), "disabled") {
		t.Fatal("a refused move stopped the site")
	}
}

func (t httpTarget) Migrate(_ context.Context, d string, m []byte, workers []string) (string, error) {
	body, _ := json.Marshal(map[string]any{"manifest": base64.StdEncoding.EncodeToString(m), "workers": workers})
	if err := t.do("POST", "/relocate/"+d+"/migrate", "application/json", bytes.NewReader(body), nil); err != nil {
		return "", err
	}
	for i := 0; i < 300; i++ {
		var j prepareJob
		if err := t.do("GET", "/relocate/"+d+"/migrate", "", nil, &j); err == nil && j.Done {
			if j.Error != "" {
				return "", errors.New(j.Error)
			}
			return j.Output, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", errors.New("the import never ended")
}

// A site with a worker whose data moves by command goes from one node to
// another: its export runs here (with the site held, or while it serves
// when the worker says live), its import there before the site is put in
// place. Between nodes this was refused: only an import from V1 ran it.
func TestASiteWithAWorkerMovesBetweenNodesWithItsData(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 stands in for the worker")
	}
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "held", true: "live"}[live], func(t *testing.T) {
			a, b := newFixture(t).withToken(), newFixture(t).withToken()
			dir := filepath.Join(a.root, "sites", "w.test")
			os.MkdirAll(filepath.Join(dir, "public"), 0o755)
			os.WriteFile(filepath.Join(dir, "data.txt"), []byte("rows here"), 0o644)
			liveField := ""
			if live {
				liveField = `,"live":true`
			}
			// The import writes where a database keeps its data, out of the
			// folder: what a live import writes in the folder, the last copy
			// replaces with the folder as it is where the site was.
			store := filepath.Join(t.TempDir(), "volume")
			os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"static":{"root":"public"},
 "workers":{"db":{"command":["python3","-m","http.server","$PORT","--bind","127.0.0.1"],
  "move":{"export":["sh","-c","mkdir -p .move && cp data.txt .move/dump"],
   "import":["sh","-c","cp .move/dump `+store+`"]`+liveField+`}}}}`), 0o644)
			// The worker as the engine records it running.
			os.MkdirAll(filepath.Join(a.root, "data", "state"), 0o755)
			fsx.WriteJSON(runtime.StatePath(filepath.Join(a.root, "data"), "w.test/db"), runtime.State{Domain: "w.test/db", Runtime: "process", PID: os.Getpid()}, 0o644)

			mgr, _ := a.srv.sitesManager()
			m := &relocate.Mover{Root: a.root, Source: siteSource{s: a.srv, mgr: mgr}, Poll: 20 * time.Millisecond, ReadyWait: 5 * time.Second}
			var steps []string
			if _, err := m.Move(context.Background(), "w.test", httpTarget{f: b}, func(s, _ string) { steps = append(steps, s) }); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(store); string(got) != "rows here" {
				t.Fatalf("the import did not run there with the export's data: %q", got)
			}
			order := strings.Join(steps, " ")
			pos := func(s string) int { return strings.Index(" "+order+" ", " "+s+" ") }
			if live && !(pos("export") < pos("copy") && strings.Index(order, "import") < pos("stop")) {
				t.Fatalf("live: export before the first copy and import before the stop: %s", order)
			}
			if !live && !(pos("stop") < pos("export") && pos("final") < strings.Index(order, "import")) {
				t.Fatalf("held: export once stopped and import after the last copy: %s", order)
			}
		})
	}
}

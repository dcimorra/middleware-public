package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// siteTasksFixture is two sites, a task at the root and one of the
// second site's, written as files the way the operator or that site would
// have.
func siteTasksFixture(t *testing.T, a, b string) *fixture {
	t.Helper()
	f := newFixture(t).withToken()
	for _, d := range []string{a, b} {
		if resp, body := f.call("POST", "/api/v1/sites", map[string]any{"domain": d}); resp.StatusCode != 201 {
			t.Fatalf("create %s: %d %v", d, resp.StatusCode, body)
		}
	}
	os.MkdirAll(filepath.Join(f.root, "tasks"), 0o755)
	for name, body := range map[string]string{
		"house":  `{"version": 2, "schedule": "0 3 * * *", "command": ["true"]}`,
		"theirs": `{"version": 2, "schedule": "0 4 * * *", "site": "` + b + `", "command": ["true"]}`,
	} {
		if err := os.WriteFile(tasks.Path(f.root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sched := tasks.NewScheduler(tasks.Options{Root: f.root, Bus: f.bus})
	f.srv.RegisterTasks(TasksOptions{Scheduler: sched, Root: f.root})
	return f
}

func (f *fixture) taskEvent(typ, subject string) (events.Event, bool) {
	recent := f.bus.Recent()
	for i := len(recent) - 1; i >= 0; i-- {
		if ev := recent[i]; ev.Type == typ && ev.Subject == subject {
			return ev, true
		}
	}
	return events.Event{}, false
}

func (f *fixture) body(path string) string {
	f.t.Helper()
	resp, body := f.call("GET", path, nil)
	if resp.StatusCode != 200 {
		f.t.Fatalf("GET %s: %d %v", path, resp.StatusCode, body)
	}
	return text(body)
}

// A site's routes list, write, run, read and delete its own tasks, and
// for anything else answer as if there were nothing there: another
// site's task and the house's are neither listed, nor replaced, nor run.
func TestASitesTasksAreItsOwnThroughTheSiteRoutes(t *testing.T) {
	f := siteTasksFixture(t, "a.test", "b.test")

	if got := strings.TrimSpace(f.body("/api/v1/sites/a.test/tasks")); got != "[]" {
		t.Fatalf("a site with no tasks lists none, not the house's: %q", got)
	}

	// No site in the body: it is the site's the route names.
	resp, body := f.call("PUT", "/api/v1/sites/a.test/tasks/mine", `{"version": 2, "schedule": "0 5 * * *", "command": ["sh", "-c", "echo mine ran in $PWD"]}`)
	if resp.StatusCode != 200 || body["name"] != "mine" || body["site"] != "a.test" {
		t.Fatalf("put: %d %v", resp.StatusCode, body)
	}
	if saved, err := tasks.Load(f.root, "mine"); err != nil || saved.Site != "a.test" {
		t.Fatalf("the file does not say a.test: %+v %v", saved, err)
	}
	if ev, ok := f.taskEvent("task.saved", "mine"); !ok || ev.Data["site"] != "a.test" || ev.Actor != "operator" {
		t.Fatalf("task.saved: %+v %v", ev, ok)
	}
	// Naming its own site is the same, and an edit of its own task is
	// what PUT is for; naming another site is refused.
	if resp, body := f.call("PUT", "/api/v1/sites/a.test/tasks/mine", `{"version": 2, "schedule": "0 6 * * *", "site": "a.test", "command": ["sh", "-c", "echo mine ran in $PWD"]}`); resp.StatusCode != 200 || body["schedule"] != "0 6 * * *" {
		t.Fatalf("an edit naming its own site: %d %v", resp.StatusCode, body)
	}
	if resp, body := f.call("PUT", "/api/v1/sites/a.test/tasks/other", `{"version": 2, "schedule": "0 5 * * *", "site": "b.test", "command": ["true"]}`); resp.StatusCode != 400 || !strings.Contains(text(body), "belongs to that site") {
		t.Fatalf("a task for another site: %d %v", resp.StatusCode, body)
	}
	if _, err := os.Stat(tasks.Path(f.root, "other")); !os.IsNotExist(err) {
		t.Fatal("the refused task was written")
	}
	if resp, body := f.call("PUT", "/api/v1/sites/a.test/tasks/broken", `{"version": 2, "schedule": "every tuesday", "command": ["true"]}`); resp.StatusCode != 400 {
		t.Fatalf("an invalid task: %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.call("PUT", "/api/v1/sites/a.test/tasks/Bad%20Name", `{}`); resp.StatusCode != 400 {
		t.Fatalf("an invalid name: %d", resp.StatusCode)
	}

	// A name taken at the root or by another site is not overwritten.
	for name, want := range map[string]string{"house": "the house's", "theirs": "belongs to b.test"} {
		before, _ := os.ReadFile(tasks.Path(f.root, name))
		resp, body := f.call("PUT", "/api/v1/sites/a.test/tasks/"+name, `{"version": 2, "schedule": "* * * * *", "command": ["true"]}`)
		if resp.StatusCode != 409 || !strings.Contains(text(body), want) {
			t.Errorf("overwriting %s: %d %v", name, resp.StatusCode, body)
		}
		if after, _ := os.ReadFile(tasks.Path(f.root, name)); string(after) != string(before) {
			t.Errorf("%s was overwritten:\n%s", name, after)
		}
	}

	// The list is the site's, a broken task of its own included.
	os.WriteFile(tasks.Path(f.root, "rotten"), []byte(`{"version": 2, "site": "a.test", "schedule": "never", "command": ["true"]}`), 0o644)
	var list []tasks.Status
	if err := json.Unmarshal([]byte(f.body("/api/v1/sites/a.test/tasks")), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Name != "mine" || list[1].Name != "rotten" || list[1].Error == "" || list[1].Site != "a.test" {
		t.Fatalf("the site's list: %+v", list)
	}
	if all := f.body("/api/v1/tasks"); !strings.Contains(all, `"house"`) || !strings.Contains(all, `"theirs"`) || !strings.Contains(all, `"mine"`) {
		t.Fatalf("the operator's list lost something: %s", all)
	}

	// Turning its own task off and on is the field and nothing else.
	if resp, body := f.call("POST", "/api/v1/sites/a.test/tasks/mine/disable", nil); resp.StatusCode != 200 || body["disabled"] != true {
		t.Fatalf("disable its own task: %d %v", resp.StatusCode, body)
	}
	if ev, ok := f.taskEvent("task.disabled", "mine"); !ok || ev.Data["site"] != "a.test" {
		t.Fatalf("task.disabled: %+v %v", ev, ok)
	}
	if resp, body := f.call("POST", "/api/v1/sites/a.test/tasks/mine/enable", nil); resp.StatusCode != 200 || body["disabled"] != nil {
		t.Fatalf("enable its own task: %d %v", resp.StatusCode, body)
	}
	if saved, err := tasks.Load(f.root, "mine"); err != nil || saved.Disabled || saved.Site != "a.test" || saved.Schedule != "0 6 * * *" {
		t.Fatalf("after off and on: %+v %v", saved, err)
	}

	// Another site's task and the house's are not there for this site.
	for _, name := range []string{"theirs", "house", "missing"} {
		for _, c := range [][2]string{{"DELETE", ""}, {"POST", "/run"}, {"POST", "/stop"}, {"POST", "/disable"}, {"POST", "/enable"}, {"GET", "/log"}} {
			if resp, body := f.call(c[0], "/api/v1/sites/a.test/tasks/"+name+c[1], nil); resp.StatusCode != 404 {
				t.Errorf("%s %s%s from a.test: %d %v", c[0], name, c[1], resp.StatusCode, body)
			}
		}
	}
	for _, name := range []string{"theirs", "house"} {
		if _, err := os.Stat(tasks.Path(f.root, name)); err != nil {
			t.Fatalf("%s is gone: %v", name, err)
		}
		if r := tasks.LoadRecord(f.root, name); len(r.Runs) > 0 {
			t.Fatalf("%s ran: %+v", name, r.Runs)
		}
	}

	// Its own: run, its output, stop when nothing runs, delete.
	resp, body = f.call("POST", "/api/v1/sites/a.test/tasks/mine/run", nil)
	if resp.StatusCode != 200 || body["ok"] != true {
		t.Fatalf("run: %d %v", resp.StatusCode, body)
	}
	waitRun(t, f.root, "mine")
	if log := f.body("/api/v1/sites/a.test/tasks/mine/log"); !strings.Contains(log, "mine ran in "+filepath.Join(f.root, "sites", "a.test")) {
		t.Fatalf("log (it runs in the site's folder): %q", log)
	}
	if resp, body := f.call("POST", "/api/v1/sites/a.test/tasks/mine/stop", nil); resp.StatusCode != 409 {
		t.Fatalf("stop with nothing running: %d %v", resp.StatusCode, body)
	}
	if resp, body := f.call("DELETE", "/api/v1/sites/a.test/tasks/mine", nil); resp.StatusCode != 200 || body["ok"] != true {
		t.Fatalf("delete: %d %v", resp.StatusCode, body)
	}
	if _, err := os.Stat(tasks.Path(f.root, "mine")); !os.IsNotExist(err) {
		t.Fatal("the task file is still there")
	}
	if _, err := os.Stat(tasks.LogPath(f.root, "mine")); err != nil {
		t.Fatalf("the log went with the task: %v", err)
	}
	if ev, ok := f.taskEvent("task.deleted", "mine"); !ok || ev.Data["site"] != "a.test" {
		t.Fatalf("task.deleted: %+v %v", ev, ok)
	}

	// A site that is not here has no tasks to list or write.
	if resp, _ := f.call("GET", "/api/v1/sites/nope.test/tasks", nil); resp.StatusCode != 404 {
		t.Fatalf("list for an unknown site: %d", resp.StatusCode)
	}
	if resp, _ := f.call("PUT", "/api/v1/sites/nope.test/tasks/x", `{"version": 2, "schedule": "0 5 * * *", "command": ["true"]}`); resp.StatusCode != 404 {
		t.Fatalf("put for an unknown site: %d", resp.StatusCode)
	}
	if _, err := os.Stat(tasks.Path(f.root, "x")); !os.IsNotExist(err) {
		t.Fatal("a task was written for a site that is not here")
	}
	if resp, _ := f.call("POST", "/api/v1/sites/nope.test/tasks/theirs/run", nil); resp.StatusCode != 404 {
		t.Fatalf("run for an unknown site: %d", resp.StatusCode)
	}
}

func waitRun(t *testing.T, root, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r := tasks.LoadRecord(root, name); len(r.Runs) > 0 {
			if r.Runs[0].Status != tasks.StatusOK {
				t.Fatalf("run: %+v", r.Runs[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the task did not run")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A tasks:<site> token is a service door to that site's tasks: not to
// another site's, not to the house's /tasks, not to anything else of the
// site, and what it writes is signed with its name.
func TestATasksTokenOpensItsSitesTasksAndNothingElse(t *testing.T) {
	f := siteTasksFixture(t, "a.test", "b.test")
	resp, body := f.call("POST", "/api/v1/auth/tokens", map[string]any{"name": "cron-ui", "scopes": []string{"tasks:a.test"}})
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	f.token = body["token"].(string)
	task := `{"version": 2, "schedule": "0 5 * * *", "command": ["true"]}`
	if resp, body := f.call("PUT", "/api/v1/sites/a.test/tasks/mine", task); resp.StatusCode != 200 || body["site"] != "a.test" {
		t.Fatalf("its own site: %d %v", resp.StatusCode, body)
	}
	if ev, ok := f.taskEvent("task.saved", "mine"); !ok || ev.Actor != "token:cron-ui" {
		t.Fatalf("task.saved: %+v %v", ev, ok)
	}
	if list := f.body("/api/v1/sites/a.test/tasks"); !strings.Contains(list, `"mine"`) {
		t.Fatalf("its own list: %s", list)
	}
	for _, c := range [][2]string{
		{"PUT", "/api/v1/sites/b.test/tasks/mine"},
		{"GET", "/api/v1/sites/b.test/tasks"},
		{"GET", "/api/v1/tasks"},
		{"PUT", "/api/v1/tasks/mine"},
		{"POST", "/api/v1/tasks/mine/run"},
		{"DELETE", "/api/v1/tasks/theirs"},
		{"GET", "/api/v1/sites/a.test"},
		{"POST", "/api/v1/sites/a.test/restart"},
	} {
		if resp, body := f.call(c[0], c[1], task); resp.StatusCode != 403 {
			t.Errorf("%s %s with tasks:a.test: %d %v", c[0], c[1], resp.StatusCode, body)
		}
	}
	if saved, err := tasks.Load(f.root, "theirs"); err != nil || saved.Site != "b.test" {
		t.Fatalf("b.test's task: %+v %v", saved, err)
	}
}

// A site's agent turn manages its own site's tasks from its Bash calls,
// as `middleware site tasks` does over the socket, and is refused on
// another site's and on the house's.
func TestASiteTurnManagesItsOwnTasksOverTheSocket(t *testing.T) {
	f := siteTasksFixture(t, "a.example", "b.example")
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	// This test's process is the turn: the helper is its child.
	turns := map[int]string{os.Getpid(): "a.example"}
	siteOf := func(pid int) (string, error) { return chat.SiteOfProcess("/proc", pid, turns) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := SocketServer(ctx, f.srv.Panel(), siteOf)
	go srv.Serve(ln)
	defer srv.Close()

	task := `{"version": 2, "schedule": "0 5 * * *", "command": ["true"]}`
	calls := []string{
		"PUT /api/v1/sites/a.example/tasks/mine " + task,
		"GET /api/v1/sites/a.example/tasks",
		"POST /api/v1/sites/a.example/tasks/mine/run",
		"PUT /api/v1/sites/a.example/tasks/theirs " + task,
		"GET /api/v1/sites/b.example/tasks",
		"PUT /api/v1/sites/b.example/tasks/mine " + task,
		"DELETE /api/v1/sites/b.example/tasks/theirs",
		"GET /api/v1/tasks",
		"PUT /api/v1/tasks/other " + task,
		"DELETE /api/v1/tasks/house",
	}
	want := []int{200, 200, 200, 409, 403, 403, 403, 403, 403, 403}
	got := peer(t, sock, nil, calls...)
	for i := range calls {
		if got[i] != want[i] {
			t.Errorf("site turn: %s -> %d, want %d", calls[i], got[i], want[i])
		}
	}
	if saved, err := tasks.Load(f.root, "mine"); err != nil || saved.Site != "a.example" {
		t.Fatalf("the turn's task: %+v %v", saved, err)
	}
	if ev, ok := f.taskEvent("task.saved", "mine"); !ok || ev.Actor != "agent" {
		t.Fatalf("task.saved: %+v %v", ev, ok)
	}
	for _, name := range []string{"house", "theirs"} {
		if _, err := os.Stat(tasks.Path(f.root, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(tasks.Path(f.root, "other")); !os.IsNotExist(err) {
		t.Fatal("the turn wrote a task at the root")
	}
	waitRun(t, f.root, "mine")
}

package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/tasks"
)

func TestTasksAreWrittenListedRunAndDeletedThroughTheAPI(t *testing.T) {
	f := newFixture(t).withToken()
	os.MkdirAll(filepath.Join(f.root, "tasks"), 0o755)
	sched := tasks.NewScheduler(tasks.Options{Root: f.root, Bus: f.bus})
	f.srv.RegisterTasks(TasksOptions{Scheduler: sched, Root: f.root})

	resp, body := f.call("GET", "/api/v1/tasks", nil)
	if resp.StatusCode != 200 || strings.TrimSpace(text(body)) != "[]" {
		t.Fatalf("empty list: %d %q", resp.StatusCode, text(body))
	}
	resp, body = f.call("PUT", "/api/v1/tasks/hello", `{"version": 2, "schedule": "0 3 * * *", "command": ["sh", "-c", "echo hi from the task"]}`)
	if resp.StatusCode != 200 || body["name"] != "hello" {
		t.Fatalf("put: %d %v", resp.StatusCode, body)
	}
	resp, body = f.call("PUT", "/api/v1/tasks/broken", `{"version": 2, "schedule": "every tuesday", "command": ["true"]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid task must be 400: %d %v", resp.StatusCode, body)
	}
	resp, _ = f.call("PUT", "/api/v1/tasks/Bad Name", `{}`)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid name must be 400: %d", resp.StatusCode)
	}
	resp, body = f.call("POST", "/api/v1/tasks/hello/run", nil)
	if resp.StatusCode != 200 || body["ok"] != true {
		t.Fatalf("run: %d %v", resp.StatusCode, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r := tasks.LoadRecord(f.root, "hello"); len(r.Runs) > 0 {
			if r.Runs[0].Status != tasks.StatusOK {
				t.Fatalf("run: %+v", r.Runs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task did not run")
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp, body = f.call("GET", "/api/v1/tasks/hello/log", nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || !strings.Contains(text(body), "hi from the task") {
		t.Fatalf("log: %d %q", resp.StatusCode, text(body))
	}
	resp, _ = f.call("POST", "/api/v1/tasks/missing/run", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("run of a missing task: %d", resp.StatusCode)
	}
	resp, _ = f.call("DELETE", "/api/v1/tasks/hello", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := os.Stat(tasks.Path(f.root, "hello")); !os.IsNotExist(err) {
		t.Fatal("task file still there")
	}
}

func TestLastLinesKeepsTheTail(t *testing.T) {
	data := []byte("a\nb\nc\nd\n")
	if got := string(lastLines(data, 2)); got != "c\nd\n" {
		t.Fatalf("got %q", got)
	}
	if got := string(lastLines(data, 10)); got != "a\nb\nc\nd\n" {
		t.Fatalf("got %q", got)
	}
}

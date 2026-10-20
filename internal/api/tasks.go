package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/tasks"
)

// TasksOptions wire the tasks section.
type TasksOptions struct {
	Scheduler *tasks.Scheduler
	// Root is where tasks/ and data/logs/tasks/ live.
	Root string
}

// logLines is how much of a task's log GET /tasks/{name}/log returns.
const logLines = 500

// RegisterTasks adds the /tasks routes.
func (s *Server) RegisterTasks(o TasksOptions) {
	p := Prefix + "/tasks"
	s.Handle("GET", p, "The tasks with their next and last run", func(w http.ResponseWriter, r *http.Request) (any, error) {
		list := o.Scheduler.List()
		if list == nil {
			list = []tasks.Status{}
		}
		return list, nil
	})
	s.Handle("PUT", p+"/{name}", "Write a task (validated)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "body: %v", err)
		}
		t, err := tasks.Parse(data)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "task: %v", err)
		}
		if err := tasks.Save(o.Root, name, t); err != nil {
			return nil, err
		}
		s.opts.Bus.Emit(events.Event{Type: "task.saved", Subject: name, Actor: "operator", Data: map[string]any{"schedule": t.Schedule, "site": t.Site}})
		for _, st := range o.Scheduler.List() {
			if st.Name == name {
				return st, nil
			}
		}
		return t, nil
	})
	s.Handle("DELETE", p+"/{name}", "Delete a task; its history and log stay", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(tasks.Path(o.Root, name)); err != nil {
			return nil, err
		}
		s.opts.Bus.Emit(events.Event{Type: "task.deleted", Subject: name, Actor: "operator"})
		return OK, nil
	})
	s.Handle("POST", p+"/{name}/run", "Run a task now; progress is on the event stream as task.*", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		if _, err := tasks.Load(o.Root, name); err != nil {
			if os.IsNotExist(err) {
				return nil, Errorf(http.StatusNotFound, "no task %q", name)
			}
			return nil, Errorf(http.StatusBadRequest, "task: %v", err)
		}
		// RunNow waits for the task to end, which can be an hour; the
		// panel gets its answer now and the task.* events after.
		go func() {
			if err := o.Scheduler.RunNow(name); err != nil && errors.Is(err, tasks.ErrRunning) {
				s.opts.Bus.Emit(events.Event{Type: "task.skipped", Subject: name, Actor: "operator", Level: "warn", Message: err.Error()})
			}
		}()
		return map[string]any{"ok": true, "log": tasks.LogPath(o.Root, name)}, nil
	})
	s.Handle("POST", p+"/{name}/stop", "Stop a run in progress; it is recorded as stopped", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		if err := o.Scheduler.Stop(name); err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		return OK, nil
	})
	s.Handle("GET", p+"/{name}/log", "The last lines of a task's output", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		n := logLines
		if v := r.URL.Query().Get("lines"); v != "" {
			if i, err := strconv.Atoi(v); err == nil && i > 0 {
				n = i
			}
		}
		data, err := os.ReadFile(tasks.LogPath(o.Root, name))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(lastLines(data, n))
		return Handled, nil
	})
}

func taskName(r *http.Request) (string, error) {
	name := r.PathValue("name")
	if !tasks.ValidName(name) {
		return "", Errorf(http.StatusBadRequest, "%q is not a task name", name)
	}
	return name, nil
}

// lastLines keeps the final n lines. A task log is bounded by the
// scheduler's own rotation, so reading it whole is fine.
func lastLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return data
	}
	trimmed := bytes.TrimRight(data, "\n")
	pos := len(trimmed)
	for i := 0; i < n; i++ {
		j := bytes.LastIndexByte(trimmed[:pos], '\n')
		if j < 0 {
			return data
		}
		pos = j
	}
	return data[pos+1:]
}

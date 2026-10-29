package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/tasks"
)

// TasksOptions wire the tasks section.
type TasksOptions struct {
	Scheduler *tasks.Scheduler
	// Root is where tasks/ and data/logs/tasks/ live.
	Root string
}

// logLines is how much of a task's log GET /tasks/{name}/log returns.
const logLines = 500

// RegisterTasks adds the /tasks routes, the operator's over every task
// file, and /sites/{domain}/tasks, a site's over its own. They are one
// set of handlers: the site's routes only add, before them, that the
// task is that site's. Two copies of "run a task" would drift the first
// time one of them learned something.
func (s *Server) RegisterTasks(o TasksOptions) {
	tr := taskRoutes{s: s, o: o}
	p := Prefix + "/tasks"
	s.Handle("GET", p, "The tasks with their next and last run", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return tr.list(""), nil
	})
	s.Handle("PUT", p+"/{name}", "Write a task (validated)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		t, err := readTask(w, r)
		if err != nil {
			return nil, err
		}
		return tr.save(r, name, t)
	})
	s.Handle("DELETE", p+"/{name}", "Delete a task; its history and log stay", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		return tr.remove(r, name, "")
	})
	s.Handle("POST", p+"/{name}/run", "Run a task now; progress is on the event stream as task.*", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		return tr.run(r, name)
	})
	s.Handle("POST", p+"/{name}/stop", "Stop a run in progress; it is recorded as stopped", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		return tr.stop(name)
	})
	s.Handle("POST", p+"/{name}/disable", "Turn a task off (writes disabled: true); a run in progress ends on its own", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		return tr.setDisabled(r, name, "", true)
	})
	s.Handle("POST", p+"/{name}/enable", "Turn a task on again (clears disabled)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		return tr.setDisabled(r, name, "", false)
	})
	s.Handle("GET", p+"/{name}/log", "The last lines of a task's output", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		return tr.log(w, r, name)
	})

	// A site's own tasks. Names stay one space for the whole house (the
	// file is tasks/<name>.json whoever wrote it), so what makes a task a
	// site's is its "site" field, and every route here checks it before
	// doing anything: these are what a site's token and its agent's turn
	// reach (the tasks scope), and a task that runs at the root or in
	// another site's folder is not theirs to read, run or replace.
	sp := Prefix + "/sites/{domain}/tasks"
	s.Handle("GET", sp, "The site's tasks (the ones whose site is this one) with their next and last run", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := tr.site(r)
		if err != nil {
			return nil, err
		}
		return tr.list(domain), nil
	})
	s.Handle("PUT", sp+"/{name}", "Write one of the site's tasks (validated; site defaults to this one, another site is 400, a name taken by another site or the root is 409)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := tr.site(r)
		if err != nil {
			return nil, err
		}
		name, err := taskName(r)
		if err != nil {
			return nil, err
		}
		t, err := readTask(w, r)
		if err != nil {
			return nil, err
		}
		switch t.Site {
		case "":
			t.Site = domain
		case domain:
		default:
			return nil, Errorf(http.StatusBadRequest, "site: a site's task belongs to that site; %s cannot write one for %s", domain, t.Site)
		}
		// Overwriting is how a task is edited, so only someone else's
		// file is refused: replacing a root task with one of the same
		// name would quietly move the house's backup into a site's folder.
		if owner, exists := taskOwner(o.Root, name); exists && owner != domain {
			if owner == "" {
				return nil, Errorf(http.StatusConflict, "task %q is the house's (it runs at the root, or its file does not say whose it is): choose another name", name)
			}
			return nil, Errorf(http.StatusConflict, "task %q belongs to %s: choose another name", name, owner)
		}
		return tr.save(r, name, t)
	})
	s.Handle("DELETE", sp+"/{name}", "Delete one of the site's tasks; its history and log stay", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, name, err := tr.siteTask(r)
		if err != nil {
			return nil, err
		}
		return tr.remove(r, name, domain)
	})
	s.Handle("POST", sp+"/{name}/run", "Run one of the site's tasks now; progress is on the event stream as task.*", func(w http.ResponseWriter, r *http.Request) (any, error) {
		_, name, err := tr.siteTask(r)
		if err != nil {
			return nil, err
		}
		return tr.run(r, name)
	})
	s.Handle("POST", sp+"/{name}/stop", "Stop a run in progress of one of the site's tasks", func(w http.ResponseWriter, r *http.Request) (any, error) {
		_, name, err := tr.siteTask(r)
		if err != nil {
			return nil, err
		}
		return tr.stop(name)
	})
	s.Handle("POST", sp+"/{name}/disable", "Turn one of the site's tasks off (writes disabled: true)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, name, err := tr.siteTask(r)
		if err != nil {
			return nil, err
		}
		return tr.setDisabled(r, name, domain, true)
	})
	s.Handle("POST", sp+"/{name}/enable", "Turn one of the site's tasks on again (clears disabled)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, name, err := tr.siteTask(r)
		if err != nil {
			return nil, err
		}
		return tr.setDisabled(r, name, domain, false)
	})
	s.Handle("GET", sp+"/{name}/log", "The last lines of one of the site's tasks' output", func(w http.ResponseWriter, r *http.Request) (any, error) {
		_, name, err := tr.siteTask(r)
		if err != nil {
			return nil, err
		}
		return tr.log(w, r, name)
	})
}

// taskRoutes are the bodies both route sets share.
type taskRoutes struct {
	s *Server
	o TasksOptions
}

// site is the {domain} of a site's tasks route: a valid name (400) and a
// site or project here (404), so a typo is not an empty list.
func (tr taskRoutes) site(r *http.Request) (string, error) {
	m, err := tr.s.sitesManager()
	if err != nil {
		return "", err
	}
	domain, err := domainOf(r)
	if err != nil {
		return "", err
	}
	if _, err := m.Get(domain); err != nil {
		return "", Errorf(http.StatusNotFound, "%s: no such site", domain)
	}
	return domain, nil
}

// siteTask is the site and the task of a route on one of its tasks; a
// task that is not there and one that is someone else's are the same
// 404, so a site cannot tell the other sites' task names by asking.
func (tr taskRoutes) siteTask(r *http.Request) (string, string, error) {
	domain, err := tr.site(r)
	if err != nil {
		return "", "", err
	}
	name, err := taskName(r)
	if err != nil {
		return "", "", err
	}
	if owner, exists := taskOwner(tr.o.Root, name); !exists || owner != domain {
		return "", "", Errorf(http.StatusNotFound, "%s has no task %q", domain, name)
	}
	return domain, name, nil
}

// taskOwner is the site a task file names and whether the file is there.
// It reads the field alone, as tasks.OfSite does, so a site's task that
// no longer validates is still the site's to fix or remove; a file that
// is not even JSON says nothing, and is nobody's but the house's.
func taskOwner(root, name string) (string, bool) {
	data, err := os.ReadFile(tasks.Path(root, name))
	if err != nil {
		return "", !os.IsNotExist(err)
	}
	var loose struct {
		Site string `json:"site"`
	}
	if json.Unmarshal(data, &loose) != nil {
		return "", true
	}
	return loose.Site, true
}

// list is the scheduler's list, or the part of it that is site's. A
// task that does not parse has no site in its row; its file is read for
// it, so a broken task of the site is a line with an error on the site's
// screen and not an absence.
func (tr taskRoutes) list(site string) []tasks.Status {
	out := []tasks.Status{}
	for _, st := range tr.o.Scheduler.List() {
		if site == "" {
			out = append(out, st)
			continue
		}
		if st.Site == "" && st.Error != "" {
			st.Site, _ = taskOwner(tr.o.Root, st.Name)
		}
		if st.Site == site {
			out = append(out, st)
		}
	}
	return out
}

func readTask(w http.ResponseWriter, r *http.Request) (*tasks.Task, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "body: %v", err)
	}
	t, err := tasks.Parse(data)
	if err != nil {
		return nil, Errorf(http.StatusBadRequest, "task: %v", err)
	}
	return t, nil
}

func (tr taskRoutes) save(r *http.Request, name string, t *tasks.Task) (any, error) {
	if err := tasks.Save(tr.o.Root, name, t); err != nil {
		return nil, err
	}
	tr.s.opts.Bus.Emit(events.Event{Type: "task.saved", Subject: name, Actor: actorOf(r), Data: map[string]any{"schedule": t.Schedule, "site": t.Site}})
	for _, st := range tr.o.Scheduler.List() {
		if st.Name == name {
			return st, nil
		}
	}
	return t, nil
}

// remove deletes the file; site is only what the event says.
func (tr taskRoutes) remove(r *http.Request, name, site string) (any, error) {
	if err := os.Remove(tasks.Path(tr.o.Root, name)); err != nil {
		return nil, err
	}
	ev := events.Event{Type: "task.deleted", Subject: name, Actor: actorOf(r)}
	if site != "" {
		ev.Data = map[string]any{"site": site}
	}
	tr.s.opts.Bus.Emit(ev)
	return OK, nil
}

func (tr taskRoutes) run(r *http.Request, name string) (any, error) {
	if _, err := tasks.Load(tr.o.Root, name); err != nil {
		if os.IsNotExist(err) {
			return nil, Errorf(http.StatusNotFound, "no task %q", name)
		}
		return nil, Errorf(http.StatusBadRequest, "task: %v", err)
	}
	actor := actorOf(r)
	// RunNow waits for the task to end, which can be an hour; the
	// panel gets its answer now and the task.* events after.
	go func() {
		if err := tr.o.Scheduler.RunNow(name); err != nil && errors.Is(err, tasks.ErrRunning) {
			tr.s.opts.Bus.Emit(events.Event{Type: "task.skipped", Subject: name, Actor: actor, Level: "warn", Message: err.Error()})
		}
	}()
	return map[string]any{"ok": true, "log": tasks.LogPath(tr.o.Root, name)}, nil
}

// setDisabled writes the decision to the task's file, as a stopped site
// writes it to its manifest: a switch kept in memory would turn the task
// back on at the next restart. Only the field changes, so the panel's
// switch does not need the whole JSON round-tripped through an editor.
func (tr taskRoutes) setDisabled(r *http.Request, name, site string, disabled bool) (any, error) {
	if _, err := tasks.Load(tr.o.Root, name); err != nil {
		if os.IsNotExist(err) {
			return nil, Errorf(http.StatusNotFound, "no task %q", name)
		}
		return nil, Errorf(http.StatusBadRequest, "task: %v", err)
	}
	if err := tasks.SetDisabled(tr.o.Root, name, disabled); err != nil {
		return nil, err
	}
	ev := events.Event{Type: "task.enabled", Subject: name, Actor: actorOf(r)}
	if disabled {
		ev.Type = "task.disabled"
	}
	if site != "" {
		ev.Data = map[string]any{"site": site}
	}
	tr.s.opts.Bus.Emit(ev)
	for _, st := range tr.o.Scheduler.List() {
		if st.Name == name {
			return st, nil
		}
	}
	return OK, nil
}

func (tr taskRoutes) stop(name string) (any, error) {
	if err := tr.o.Scheduler.Stop(name); err != nil {
		return nil, Errorf(http.StatusConflict, "%v", err)
	}
	return OK, nil
}

func (tr taskRoutes) log(w http.ResponseWriter, r *http.Request, name string) (any, error) {
	n := logLines
	if v := r.URL.Query().Get("lines"); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			n = i
		}
	}
	data, err := os.ReadFile(tasks.LogPath(tr.o.Root, name))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(lastLines(data, n))
	return Handled, nil
}

// actorOf is who an event says acted: a site's agent turn is "agent", as
// its refusals and its pushes are; a delegated user is named, where they
// signed in and on the node their request was relayed to; a scoped token is the service door it
// is, by name, since the audit trail is where an operator looks for which
// program changed a site's tasks; a node is named too. Everyone else (the
// panel, the socket, a token without scopes) is the operator.
func actorOf(r *http.Request) string {
	c := auth.FromContext(r.Context())
	switch {
	case c.Turn != "":
		return "agent"
	case c.Delegate():
		return "user:" + c.User
	case c.Via == auth.ViaNode:
		return "node:" + c.Token
	case c.Via == auth.ViaToken && len(c.Scopes) > 0:
		return "token:" + c.Token
	}
	return "operator"
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

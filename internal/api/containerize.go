package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/containerize"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Putting a site in a container, from one button. The house works the
// container out when it can (containerize.For), tries it beside the
// generation that serves (engine.Trial), and only then writes it in the
// manifest, so the handover that follows is the zero-downtime one every
// manifest change gets. When it cannot, or the try fails, the agent the
// operator picked is asked, in a conversation of its own, with what was
// tried and how it failed: an agent may not run docker from its jail, so
// it writes the Dockerfile and the manifest's run.docker, and the house
// tries that the next time the button is pressed. Who decides what is
// written may be a model; who starts the container is the house.

// containJob is one site's containerization: its step, or how it ended.
type containJob struct {
	Step    string    `json:"step"` // planning | trying | asking | done | failed
	How     string    `json:"how,omitempty"`
	Error   string    `json:"error,omitempty"`
	Chat    string    `json:"chat,omitempty"` // the conversation the agent was asked in
	Done    bool      `json:"done"`
	Started time.Time `json:"started"`
}

type containJobs struct {
	mu   sync.Mutex
	jobs map[string]*containJob
}

// ContainOptions wire the containerization routes.
type ContainOptions struct {
	Chat *chat.Manager
	// Probe and Docker are replaced by tests; Docker says whether a
	// daemon answers.
	Probe  *containerize.Probe
	Docker func(ctx context.Context) error
}

// RegisterContainerize mounts POST and GET /sites/{domain}/containerize.
func (s *Server) RegisterContainerize(o ContainOptions) {
	probe := containerize.Machine()
	if o.Probe != nil {
		probe = *o.Probe
	}
	docker := o.Docker
	if docker == nil {
		docker = dockerAnswers
	}
	s.Handle("POST", Prefix+"/sites/{domain}/containerize", "Put the site in a container (body: provider?, model?): worked out and tried beside the one that serves before it is written; when it cannot be, the agent named is asked in a new conversation of the site", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if r.ContentLength != 0 {
			if err := ReadJSON(r, &body); err != nil {
				return nil, err
			}
		}
		mgr, err := s.sitesManager()
		if err != nil {
			return nil, err
		}
		site, err := mgr.Get(domain)
		if err != nil {
			return nil, err
		}
		if !site.Managed || site.Manifest == nil {
			return nil, Errorf(http.StatusConflict, "%s has no manifest the house manages", domain)
		}
		if site.Manifest.Run != nil && site.Manifest.Run.Runtime == "docker" {
			return nil, Errorf(http.StatusConflict, "%s already runs in a container", domain)
		}
		if s.opts.Engine == nil {
			return nil, Errorf(http.StatusNotImplemented, "no engine")
		}
		// Asked now: without a daemon there is nothing to try and nothing
		// an agent can fix from its folder.
		if err := docker(r.Context()); err != nil {
			return nil, Errorf(http.StatusConflict, "%v", err)
		}
		s.contains.mu.Lock()
		if s.contains.jobs == nil {
			s.contains.jobs = map[string]*containJob{}
		}
		if j := s.contains.jobs[domain]; j != nil && !j.Done {
			s.contains.mu.Unlock()
			return nil, Errorf(http.StatusConflict, "%s is already being put in a container (%s)", domain, j.Step)
		}
		job := &containJob{Step: "planning", Started: time.Now()}
		s.contains.jobs[domain] = job
		s.contains.mu.Unlock()
		dir := mgr.Dir(domain)
		m := site.Manifest
		go func() {
			// A build and a first pull take minutes: it outlives the request.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			set := func(f func(j *containJob)) {
				s.contains.mu.Lock()
				f(job)
				s.contains.mu.Unlock()
			}
			fail := func(tried string, err error) {
				msg := err.Error()
				if o.Chat == nil || body.Provider == "" {
					set(func(j *containJob) { j.Step, j.Error, j.Done = "failed", msg, true })
					s.emitContain("containerize.failed", domain, "error", msg, nil)
					return
				}
				set(func(j *containJob) { j.Step, j.Error = "asking", msg })
				name, cerr := o.Chat.Create(domain, chat.CreateOptions{Origin: chat.OriginPanel})
				if cerr == nil {
					_, cerr = o.Chat.SendWith(name, containBrief(dir, m, tried, msg), chat.SendOptions{Provider: body.Provider, Model: body.Model, Queue: true})
					if errors.Is(cerr, chat.ErrQueued) {
						cerr = nil
					}
				}
				if cerr != nil {
					msg += "; and the agent could not be asked: " + cerr.Error()
					set(func(j *containJob) { j.Step, j.Error, j.Done = "failed", msg, true })
					s.emitContain("containerize.failed", domain, "error", msg, nil)
					return
				}
				_, id, _ := strings.Cut(name, "#")
				set(func(j *containJob) { j.Chat, j.Done = id, true })
				s.emitContain("containerize.asked", domain, "warn", msg, map[string]any{"provider": body.Provider, "chat": id})
			}
			plan, err := containerize.For(ctx, dir, m, probe)
			if err != nil {
				fail("", err)
				return
			}
			set(func(j *containJob) { j.Step, j.How = "trying", plan.How })
			s.emitContain("containerize.trying", domain, "", plan.How, nil)
			run := *m.Run
			run.Runtime, run.Docker = "docker", plan.Docker
			try := *m
			try.Run = &run
			if err := s.opts.Engine.Trial(ctx, domain, dir, &try); err != nil {
				fail(plan.How, err)
				return
			}
			// Written over the manifest as it is now, not as it was when the
			// button was pressed: minutes went by, and an edit meanwhile
			// must not be undone by this one.
			now, err := mgr.Get(domain)
			if err == nil && (now.Manifest == nil || now.Manifest.Run == nil || strings.Join(now.Manifest.Run.Command, "\x00") != strings.Join(m.Run.Command, "\x00")) {
				err = errors.New("its command changed while it was tried")
			}
			if err == nil {
				cur := *now.Manifest.Run
				cur.Runtime, cur.Docker = "docker", plan.Docker
				var raw []byte
				if raw, err = json.Marshal(cur); err == nil {
					_, err = mgr.Patch(domain, map[string]json.RawMessage{"run": raw})
				}
			}
			if err != nil {
				msg := "it worked, but the manifest could not be written: " + err.Error()
				set(func(j *containJob) { j.Step, j.Error, j.Done = "failed", msg, true })
				s.emitContain("containerize.failed", domain, "error", msg, nil)
				return
			}
			set(func(j *containJob) { j.Step, j.Done = "done", true })
			s.emitContain("containerize.done", domain, "", plan.How, nil)
		}()
		s.contains.mu.Lock()
		c := *job
		s.contains.mu.Unlock()
		return c, nil
	})
	s.Handle("GET", Prefix+"/sites/{domain}/containerize", "The site's containerization: its step, or how it ended", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		s.contains.mu.Lock()
		defer s.contains.mu.Unlock()
		if j := s.contains.jobs[domain]; j != nil {
			c := *j
			return c, nil
		}
		return nil, Errorf(http.StatusNotFound, "%s has not been put in a container since this node started", domain)
	})
}

func (s *Server) emitContain(typ, domain, level, msg string, data map[string]any) {
	if s.opts.Bus == nil {
		return
	}
	s.opts.Bus.Emit(events.Event{Type: typ, Subject: domain, Actor: "engine", Level: level, Message: msg, Data: data})
}

// containBrief is what the agent is asked: what the container is in this
// house, what was tried and how it failed, and what it may and may not do.
func containBrief(dir string, m *manifest.Manifest, tried, failure string) string {
	run, _ := json.MarshalIndent(m.Run, "", "  ")
	var b strings.Builder
	b.WriteString("Put this site in a container. The house could not do it on its own.\n\n")
	if tried != "" {
		fmt.Fprintf(&b, "What it tried: %s.\nHow it failed: %s\n\n", tried, failure)
	} else {
		fmt.Fprintf(&b, "Why it could not work it out: %s\n\n", failure)
	}
	fmt.Fprintf(&b, "The site's run section today:\n```json\n%s\n```\n\n", run)
	fmt.Fprintf(&b, `How a container runs in this house, which you cannot change:
- The folder is mounted at the same absolute path (%s), the working directory is the same, and the manifest's own run.command is what runs: the image only has to provide what that command needs.
- It runs as the folder owner's uid:gid (no root), with no capabilities and no new privileges, its HOME mounted, $PORT set; it must listen on $PORT.
- Only the site's folder and HOME are seen inside; named or relative volumes go in run.docker.volumes.

What to do:
1. Write a Dockerfile in the site folder that gives the command what it needs (runtime, system libraries, certificates). Pin the base image to a current, supported line. Do not COPY the app in: it is mounted.
2. Set run.docker in site.json to {"build": "Dockerfile"} (keep anything else in run as it is). Leave run.runtime alone.
3. You cannot run docker from here: do not try. Say what you wrote, and tell the operator to press "Put in a container" again: the house builds it, tries it beside the running site, and switches only if it answers.
`, dir)
	return b.String()
}

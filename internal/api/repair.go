package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/agent"
	"github.com/dcimorra/the-middleware/internal/chat"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// A site that arrived here from a move and does not start is handed to
// the house's AI for automatic work (settings.Automation) before the move
// is undone: one turn in the site's own conversation, with why and the
// last lines of its log, then a restart. The moving node asks how it goes,
// as it does for the install; the turn has its own time, bounded.

// repairTime bounds the AI's turn: the site is down at the source
// meanwhile.
const repairTime = 10 * time.Minute

type repairJob struct {
	Done   bool   `json:"done"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

type repairJobs struct {
	mu   sync.Mutex
	jobs map[string]*repairJob
}

// RepairOptions wire the repair of arrived sites.
type RepairOptions struct {
	Chat *chat.Manager
}

// RegisterRepair mounts POST and GET /relocate/{domain}/repair.
func (s *Server) RegisterRepair(o RepairOptions) {
	s.Handle("POST", Prefix+"/relocate/{domain}/repair", "The house's AI for automatic work looks at a site that arrived and does not start (body: why), then it is restarted; GET says how it goes. 409 when no AI for automatic work is configured", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		auto := s.opts.Settings().Automation
		if !auto.On() || o.Chat == nil {
			return nil, Errorf(http.StatusConflict, "no AI for automatic work is configured on this node (Settings → Automatic repairs)")
		}
		var body struct {
			Why string `json:"why"`
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
		s.repairs.mu.Lock()
		if s.repairs.jobs == nil {
			s.repairs.jobs = map[string]*repairJob{}
		}
		if j := s.repairs.jobs[domain]; j != nil && !j.Done {
			s.repairs.mu.Unlock()
			return map[string]any{"started": false, "running": true}, nil
		}
		job := &repairJob{}
		s.repairs.jobs[domain] = job
		s.repairs.mu.Unlock()
		command := ""
		if site.Manifest != nil && site.Manifest.Run != nil {
			command = strings.Join(site.Manifest.Run.Command, " ")
		}
		tail, _ := mgr.Logs(domain, "", 60)
		go s.repairArrived(o.Chat, auto, domain, command, body.Why, tail, job)
		return map[string]any{"started": true}, nil
	})
	s.Handle("GET", Prefix+"/relocate/{domain}/repair", "How the house's AI repair of an arrived site goes: done, its closing line, or why it failed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		s.repairs.mu.Lock()
		defer s.repairs.mu.Unlock()
		j := s.repairs.jobs[r.PathValue("domain")]
		if j == nil {
			return nil, Errorf(http.StatusNotFound, "no repair of %s here since this node started", r.PathValue("domain"))
		}
		return *j, nil
	})
}

func (s *Server) repairArrived(m *chat.Manager, auto settings.Automation, domain, command, why, tail string, job *repairJob) {
	finish := func(out string, err error) {
		s.repairs.mu.Lock()
		job.Done, job.Output = true, out
		if err != nil {
			job.Error = err.Error()
		}
		s.repairs.mu.Unlock()
	}
	// The why comes from the moving node and the log from the site and its
	// visitors: both go in as untrusted data, between a marker no line of
	// them can close (agent.Untrusted), and the turn runs in the site's
	// repair conversation, which has no push credential.
	prompt := fmt.Sprintf(`This site has just been moved to this machine from another one, where it ran, and here it does not start. The house is asking you, as its AI for automatic work, to make it run here (the move waits for you, with the site stopped on the other machine, so be quick and precise).

How it starts: %s

What usually breaks in a move is what did not travel: dependencies installed for the other machine, links that pointed out of the folder, paths or versions of the other machine, files ignored by the copy. Do not restart, stop or deploy anything: the house restarts the site when you finish. The smallest change that works. End your answer with one line that starts with "Done:" saying what you changed, or with "Touched nothing:" saying why.

%s`, strings.Join(strings.Fields(command), " "), agent.Untrusted("Why it does not start, as the move reported it, and the last lines of its log", "Why: "+why+"\n\n"+tail))
	ch, unsub := s.opts.Bus.Subscribe()
	defer unsub()
	name, err := m.RepairConversation(domain)
	if err != nil {
		finish("", fmt.Errorf("the AI was not reached: %w", err))
		return
	}
	id, err := m.SendWith(name, prompt, chat.SendOptions{Provider: auto.Provider, Model: auto.Model})
	if err != nil {
		finish("", fmt.Errorf("the AI was not reached: %w", err))
		return
	}
	s.opts.Bus.Emit(events.Event{Type: "repair.started", Subject: domain, Actor: "engine", Level: "warn", Message: "arrived and does not start: handed to the house's AI (" + auto.Provider + " " + auto.Model + ")", Data: map[string]any{"turn_id": id}})
	timeout := time.NewTimer(repairTime)
	defer timeout.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				finish("", fmt.Errorf("the event stream closed"))
				return
			}
			if (e.Type != "agent.done" && e.Type != "agent.error") || e.Data["turn_id"] != id {
				continue
			}
			if e.Type == "agent.error" {
				finish("", fmt.Errorf("the AI's turn failed: %s", e.Message))
				return
			}
			said := closingLine(fmt.Sprint(e.Data["text"]))
			if s.opts.Engine != nil {
				if err := s.opts.Engine.Restart(context.Background(), domain); err != nil {
					said += " (restart: " + err.Error() + ")"
				}
			}
			s.opts.Bus.Emit(events.Event{Type: "repair.done", Subject: domain, Actor: "engine", Message: said})
			finish(said, nil)
			return
		case <-timeout.C:
			_ = m.Stop(domain)
			finish("", fmt.Errorf("the AI did not finish in %s", repairTime))
			return
		}
	}
}

// closingLine is the "Done:" or "Touched nothing:" line the AI was asked
// to end with, or its last line.
func closingLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "Done:") || strings.HasPrefix(l, "Touched nothing:") {
			return l
		}
	}
	if len(lines) > 0 {
		l := strings.TrimSpace(lines[len(lines)-1])
		if r := []rune(l); len(r) > 300 {
			l = string(r[:300]) + "…"
		}
		return l
	}
	return ""
}

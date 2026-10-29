package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/inbound"
)

// A migration moves many domains from one door to another, one at a time,
// in the background: each can take the CA's time for its certificate, and
// thirty of them in one request held the panel's tab for minutes with
// nothing to show and no way to stop it. It goes on with the tab closed,
// says where it is, and stops between two domains when asked.

// Migration is the state of the last one.
type Migration struct {
	ID        string         `json:"id"`
	To        inbound.Door   `json:"to"`
	Domains   []string       `json:"domains"`
	Done      int            `json:"done"`
	Current   string         `json:"current,omitempty"`
	Lines     []inbound.Line `json:"lines"`
	Started   time.Time      `json:"started"`
	Finished  time.Time      `json:"finished,omitzero"`
	Running   bool           `json:"running"`
	Cancelled bool           `json:"cancelled,omitempty"`
	Failed    int            `json:"failed"`
}

type migrator struct {
	mu     sync.Mutex
	cur    *Migration
	cancel bool
	move   func(ctx context.Context, domain string, to inbound.Door) []inbound.Line
	bus    *events.Bus
}

func (m *migrator) state() Migration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return Migration{Lines: []inbound.Line{}, Domains: []string{}}
	}
	c := *m.cur
	c.Lines = append([]inbound.Line(nil), m.cur.Lines...)
	return c
}

func (m *migrator) start(domains []string, to inbound.Door) (Migration, error) {
	m.mu.Lock()
	if m.cur != nil && m.cur.Running {
		m.mu.Unlock()
		return Migration{}, Errorf(http.StatusConflict, "a migration is running: %d of %d done", m.cur.Done, len(m.cur.Domains))
	}
	m.cur = &Migration{ID: time.Now().UTC().Format("20060102T150405.000"), To: to, Domains: domains, Lines: []inbound.Line{}, Started: time.Now().UTC(), Running: true}
	m.cancel = false
	mig := m.cur
	m.mu.Unlock()
	m.emit("inbound.migration.started", "", "", map[string]any{"total": len(domains), "to": to})
	go func() {
		for _, d := range domains {
			m.mu.Lock()
			if m.cancel {
				mig.Cancelled = true
				m.mu.Unlock()
				break
			}
			mig.Current = d
			m.mu.Unlock()
			// Each domain gets the CA's time, not the client's.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			lines := m.move(ctx, d, to)
			cancel()
			failed := false
			for _, l := range lines {
				if l.Status == "fail" {
					failed = true
				}
			}
			m.mu.Lock()
			mig.Lines = append(mig.Lines, lines...)
			mig.Done++
			if failed {
				mig.Failed++
			}
			done, total := mig.Done, len(mig.Domains)
			m.mu.Unlock()
			m.emit("inbound.migration.progress", d, "", map[string]any{"done": done, "total": total, "failed": failed})
		}
		m.mu.Lock()
		mig.Running, mig.Current, mig.Finished = false, "", time.Now().UTC()
		done, total, cancelled, failed := mig.Done, len(mig.Domains), mig.Cancelled, mig.Failed
		m.mu.Unlock()
		level := ""
		if failed > 0 {
			level = "warn"
		}
		m.emit("inbound.migration.finished", "", level, map[string]any{"done": done, "total": total, "cancelled": cancelled, "failed": failed})
	}()
	return m.state(), nil
}

func (m *migrator) emit(typ, subject, level string, data map[string]any) {
	if m.bus != nil {
		m.bus.Emit(events.Event{Type: typ, Subject: subject, Actor: "operator", Level: level, Data: data})
	}
}

// registerMigration mounts it; move puts one domain on a door.
func (s *Server) registerMigration(move func(ctx context.Context, domain string, to inbound.Door) []inbound.Line) {
	m := &migrator{move: move, bus: s.opts.Bus}
	p := Prefix + "/inbound/migration"
	s.Handle("POST", p, "Move many domains to a door in the background, one at a time (body: domains, to) → the migration; 409 while one runs", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Domains []string `json:"domains"`
			To      string   `json:"to"`
		}
		if err := ReadJSON(r, &b); err != nil {
			return nil, err
		}
		door, err := inbound.ParsePublicDoor(b.To)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		if len(b.Domains) == 0 {
			return nil, Errorf(http.StatusBadRequest, "domains: none given")
		}
		mig, err := m.start(b.Domains, door)
		if err != nil {
			return nil, err
		}
		WriteJSON(w, http.StatusAccepted, mig)
		return Handled, nil
	})
	s.Handle("GET", p, "The running migration, or the last one: where it is and every line so far", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return m.state(), nil
	})
	s.Handle("POST", p+"/cancel", "Stop the running migration after the domain it is on", func(w http.ResponseWriter, r *http.Request) (any, error) {
		m.mu.Lock()
		running := m.cur != nil && m.cur.Running
		m.cancel = running
		m.mu.Unlock()
		if !running {
			return nil, Errorf(http.StatusConflict, "no migration is running")
		}
		return OK, nil
	})
}

package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
)

func (s *Server) registerEvents() {
	s.Handle("GET", Prefix+"/events", "Past events; ?since=RFC3339&type=prefix&subject=domain&limit=200", s.eventsList)
	s.Handle("GET", Prefix+"/events/stream", "Server-sent events, one JSON event per message; ?since catches up first", s.eventsStream)
}

type eventFilter struct {
	since   time.Time
	typ     string
	subject string
	limit   int
}

func parseFilter(r *http.Request) (eventFilter, error) {
	q := r.URL.Query()
	f := eventFilter{typ: q.Get("type"), subject: q.Get("subject"), limit: 200}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			if t, err = time.Parse(time.RFC3339, v); err != nil {
				return f, Errorf(http.StatusBadRequest, "since: want RFC3339")
			}
		}
		f.since = t
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 10000 {
			return f, Errorf(http.StatusBadRequest, "limit: want 1..10000")
		}
		f.limit = n
	}
	return f, nil
}

func (f eventFilter) match(e events.Event) bool {
	if !f.since.IsZero() && !e.Time.After(f.since) {
		return false
	}
	if f.typ != "" && !strings.HasPrefix(e.Type, f.typ) {
		return false
	}
	if f.subject != "" && e.Subject != f.subject {
		return false
	}
	return true
}

// eventsList answers from the journal on disk when there is one (it
// holds days, not the last five hundred) and from the bus's memory
// otherwise, which is what a test or a journal-less installation has.
func (s *Server) eventsList(w http.ResponseWriter, r *http.Request) (any, error) {
	f, err := parseFilter(r)
	if err != nil {
		return nil, err
	}
	all := s.journalEvents(f)
	if all == nil {
		all = s.opts.Bus.Recent()
	}
	out := make([]events.Event, 0, len(all))
	for _, e := range all {
		if f.match(e) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if len(out) > f.limit {
		out = out[len(out)-f.limit:]
	}
	return out, nil
}

// journalEvents reads data/events/<day>.jsonl for the days the filter
// covers. nil means there is no journal; a torn last line is dropped,
// as the bus documents.
func (s *Server) journalEvents(f eventFilter) []events.Event {
	if s.opts.Root == "" {
		return nil
	}
	dir := filepath.Join(s.opts.Root, "data", "events")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".jsonl") {
			names = append(names, ent.Name())
		}
	}
	sort.Strings(names)
	// Without a since, the last two days are plenty for a list capped at
	// ten thousand; with one, start at its day.
	if f.since.IsZero() && len(names) > 2 {
		names = names[len(names)-2:]
	}
	sinceDay := f.since.UTC().Format("2006-01-02")
	var out []events.Event
	for _, name := range names {
		if !f.since.IsZero() && strings.TrimSuffix(name, ".jsonl") < sinceDay {
			continue
		}
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(file)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var e events.Event
			if err := json.Unmarshal(sc.Bytes(), &e); err == nil {
				out = append(out, e)
			}
		}
		file.Close()
	}
	if out == nil {
		out = []events.Event{}
	}
	return out
}

// eventsStream is the live feed. The subscription is taken before the
// catch-up is read so nothing emitted in between is lost; what the
// catch-up already sent is skipped by time. A comment line every
// Heartbeat keeps proxies and the browser from closing an idle stream.
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) (any, error) {
	f, err := parseFilter(r)
	if err != nil {
		return nil, err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, Errorf(http.StatusInternalServerError, "streaming unsupported")
	}
	ch, unsub := s.opts.Bus.Subscribe()
	defer unsub()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	var last time.Time
	send := func(e events.Event) bool {
		if !f.match(e) {
			return true
		}
		if !last.IsZero() && !e.Time.After(last) {
			return true // already sent by the catch-up
		}
		data, err := json.Marshal(e)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.Time.Format(time.RFC3339Nano), e.Type, data); err != nil {
			return false
		}
		last = e.Time
		flusher.Flush()
		return true
	}
	if !f.since.IsZero() {
		for _, e := range s.opts.Bus.Recent() {
			if !send(e) {
				return Handled, nil
			}
		}
	}
	hb := s.Heartbeat
	if hb <= 0 {
		hb = 15 * time.Second
	}
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return Handled, nil
		case e := <-ch:
			if !send(e) {
				return Handled, nil
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return Handled, nil
			}
			flusher.Flush()
		}
	}
}

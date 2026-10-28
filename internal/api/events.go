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

// maxCatchUp bounds what a stream sends before it goes live when it
// resumes from the journal: a tab that slept for a week needs to know
// the house moved on, not to replay every tick of it.
const maxCatchUp = 10000

// eventsStream is the live feed. The subscription and the copy of the
// bus's memory are taken under one lock, so an event is either in the
// catch-up or on the channel and nothing is skipped by comparing. It
// was skipped by time once, for the stream's whole life, and an event
// whose emitter read the clock first but took the lock second was lost
// for every tab. A comment line every Heartbeat keeps proxies and the
// browser from closing an idle stream.
//
// The catch-up is what followed Last-Event-ID (the header a browser's
// own reconnect sends, or ?last_event_id= from the panel, whose fresh
// EventSource cannot set headers), or else what followed ?since=.
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) (any, error) {
	f, err := parseFilter(r)
	if err != nil {
		return nil, err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, Errorf(http.StatusInternalServerError, "streaming unsupported")
	}
	lastID := r.Header.Get("Last-Event-ID")
	if lastID == "" {
		lastID = r.URL.Query().Get("last_event_id")
	}
	ch, recent, unsub := s.opts.Bus.SubscribeRecent()
	defer unsub()
	backlog := s.catchUp(f.since, lastID, recent)
	// The catch-up has been chosen; whatever comes live is newer than it,
	// and a since left in the filter would only hide events from a clock
	// that stepped back.
	f.since = time.Time{}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	send := func(e events.Event) bool {
		if !f.match(e) {
			return true
		}
		data, err := json.Marshal(e)
		if err != nil {
			return true
		}
		// No "event:" line: a browser's EventSource hands named events
		// only to a listener for that very name, never to onmessage, and
		// the panel listens to every type. With the type as the event's
		// name the panel received nothing live, ever; the type is in the
		// JSON.
		if _, err := fmt.Fprintf(w, "id: %s\ndata: %s\n\n", s.opts.Bus.ID(e), data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, e := range backlog {
		if !send(e) {
			return Handled, nil
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

// catchUp is what a stream sends before it goes live. An id from this
// process whose successor memory still holds resumes exactly, by
// sequence. Any other id (a process that is gone, a tab away for longer
// than memory lasts, the RFC 3339 ids of older versions) resumes by its
// time: the journal for what memory no longer has, memory for the rest.
// An id that does not parse is ignored rather than refused, since a
// browser sends back whatever it was given, and since decides instead.
func (s *Server) catchUp(since time.Time, lastID string, recent []events.Event) []events.Event {
	bus := s.opts.Bus
	if lastID != "" {
		if boot, seq, t, ok := events.ParseID(lastID); ok {
			if boot == bus.Boot() && seq > 0 && (len(recent) == 0 || seq+1 >= recent[0].Seq) {
				i := sort.Search(len(recent), func(i int) bool { return recent[i].Seq > seq })
				return recent[i:]
			}
			return s.journalCatchUp(t, recent)
		}
	}
	if since.IsZero() {
		return nil
	}
	out := make([]events.Event, 0, len(recent))
	for _, e := range recent {
		if e.Time.After(since) {
			out = append(out, e)
		}
	}
	return out
}

// journalCatchUp joins the journal to memory at memory's first event:
// times inside one process only grow (the bus reads the clock under its
// lock), so what the journal holds from before that instant is exactly
// what memory has let go of. With memory empty this process has emitted
// nothing yet and the cut is its start; anything after that comes on the
// stream's channel instead.
func (s *Server) journalCatchUp(t time.Time, recent []events.Event) []events.Event {
	cut := time.Unix(0, s.opts.Bus.Boot())
	if len(recent) > 0 {
		cut = recent[0].Time
	}
	var out []events.Event
	if journal := s.journalEvents(eventFilter{since: t}); journal != nil {
		for _, e := range journal {
			if e.Time.After(t) && e.Time.Before(cut) {
				out = append(out, e)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	}
	for _, e := range recent {
		if e.Time.After(t) {
			out = append(out, e)
		}
	}
	if len(out) > maxCatchUp {
		out = out[len(out)-maxCatchUp:]
	}
	return out
}

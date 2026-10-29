package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/auth"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/sites"
)

// frame is one SSE message as a browser assembles it.
type frame struct {
	id string
	ev events.Event
}

// openStream connects to the event stream and returns its frames. The
// handler subscribes before it answers, so once this returns anything
// emitted reaches the stream.
func (f *fixture) openStream(query string, headers ...string) (<-chan frame, func()) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", f.http.URL+"/api/v1/events/stream"+query, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	for _, h := range headers {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		cancel()
		f.t.Fatalf("stream: %d", resp.StatusCode)
	}
	out := make(chan frame, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var cur frame
		var data string
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "id: "):
				cur.id = strings.TrimPrefix(l, "id: ")
			case strings.HasPrefix(l, "data: "):
				data = strings.TrimPrefix(l, "data: ")
			case l == "" && data != "":
				_ = json.Unmarshal([]byte(data), &cur.ev)
				out <- cur
				cur, data = frame{}, ""
			}
		}
	}()
	return out, cancel
}

// next returns the next frame whose type starts with prefix: the auth
// events a fixture's login emits are skipped that way.
func next(t *testing.T, frames <-chan frame, prefix string) frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case fr, ok := <-frames:
			if !ok {
				t.Fatalf("stream closed waiting for %q", prefix)
			}
			if strings.HasPrefix(fr.ev.Type, prefix) {
				return fr
			}
		case <-deadline:
			t.Fatalf("no %q event", prefix)
		}
	}
}

// TestTheStreamKeepsAnEventWhoseClockWasReadFirst is the emitter that
// read the clock, lost the processor and took the bus's lock second: its
// event carries the earlier time but reaches the stream later. The
// stream skipped it, for every tab, because it was not after the last
// time sent.
func TestTheStreamKeepsAnEventWhoseClockWasReadFirst(t *testing.T) {
	f := newFixture(t).withToken()
	frames, stop := f.openStream("")
	defer stop()
	now := time.Now()
	f.bus.Emit(events.Event{Type: "t.second", Time: now})
	f.bus.Emit(events.Event{Type: "t.first", Time: now.Add(-time.Millisecond)})
	if fr := next(t, frames, "t."); fr.ev.Type != "t.second" {
		t.Fatalf("first frame %+v", fr.ev)
	}
	if fr := next(t, frames, "t."); fr.ev.Type != "t.first" {
		t.Fatalf("second frame %+v", fr.ev)
	}
}

// TestTheStreamCatchesUpWithSinceWithoutRepeating is the CLI's
// `events -f --since`: what memory holds after since, then live, each
// event once.
func TestTheStreamCatchesUpWithSinceWithoutRepeating(t *testing.T) {
	f := newFixture(t).withToken()
	f.bus.Emit(events.Event{Type: "t.old"})
	since := url.QueryEscape(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	frames, stop := f.openStream("?type=t.&since=" + since)
	defer stop()
	f.bus.Emit(events.Event{Type: "t.live"})
	f.bus.Emit(events.Event{Type: "t.end"})
	var got []string
	for len(got) < 3 {
		got = append(got, next(t, frames, "t.").ev.Type)
	}
	if strings.Join(got, ",") != "t.old,t.live,t.end" {
		t.Fatalf("got %v", got)
	}
}

// TestTheStreamResumesFromLastEventID is a tab that lost its connection:
// what was emitted while it was away arrives when it comes back, and
// nothing it had already seen arrives twice. The browser's own reconnect
// sends the header; the panel's fresh EventSource sends the query.
func TestTheStreamResumesFromLastEventID(t *testing.T) {
	for _, how := range []string{"header", "query"} {
		t.Run(how, func(t *testing.T) {
			f := newFixture(t).withToken()
			frames, stop := f.openStream("?type=r.")
			f.bus.Emit(events.Event{Type: "r.one"})
			f.bus.Emit(events.Event{Type: "r.two"})
			first := next(t, frames, "r.")
			next(t, frames, "r.")
			stop()
			f.bus.Emit(events.Event{Type: "r.three"})

			var again <-chan frame
			if how == "header" {
				again, stop = f.openStream("?type=r.", "Last-Event-ID: "+first.id)
			} else {
				again, stop = f.openStream("?type=r.&last_event_id=" + url.QueryEscape(first.id))
			}
			defer stop()
			f.bus.Emit(events.Event{Type: "r.four"})
			var got []string
			for len(got) < 3 {
				got = append(got, next(t, again, "r.").ev.Type)
			}
			if strings.Join(got, ",") != "r.two,r.three,r.four" {
				t.Fatalf("got %v", got)
			}
		})
	}
}

// TestTheStreamResumesAcrossARestartFromTheJournal is an id handed out
// by the previous process: its sequence means nothing to this bus, so
// the stream resumes by the id's time, from the journal for what came
// before this process and from memory after, each event once.
func TestTheStreamResumesAcrossARestartFromTheJournal(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sites"), 0o755)
	journal := filepath.Join(root, "data", "events")
	old := events.New(journal)
	old.Emit(events.Event{Type: "j.one"})
	time.Sleep(2 * time.Millisecond)
	old.Emit(events.Event{Type: "j.two"})
	seen := old.ID(old.Recent()[0])

	time.Sleep(2 * time.Millisecond)
	bus := events.New(journal)
	if bus.Boot() == old.Boot() {
		t.Fatal("two buses with one boot")
	}
	bus.Emit(events.Event{Type: "j.three"})
	store := auth.New(root, bus)
	srv := New(Options{Root: root, Version: "test", Auth: store, Sites: sites.New(root, nil, bus), Bus: bus, Restart: func() {}})
	srv.Heartbeat = 100 * time.Millisecond
	ts := httptest.NewServer(srv.Panel())
	t.Cleanup(ts.Close)
	f := (&fixture{t: t, root: root, srv: srv, store: store, bus: bus, http: ts}).withToken()

	frames, stop := f.openStream("?type=j.", "Last-Event-ID: "+seen)
	defer stop()
	bus.Emit(events.Event{Type: "j.four"})
	var got []string
	for len(got) < 3 {
		got = append(got, next(t, frames, "j.").ev.Type)
	}
	if strings.Join(got, ",") != "j.two,j.three,j.four" {
		t.Fatalf("got %v", got)
	}
}

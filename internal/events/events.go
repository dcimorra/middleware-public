// Package events is the single place anything that changes state reports to.
// The panel's live stream, the audit journal on disk and the notifiers all
// hang from here, so a feature that forgets to emit is a feature nobody can
// see failing. The rule is: if an action can fail and look the same as
// success from outside, it emits an event either way.
package events

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one thing that happened.
type Event struct {
	Time time.Time `json:"time"`
	// Seq is the event's place in this process's order, given under the
	// bus's lock. Times cannot be that order: two emitters read the clock
	// before either held the lock, and the stream, which skipped whatever
	// was not after the last time it sent, dropped the slower one's event
	// for every tab. It starts again at one with every process; the bus's
	// Boot is what tells two processes apart.
	Seq     uint64         `json:"seq,omitempty"`
	Type    string         `json:"type"`              // "instance.started", "handover.done", "cert.renewed"…
	Subject string         `json:"subject,omitempty"` // usually a domain
	Actor   string         `json:"actor,omitempty"`   // "operator", "agent", "guard", "schedule", "engine"
	Level   string         `json:"level,omitempty"`   // "", "warn", "error"
	Message string         `json:"message,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// RecentSize is how many events the bus keeps in memory.
const RecentSize = 500

// Bus fans events out to subscribers and to the journal.
type Bus struct {
	mu      sync.RWMutex
	subs    map[int]chan Event
	nextID  int
	journal string // directory; empty disables the journal
	recent  []Event
	seq     uint64
	boot    int64
}

// New returns a bus. journalDir may be empty for tests.
func New(journalDir string) *Bus {
	return &Bus{subs: map[int]chan Event{}, journal: journalDir, boot: time.Now().UnixNano()}
}

// Boot names this bus among every process that ever ran on the root: a
// sequence number means nothing without it, since each process counts
// from one again.
func (b *Bus) Boot() int64 { return b.boot }

// Emit publishes e. Subscribers that cannot keep up lose events rather than
// block the emitter: an engine tick must never wait on a slow browser tab.
// The clock is read under the lock, with the sequence, so that inside one
// process a later event never carries an earlier time either: the journal
// is read by time, and a stream that joins it to the ring cuts by time.
func (b *Bus) Emit(e Event) {
	b.mu.Lock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.seq++
	e.Seq = b.seq
	b.recent = append(b.recent, e)
	if len(b.recent) > RecentSize {
		b.recent = b.recent[len(b.recent)-RecentSize:]
	}
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
	b.mu.Unlock()
	b.write(e)
}

// Subscribe returns a channel of events and a function to stop receiving.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch, _, stop := b.subscribe(false)
	return ch, stop
}

// SubscribeRecent is Subscribe and Recent under one lock: every event is
// either in the slice or comes on the channel, never both and never
// neither. A stream that catches up and then goes live needs exactly that;
// with two calls an event emitted in between was in both, and the
// de-duplication that had to follow is what lost events.
func (b *Bus) SubscribeRecent() (<-chan Event, []Event, func()) {
	return b.subscribe(true)
}

func (b *Bus) subscribe(recent bool) (<-chan Event, []Event, func()) {
	ch := make(chan Event, 256)
	var out []Event
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	if recent {
		out = make([]Event, len(b.recent))
		copy(out, b.recent)
	}
	b.mu.Unlock()
	return ch, out, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

// Recent returns the last events kept in memory, oldest first.
func (b *Bus) Recent() []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Event, len(b.recent))
	copy(out, b.recent)
	return out
}

// ID is how an event is named to a stream's reader, the SSE id a browser
// hands back as Last-Event-ID: the process, the sequence and the time.
// The first two resume exactly from memory; the time is what is left when
// the id comes from a process that is gone, or from so long ago that
// memory no longer holds what followed it.
func (b *Bus) ID(e Event) string {
	return fmt.Sprintf("%d-%d-%d", b.boot, e.Seq, e.Time.UnixNano())
}

// ParseID reads an id made by ID. An id from before sequences existed was
// the event's time in RFC 3339, and it still parses, with boot and seq
// zero: a browser that kept one across the upgrade resumes by time.
func ParseID(id string) (boot int64, seq uint64, t time.Time, ok bool) {
	if parts := strings.Split(id, "-"); len(parts) == 3 {
		bt, err1 := strconv.ParseInt(parts[0], 10, 64)
		sq, err2 := strconv.ParseUint(parts[1], 10, 64)
		ns, err3 := strconv.ParseInt(parts[2], 10, 64)
		if err1 == nil && err2 == nil && err3 == nil {
			return bt, sq, time.Unix(0, ns), true
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, id); err == nil {
		return 0, 0, t, true
	}
	return 0, 0, time.Time{}, false
}

// write appends e as one JSON line to the day's journal file. Appending is
// the one write here that is not atomic, and that is fine: a torn last line
// is dropped by readers, and a journal must never make the emitter fail.
func (b *Bus) write(e Event) {
	if b.journal == "" {
		return
	}
	if err := os.MkdirAll(b.journal, 0o755); err != nil {
		return
	}
	path := filepath.Join(b.journal, e.Time.UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(e)
	_, _ = f.Write(append(line, '\n'))
}

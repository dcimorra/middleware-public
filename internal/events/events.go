// Package events is the single place anything that changes state reports to.
// The panel's live stream, the audit journal on disk and the notifiers all
// hang from here, so a feature that forgets to emit is a feature nobody can
// see failing. The rule is: if an action can fail and look the same as
// success from outside, it emits an event either way.
package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one thing that happened.
type Event struct {
	Time    time.Time      `json:"time"`
	Type    string         `json:"type"`              // "instance.started", "handover.done", "cert.renewed"…
	Subject string         `json:"subject,omitempty"` // usually a domain
	Actor   string         `json:"actor,omitempty"`   // "operator", "agent", "guard", "schedule", "engine"
	Level   string         `json:"level,omitempty"`   // "", "warn", "error"
	Message string         `json:"message,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// Bus fans events out to subscribers and to the journal.
type Bus struct {
	mu      sync.RWMutex
	subs    map[int]chan Event
	nextID  int
	journal string // directory; empty disables the journal
	recent  []Event
}

// New returns a bus. journalDir may be empty for tests.
func New(journalDir string) *Bus {
	return &Bus{subs: map[int]chan Event{}, journal: journalDir}
}

// Emit publishes e. Subscribers that cannot keep up lose events rather than
// block the emitter: an engine tick must never wait on a slow browser tab.
func (b *Bus) Emit(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	b.recent = append(b.recent, e)
	if len(b.recent) > 500 {
		b.recent = b.recent[len(b.recent)-500:]
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
	ch := make(chan Event, 256)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
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

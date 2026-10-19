package events

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestASlowSubscriberDoesNotBlockTheEmitter(t *testing.T) {
	b := New("")
	_, stop := b.Subscribe()
	defer stop()
	for i := 0; i < 1000; i++ {
		b.Emit(Event{Type: "tick"})
	}
}

func TestEventsAreJournaledOneLinePerEvent(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	b.Emit(Event{Type: "a"})
	b.Emit(Event{Type: "b", Subject: "x.test"})
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected one journal file, got %d", len(entries))
	}
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", n, data)
	}
}

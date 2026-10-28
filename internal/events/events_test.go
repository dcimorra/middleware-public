package events

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestASlowSubscriberDoesNotBlockTheEmitter(t *testing.T) {
	b := New("")
	_, stop := b.Subscribe()
	defer stop()
	for i := 0; i < 1000; i++ {
		b.Emit(Event{Type: "tick"})
	}
}

// TestConcurrentEmittersArriveInTheOrderTheyAreNumbered is many emitters
// at once: a subscriber sees sequences that only grow, and times that
// never go back, because both are given under the lock. The time was
// read before it, and an emitter preempted in between delivered an
// earlier time after a later one.
func TestConcurrentEmittersArriveInTheOrderTheyAreNumbered(t *testing.T) {
	b := New("")
	ch, stop := b.Subscribe()
	defer stop()
	const emitters, each = 8, 25
	var wg sync.WaitGroup
	for i := 0; i < emitters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				b.Emit(Event{Type: "tick"})
			}
		}()
	}
	wg.Wait()
	var last Event
	for i := 0; i < emitters*each; i++ {
		e := <-ch
		if e.Seq != last.Seq+1 {
			t.Fatalf("seq %d after %d", e.Seq, last.Seq)
		}
		if e.Time.Before(last.Time) {
			t.Fatalf("event %d at %v, before %v", e.Seq, e.Time, last.Time)
		}
		last = e
	}
}

// TestSubscribeRecentSplitsWithoutOverlap: an event is in the copy of
// memory or on the channel, never both and never neither.
func TestSubscribeRecentSplitsWithoutOverlap(t *testing.T) {
	b := New("")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			b.Emit(Event{Type: "tick"})
		}
	}()
	time.Sleep(time.Millisecond)
	ch, recent, stop := b.SubscribeRecent()
	defer stop()
	<-done
	want := uint64(1)
	if len(recent) > 0 {
		want = recent[0].Seq
	}
	for _, e := range recent {
		if e.Seq != want {
			t.Fatalf("memory: seq %d, want %d", e.Seq, want)
		}
		want++
	}
	for want <= 300 {
		e := <-ch
		if e.Seq != want {
			t.Fatalf("channel: seq %d, want %d", e.Seq, want)
		}
		want++
	}
}

func TestAnIDParsesBackAndAnOldOneByItsTime(t *testing.T) {
	b := New("")
	b.Emit(Event{Type: "a"})
	e := b.Recent()[0]
	boot, seq, at, ok := ParseID(b.ID(e))
	if !ok || boot != b.Boot() || seq != e.Seq || !at.Equal(e.Time) {
		t.Fatalf("parsed %d %d %v %v from %s", boot, seq, at, ok, b.ID(e))
	}
	old := e.Time.UTC().Format(time.RFC3339Nano)
	if boot, seq, at, ok := ParseID(old); !ok || boot != 0 || seq != 0 || !at.Equal(e.Time) {
		t.Fatalf("old id %s: %d %d %v %v", old, boot, seq, at, ok)
	}
	if _, _, _, ok := ParseID("nonsense"); ok {
		t.Fatal("nonsense parsed")
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

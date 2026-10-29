package firewall

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/helper"
)

func countType(bus *events.Bus, typ string) int {
	n := 0
	for _, e := range bus.Recent() {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestWithoutTheHelperEveryCallIsANoOpWithOneWarning(t *testing.T) {
	bus := events.New("")
	f := New(nil, bus)
	for i := 0; i < 5; i++ {
		if err := f.OnBlock("203.0.113.1", time.Minute); !errors.Is(err, ErrNoHelper) {
			t.Fatalf("got %v", err)
		}
	}
	if err := f.Apply(Policy{}); !errors.Is(err, ErrNoHelper) {
		t.Fatalf("got %v", err)
	}
	if n := countType(bus, "firewall.unavailable"); n != 1 {
		t.Fatalf("expected exactly one warning, got %d", n)
	}
	// A client pointing at a socket nobody listens on is the same case.
	g := New(helper.Dial(filepath.Join(t.TempDir(), "none.sock")), bus)
	if g.Available() {
		t.Fatal("available without a socket")
	}
	if err := g.Unblock("203.0.113.1"); !errors.Is(err, ErrNoHelper) {
		t.Fatalf("got %v", err)
	}
}

func TestABlockReachesTheHelperAndEmitsAnEvent(t *testing.T) {
	dir, _ := os.MkdirTemp("", "mwfw")
	t.Cleanup(func() { os.RemoveAll(dir) })
	rec := &helper.Recorder{Outputs: map[string]string{}}
	srv := &helper.Server{
		Socket: filepath.Join(dir, "h.sock"), SocketGroup: -1, AllowUID: -1, AllowGID: -1, Exec: rec,
		Credentials: func(net.Conn) (helper.Peer, error) { return helper.Peer{UID: 0}, nil },
		Log:         log.New(io.Discard, "", 0),
	}
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })

	bus := events.New("")
	f := New(helper.Dial(srv.Socket), bus)
	if !f.Available() {
		t.Fatal("helper should be available")
	}
	if err := f.OnBlock("203.0.113.1", 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	last, _ := rec.Last()
	if last.Line() != "nft add element inet middleware blocked4 { 203.0.113.1 timeout 120s }" {
		t.Fatalf("got %q", last.Line())
	}
	if countType(bus, "firewall.blocked") != 1 || countType(bus, "firewall.unavailable") != 0 {
		t.Fatalf("events: %+v", bus.Recent())
	}
	if err := f.OnBlock("not-an-ip", time.Minute); err == nil || errors.Is(err, ErrNoHelper) {
		t.Fatalf("a validation error is not 'no helper': %v", err)
	}
	if countType(bus, "firewall.block.failed") != 1 {
		t.Fatalf("events: %+v", bus.Recent())
	}
	if err := f.Apply(Policy{AllowPorts: []int{443}}); err != nil {
		t.Fatal(err)
	}
	if last, _ = rec.Last(); last.Line() != "nft -f -" {
		t.Fatalf("got %q", last.Line())
	}
}

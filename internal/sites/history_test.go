package sites

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/ports"
)

func waitHistory(t *testing.T, m *Manager, domain string, n int) []engine.Version {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := m.History(domain)
		if len(v) == n {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("history has %d versions, want %d", len(v), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestEveryWriterIsKeptAndAVersionComesBack writes the manifest the ways
// it is written in real life (by hand, through the manager) and checks
// the engine kept each valid one, skipped the invalid one, and that a
// restore is itself a version.
func TestEveryWriterIsKeptAndAVersionComesBack(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sites", "h.test")
	os.MkdirAll(dir, 0o755)
	e := engine.New(engine.Options{SitesDir: filepath.Join(root, "sites"), DataDir: filepath.Join(root, "data"), Ports: ports.New(41200, 41210), Bus: events.New(""), Tick: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	m := New(root, e, events.New(""))

	first := `{"version":2,"title":"One"}`
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(first), 0o644)
	waitHistory(t, m, "h.test", 1)
	// By hand, invalid: never in force, never kept.
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"titel":"typo"}`), 0o644)
	time.Sleep(400 * time.Millisecond)
	if v, _ := m.History("h.test"); len(v) != 1 {
		t.Fatalf("an invalid manifest was kept: %d versions", len(v))
	}
	// Fixed by hand, then changed through the manager.
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2,"title":"Two"}`), 0o644)
	waitHistory(t, m, "h.test", 2)
	if _, err := m.Patch("h.test", map[string]json.RawMessage{"title": json.RawMessage(`"Three"`)}); err != nil {
		t.Fatal(err)
	}
	v := waitHistory(t, m, "h.test", 3)
	data, err := m.Version("h.test", v[2].ID)
	if err != nil || string(data) != first {
		t.Fatalf("the oldest version is %q, %v", data, err)
	}
	if _, err := m.RestoreVersion("h.test", v[2].ID); err != nil {
		t.Fatal(err)
	}
	waitHistory(t, m, "h.test", 4)
	if now, _ := os.ReadFile(filepath.Join(dir, "site.json")); !strings.Contains(string(now), `"One"`) {
		t.Fatalf("restored to %s", now)
	}
	if _, err := m.Version("h.test", "../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a path as a version id: %v", err)
	}
}

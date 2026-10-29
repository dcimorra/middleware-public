package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
)

// TestSIGUSR1WritesTheDiagnostics sends the real signal to this process.
func TestSIGUSR1WritesTheDiagnostics(t *testing.T) {
	root := t.TempDir()
	a := &App{Root: root, Bus: events.New("")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.watchDiag(ctx)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		files, _ := filepath.Glob(filepath.Join(root, "data", "diag", "*.txt"))
		if len(files) == 1 {
			b, _ := os.ReadFile(files[0])
			if !strings.Contains(string(b), "goroutines") || !strings.Contains(string(b), "TestSIGUSR1WritesTheDiagnostics") {
				t.Fatalf("the report lacks the stacks:\n%.500s", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no report after SIGUSR1")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

package ptyx_test

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/ptyx"
)

// TestAProgramSeesARealTerminal is the whole reason the package exists:
// a child attached here answers "yes" to isatty, which a pipe never
// does, and what it prints comes back through the master.
func TestAProgramSeesARealTerminal(t *testing.T) {
	p, err := ptyx.Open()
	if err != nil {
		t.Skip("no pty on this box:", err)
	}
	defer p.Close()
	if err := p.Resize(24, 80); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `if [ -t 0 ] && [ -t 1 ]; then echo TTY_OK; else echo NO_TTY; fi; stty size`)
	if err := p.Start(cmd); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var out []byte
		for {
			n, err := p.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				break
			}
		}
		done <- string(out)
	}()
	_ = cmd.Wait()
	var out string
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no output read from the terminal")
	}
	if !strings.Contains(out, "TTY_OK") {
		t.Fatalf("the child did not see a terminal: %q", out)
	}
	if !strings.Contains(out, "24 80") {
		t.Fatalf("the child did not see the size set: %q", out)
	}
}

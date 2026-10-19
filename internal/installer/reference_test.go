package installer

import (
	"os"
	"path/filepath"
	"testing"
)

// referenceParams render the copies checked in under systemd/, which exist
// so an operator can read the units before installing anything.
func referenceParams() Params {
	p := DefaultParams()
	p.User = "middleware"
	return p
}

// TestTheCheckedInUnitsMatchWhatInstallRenders keeps systemd/ from drifting
// from the templates: a second copy of a unit ends up saying something
// different. Run with REGEN=1 to rewrite the files.
func TestTheCheckedInUnitsMatchWhatInstallRenders(t *testing.T) {
	files, err := Render(referenceParams())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("..", "..", "systemd")
	for name, content := range files {
		p := filepath.Join(dir, name)
		if os.Getenv("REGEN") == "1" {
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v (REGEN=1 go test ./internal/installer regenerates)", p, err)
		}
		if string(got) != content {
			t.Errorf("%s differs from the template (REGEN=1 go test ./internal/installer regenerates)", p)
		}
	}
}

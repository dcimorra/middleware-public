package relocate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// An export that fails halfway leaves no half dump in the folder: its
// .part stayed, and went with the site on its next move.
func TestAFailedExportLeavesNoPartOfItsDump(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	spec := &manifest.Spec{Domain: "a.test/db/move", SiteDir: dir, Cwd: dir, Env: map[string]string{}, Runtime: "process",
		Command: []string{"sh", "-c", "echo half a dump; exit 2"}}
	err := runCommand(context.Background(), runtime.Docker{}, "", spec, 0, filepath.Join(dir, "move.log"), filepath.Join(dir, "home"), time.Minute, ".move/db.dump")
	if err == nil {
		t.Fatal("it did not fail")
	}
	if _, err := os.Stat(filepath.Join(dir, ".move", "db.dump.part")); err == nil {
		t.Fatal("the half dump is still there")
	}
	ex, _ := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
	if !strings.Contains(string(ex), "/.move/db.dump\n") {
		t.Fatalf("exclude: %q", ex)
	}
	// Asked again, the exclude file does not grow.
	ExcludeFromGit(dir, ".move/db.dump")
	if again, _ := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude")); string(again) != string(ex) {
		t.Fatalf("written twice: %q", again)
	}
}

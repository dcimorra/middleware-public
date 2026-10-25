package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dcimorra/middleware-public/internal/helper"
	"github.com/dcimorra/middleware-public/internal/installer"
)

// The panel follows the run by its log alone: its last line says how it
// ended, a new run never shows the last one's ending, and a log that went
// quiet without one is said as cut.
func TestTheRequirementsRunIsFollowedByItsLog(t *testing.T) {
	f := newFixture(t).withToken()
	var asked []bool
	f.srv.RegisterRequirements(func(_ context.Context, browser bool) error {
		asked = append(asked, browser)
		return nil
	})
	log := helper.InstallLog(f.root)
	os.MkdirAll(filepath.Dir(log), 0o755)
	state := func() map[string]any {
		_, out := f.call("GET", "/api/v1/system/requirements", nil)
		return out
	}
	if st := state(); st["state"] != "none" {
		t.Fatalf("before any run: %v", st)
	}
	os.WriteFile(log, []byte("docker   present\n\n"+installer.PanelFinished+"\n"), 0o644)
	if st := state(); st["state"] != "finished" {
		t.Fatalf("finished: %v", st)
	}
	if resp, out := f.call("POST", "/api/v1/system/requirements", map[string]bool{"browser": true}); resp.StatusCode != 200 || out["state"] != "running" {
		t.Fatalf("start: %d %v", resp.StatusCode, out)
	}
	if len(asked) != 1 || !asked[0] {
		t.Fatalf("the helper was asked %v", asked)
	}
	if st := state(); st["state"] != "none" {
		t.Fatalf("the last run's ending is still shown: %v", st)
	}
	os.WriteFile(log, []byte("installing…\n"), 0o644)
	if st := state(); st["state"] != "running" {
		t.Fatalf("running: %v", st)
	}
	if resp, _ := f.call("POST", "/api/v1/system/requirements", nil); resp.StatusCode != 409 {
		t.Fatalf("a second run while one runs: %d", resp.StatusCode)
	}
	os.WriteFile(log, []byte("x\n"+installer.PanelFailed+"E: unable to locate package\n"), 0o644)
	if st := state(); st["state"] != "failed" || st["error"] != "E: unable to locate package" {
		t.Fatalf("failed: %v", st)
	}
	os.WriteFile(log, []byte("x\n"+installer.PanelRestart+"\n"), 0o644)
	if st := state(); st["state"] != "restart" {
		t.Fatalf("restart: %v", st)
	}
	// The house restarted after the run: done, not "restart" for ever.
	before := time.Now().Add(-time.Hour)
	os.Chtimes(log, before, before)
	if st := state(); st["state"] != "finished" {
		t.Fatalf("after the restart: %v", st)
	}
	os.WriteFile(log, []byte("installing…\n"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(log, old, old)
	if st := state(); st["state"] != "interrupted" {
		t.Fatalf("a quiet log: %v", st)
	}
}

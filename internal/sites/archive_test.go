package sites

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Archiving a site moved to another node packs it with the manifest it
// had, enabled, and that manifest is never where the engine reads: the
// folder leaves sites/ first. Written in place, as it once was, the engine
// started the site again here, database and all, for as long as the pack
// took, and for good when the pack failed.
func TestASiteBeingArchivedIsNotStartedAgainByItsManifest(t *testing.T) {
	f := newFixture(t, true)
	if _, err := f.m.Create("a.test", runManifest(), ""); err != nil {
		t.Fatal(err)
	}
	waitSite(t, f.m, "a.test", func(s Site) bool { return s.Alive })
	original, err := f.m.StopForMove(context.Background(), "a.test")
	if err != nil {
		t.Fatal(err)
	}
	waitSite(t, f.m, "a.test", func(s Site) bool { return !s.Alive })

	var during []string
	beforePack = func(folder string) error {
		if _, err := os.Stat(f.m.Dir("a.test")); err == nil {
			during = append(during, "the folder is still in sites/")
		}
		mf, err := manifest.Load(folder)
		if err != nil || mf.Disabled || mf.Hold != nil {
			during = append(during, "what is packed is not the manifest it had")
		}
		// Five of the engine's ticks: time to start what it can see.
		time.Sleep(time.Second)
		if sn, ok := f.m.lookup("a.test"); ok && sn.Alive {
			during = append(during, "the engine started it again")
		}
		return nil
	}
	t.Cleanup(func() { beforePack = nil })
	dest, err := f.m.ArchiveMoved(context.Background(), "a.test", original)
	if err != nil {
		t.Fatal(err)
	}
	if len(during) > 0 {
		t.Fatal(strings.Join(during, "; "))
	}
	if _, err := os.Stat(dest); err != nil || !strings.HasSuffix(dest, ".tar.gz") {
		t.Fatalf("archive %s: %v", dest, err)
	}
	if left, _ := filepath.Glob(filepath.Join(f.m.ArchiveDir(), "*"+packingSuffix)); len(left) > 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// A pack that fails puts the folder back, stopped as it was: deleting
// with archive=true never deletes and never starts anything.
func TestAPackThatFailsLeavesTheSiteStoppedWhereItWas(t *testing.T) {
	f := newFixture(t, true)
	if _, err := f.m.Create("a.test", runManifest(), ""); err != nil {
		t.Fatal(err)
	}
	waitSite(t, f.m, "a.test", func(s Site) bool { return s.Alive })
	beforePack = func(string) error { return errors.New("no room") }
	t.Cleanup(func() { beforePack = nil })
	if _, err := f.m.Delete(context.Background(), "a.test", true); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("err: %v", err)
	}
	mf, err := manifest.Load(f.m.Dir("a.test"))
	if err != nil || !mf.Disabled {
		t.Fatalf("not back stopped: %+v %v", mf, err)
	}
	time.Sleep(time.Second)
	if sn, ok := f.m.lookup("a.test"); ok && sn.Alive {
		t.Fatal("it started again")
	}

	// And one a restart cut short, its folder already out of sites/, is
	// packed when the move asks again.
	beforePack = nil
	original, _ := os.ReadFile(filepath.Join(f.m.Dir("a.test"), manifest.FileName))
	packing := filepath.Join(f.m.ArchiveDir(), "a.test-20260101T000000Z"+packingSuffix)
	os.MkdirAll(f.m.ArchiveDir(), 0o755)
	if err := os.Rename(f.m.Dir("a.test"), packing); err != nil {
		t.Fatal(err)
	}
	dest, err := f.m.ArchiveMoved(context.Background(), "a.test", original)
	if err != nil || filepath.Base(dest) != "a.test-20260101T000000Z.tar.gz" {
		t.Fatalf("%s: %v", dest, err)
	}
	if _, err := os.Stat(packing); !os.IsNotExist(err) {
		t.Fatal("the folder being packed is still there")
	}
}

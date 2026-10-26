package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

// A site in a container writes its own folder, and the house reads that
// folder's site.json on the machine: if the container could rewrite it, a
// `runtime: "process"` of its own would run outside the walls.
func TestAContainerCannotRewriteItsManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir}
	args, err := siteMounts(spec, filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, manifest.FileName) + ":" + filepath.Join(dir, manifest.FileName) + ":ro"
	i := slices.Index(args, want)
	if i < 1 || args[i-1] != "-v" {
		t.Fatalf("the manifest is not mounted read-only: %v", args)
	}
	if folder := slices.Index(args, real+":"+dir); folder < 0 || folder > i {
		t.Fatalf("the read-only manifest must come after the folder's mount, or the folder covers it: %v", args)
	}
}

func TestALinkedManifestDoesNotGoIntoAContainer(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "site.json")
	if err := os.WriteFile(other, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, manifest.FileName)); err != nil {
		t.Fatal(err)
	}
	spec := &manifest.Spec{Domain: "a.test", SiteDir: dir}
	if args, err := siteMounts(spec, filepath.Join(t.TempDir(), "home")); err == nil {
		t.Fatalf("a linked manifest was mounted: %v", args)
	}
}

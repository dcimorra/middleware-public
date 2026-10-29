package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// fakeDocker is a docker CLI that keeps its volumes and containers as
// files under state: a volume is a file holding its labels as JSON, a
// container one holding its middleware.domain label. Only what Start,
// Adopt and ClaimVolume ask is answered; a create of a name that is there
// succeeds and changes nothing, as docker's does.
func fakeDocker(t *testing.T) (Docker, string) {
	t.Helper()
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "volumes"), 0o755)
	os.MkdirAll(filepath.Join(state, "containers"), 0o755)
	script := `#!/bin/sh
S=` + state + `
last() { for a; do :; done; echo "$a"; }
case "$1 $2" in
"volume inspect")
	n=$(last "$@"); [ -f "$S/volumes/$n" ] || { echo "no such volume" >&2; exit 1; }
	cat "$S/volumes/$n"; exit 0;;
"volume create")
	n=$(last "$@"); site=${4#middleware.site=}
	[ -f "$S/volumes/$n" ] || printf '{"middleware.site":"%s"}' "$site" > "$S/volumes/$n"
	echo "$n"; exit 0;;
esac
case "$1" in
inspect)
	n=$(last "$@"); [ -f "$S/containers/$n" ] || exit 1
	case "$3" in
	*State.Running*) echo "true id-$n $(cat "$S/containers/$n")";;
	*) cat "$S/containers/$n";;
	esac;;
rm) rm -f "$S/containers/$3";;
run)
	shift 3; name=$1; dom=
	while [ $# -gt 0 ]; do case "$1" in --label) case "$2" in middleware.domain=*) dom=${2#middleware.domain=};; esac;; esac; shift; done
	echo "$dom" > "$S/containers/$name"; echo "id-$name";;
info|wait|logs) ;;
esac
`
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Docker{Bin: bin, Data: filepath.Join(t.TempDir(), "data")}, state
}

func dockerSpec(t *testing.T, domain string, volumes ...string) *manifest.Spec {
	dir := t.TempDir()
	return &manifest.Spec{Domain: domain, SiteDir: dir, Cwd: dir, Command: []string{"./server"}, Env: map[string]string{}, Revision: "r1",
		Docker: &manifest.Docker{Image: "app:1", Volumes: volumes}}
}

// my-shop.com and my.shop.com are one docker name. The volume they share
// is made labelled with the first, and the second is refused, not handed
// its database.
func TestTwoSitesWithOneDockerNameDoNotShareAVolume(t *testing.T) {
	d, state := fakeDocker(t)
	ctx := context.Background()
	first := dockerSpec(t, "my-shop.com", "data:/app/data")
	inst, err := d.Start(ctx, first, 41001, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst.Stop(ctx, 0)
	raw, _ := os.ReadFile(filepath.Join(state, "volumes", "mw-my-shop-com-data"))
	var labels map[string]string
	if json.Unmarshal(raw, &labels); labels[SiteLabel] != "my-shop.com" {
		t.Fatalf("the volume was not made with its owner: %s", raw)
	}
	// Again, and through a worker of it: still its own.
	if _, err := d.Start(ctx, dockerSpec(t, "my-shop.com/db", "data:/d"), 41002, "", t.TempDir()); err != nil {
		t.Fatalf("its own worker was refused its volume: %v", err)
	}
	_, err = d.Start(ctx, dockerSpec(t, "my.shop.com", "data:/app/data"), 41003, "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "my-shop.com's") {
		t.Fatalf("the other site got the volume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "containers", ContainerName("my.shop.com", 41003))); err == nil {
		t.Fatal("the refused site's container was started anyway")
	}
}

// A volume made before volumes carried their owner goes to the first site
// that claims it, written down, and only to that one.
func TestAnUnlabelledVolumeIsAdoptedByTheFirstSiteOnly(t *testing.T) {
	d, state := fakeDocker(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(state, "volumes", "mw-my-shop-com-pg"), []byte("null"), 0o644)
	if err := d.ClaimVolume(ctx, "my.shop.com", "mw-my-shop-com-pg"); err != nil {
		t.Fatal(err)
	}
	if err := d.ClaimVolume(ctx, "my.shop.com", "mw-my-shop-com-pg"); err != nil {
		t.Fatalf("its adopter was refused it: %v", err)
	}
	if err := d.ClaimVolume(ctx, "my-shop.com", "mw-my-shop-com-pg"); err == nil {
		t.Fatal("a second site was given an adopted volume")
	}
	raw, err := os.ReadFile(filepath.Join(d.Data, VolumeClaimsFile))
	if err != nil || !strings.Contains(string(raw), `"mw-my-shop-com-pg": "my.shop.com"`) {
		t.Fatalf("the claim was not written down: %s %v", raw, err)
	}
	// The record survives the process: a new Docker value reads it.
	again := Docker{Bin: d.Bin, Data: d.Data}
	if err := again.ClaimVolume(ctx, "my-shop.com", "mw-my-shop-com-pg"); err == nil {
		t.Fatal("after a restart the second site was given it")
	}
}

// A worker's port is in its manifest, so two sites with one docker name
// can ask for one container name: the second must not remove the first's
// running container to make room, nor adopt it as its own.
func TestASiteDoesNotRemoveOrAdoptAnotherSitesContainer(t *testing.T) {
	d, state := fakeDocker(t)
	ctx := context.Background()
	if _, err := d.Start(ctx, dockerSpec(t, "my-shop.com/db"), 5433, "", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	name := ContainerName("my.shop.com/db", 5433)
	if name != ContainerName("my-shop.com/db", 5433) {
		t.Fatalf("the names do not collide (%s), the test proves nothing", name)
	}
	if _, err := d.Start(ctx, dockerSpec(t, "my.shop.com/db"), 5433, "", t.TempDir()); err == nil {
		t.Fatal("the second site's worker started over the first's")
	}
	if raw, _ := os.ReadFile(filepath.Join(state, "containers", name)); strings.TrimSpace(string(raw)) != "my-shop.com/db" {
		t.Fatalf("the first site's container was replaced: %q", raw)
	}
	if _, err := d.Adopt(ctx, State{Domain: "my.shop.com/db", Container: name, Port: 5433}); err != ErrGone {
		t.Fatalf("another site's container was adopted: %v", err)
	}
	if _, err := d.Adopt(ctx, State{Domain: "my-shop.com/db", Container: name, Port: 5433}); err != nil {
		t.Fatalf("its own container was not adopted: %v", err)
	}
}

package relocate

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// fakeDocker is a docker that knows volumes, listed one per line in a
// file, and nothing else.
func fakeDocker(t *testing.T, vols ...string) (runtime.Docker, string) {
	t.Helper()
	dir := t.TempDir()
	list := filepath.Join(dir, "volumes")
	os.WriteFile(list, []byte(strings.Join(vols, "\n")+"\n"), 0o644)
	bin := filepath.Join(dir, "docker")
	os.WriteFile(bin, []byte(`#!/bin/sh
f=`+list+`
case "$1 $2" in
"volume inspect") grep -qx "$3" "$f" ;;
"volume rm") grep -vx "$4" "$f" > "$f.new"; mv "$f.new" "$f" ;;
*) exit 1 ;;
esac
`), 0o755)
	return runtime.Docker{Bin: bin}, list
}

// volumeTarget checks as the API's check does, against its own docker.
type volumeTarget struct{ localTarget }

func (t *volumeTarget) Check(ctx context.Context, d string, need Needs) error {
	if err := t.rc.CheckNeeds(ctx, need); err != nil {
		return err
	}
	return t.localTarget.Check(ctx, d, need)
}

func (t *volumeTarget) Migrate(context.Context, string, []byte, []string) (string, error) {
	return "", nil
}

// volumeSource is a source whose workers keep their data in volumes of
// its own docker; its exports write the dump they are told to.
type volumeSource struct {
	*localSource
	removed     []string
	failArchive bool
}

func (s *volumeSource) Export(_ context.Context, d string, raw []byte, workers []string) (string, error) {
	m, _ := manifest.Parse(raw)
	for _, name := range workers {
		if out := m.Workers[name].Move.Output; out != "" {
			p := filepath.Join(s.root, "sites", d, out)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("dump of "+name), 0o644)
		}
	}
	return "", nil
}

func (s *volumeSource) RemoveVolumes(_ context.Context, vols []string) error {
	s.removed = append(s.removed, vols...)
	return nil
}

func (s *volumeSource) Archive(ctx context.Context, d string, m []byte) (string, error) {
	if s.failArchive {
		return "", errors.New("no room for the archive")
	}
	return s.localSource.Archive(ctx, d, m)
}

const volumeManifest = `{"version":2,"static":{"root":"public"},"workers":{
 "db":{"command":["postgres"],"runtime":"docker","port":%PORT%,
  "docker":{"image":"postgres:16-alpine","volumes":["pg:/var/lib/postgresql/data"]},
  "move":{"export":["pg_dump"],"output":".move/db.dump","import":["pg_restore"]}},
 "cache":{"command":["redis-server"],"runtime":"docker",
  "docker":{"image":"redis:7","volumes":["cache:/data"]},
  "move":{"export":["true"],"import":["true"],"live":true}}}}`

func freePort(t *testing.T) (int, net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l.Addr().(*net.TCPAddr).Port, l
}

func volumeSeed(t *testing.T, root string, port int) {
	seed(t, root)
	raw := strings.ReplaceAll(volumeManifest, "%PORT%", strconv.Itoa(port))
	os.WriteFile(filepath.Join(root, "sites/a.test/site.json"), []byte(raw), 0o644)
}

// Moving a site back to a node it left found the volume its database had
// there at the import, with the site already stopped and its last copy
// sent. The target says it at the check now, and a written port it has
// taken too; nothing is stopped.
func TestATargetWithTheSitesVolumeOrPortRefusesItAtTheCheck(t *testing.T) {
	for _, c := range []struct {
		name, want string
		vols       []string
		takePort   bool
	}{
		{"volume", "volume mw-a-test-pg is already", []string{"mw-a-test-pg"}, false},
		{"port", "is taken on this node", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			port, l := freePort(t)
			if !c.takePort {
				l.Close()
			} else {
				defer l.Close()
			}
			volumeSeed(t, src, port)
			d, _ := fakeDocker(t, c.vols...)
			tg := &volumeTarget{localTarget{rc: Receiver{Root: dst, Docker: &d}, ready: true}}
			s := &volumeSource{localSource: &localSource{root: src}}
			m := &Mover{Root: src, Source: s, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
			var steps []string
			_, err := m.Move(context.Background(), "a.test", tg, func(step, _ string) { steps = append(steps, step) })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err: %v", err)
			}
			if strings.Join(steps, " ") != "check" {
				t.Fatalf("it went as far as %v", steps)
			}
		})
	}
}

// Once a site has moved and its copy here is archived with the dump of
// each database exported with the site stopped, that database's volume
// goes: it holds nothing the target and the archive do not. A live
// worker's volume stays, listed as a leftover, as do all of them when the
// archive could not be written: what is only there is the operator's to
// throw away.
func TestASiteThatMovedTakesItsVolumesOnlyWhenItsArchiveHasThem(t *testing.T) {
	t.Run("archived", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		port, l := freePort(t)
		l.Close()
		volumeSeed(t, src, port)
		d, list := fakeDocker(t, "mw-a-test-cache")
		tg := &volumeTarget{localTarget{rc: Receiver{Root: dst}, ready: true}}
		s := &volumeSource{localSource: &localSource{root: src}}
		m := &Mover{Root: src, Source: s, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
		if _, err := m.Move(context.Background(), "a.test", tg, nil); err != nil {
			t.Fatal(err)
		}
		if strings.Join(s.removed, " ") != "mw-a-test-pg" {
			t.Fatalf("removed %v", s.removed)
		}
		var left []Leftover
		for _, l := range Leftovers(src, time.Now()) {
			if l.Kind == "volume" {
				left = append(left, l)
			}
		}
		if len(left) != 1 || left[0].Path != "docker-volume:mw-a-test-cache" || left[0].Domain != "a.test" {
			t.Fatalf("leftovers: %+v", left)
		}
		cleanDocker = d
		t.Cleanup(func() { cleanDocker = runtime.Docker{} })
		if err := Clean(src, left[0].Path, time.Now()); err != nil {
			t.Fatal(err)
		}
		if raw, _ := os.ReadFile(list); strings.Contains(string(raw), "mw-a-test-cache") {
			t.Fatal("the volume was not removed")
		}
		for _, l := range Leftovers(src, time.Now()) {
			if l.Kind == "volume" {
				t.Fatalf("still listed: %+v", l)
			}
		}
	})
	t.Run("not archived", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		port, l := freePort(t)
		l.Close()
		volumeSeed(t, src, port)
		tg := &volumeTarget{localTarget{rc: Receiver{Root: dst}, ready: true}}
		s := &volumeSource{localSource: &localSource{root: src}, failArchive: true}
		m := &Mover{Root: src, Source: s, Poll: 10 * time.Millisecond, ReadyWait: time.Second}
		if _, err := m.Move(context.Background(), "a.test", tg, nil); err == nil || !strings.Contains(err.Error(), "no room") {
			t.Fatalf("err: %v", err)
		}
		if len(s.removed) > 0 {
			t.Fatalf("removed %v with no archive", s.removed)
		}
		n := 0
		for _, l := range Leftovers(src, time.Now()) {
			if l.Kind == "volume" {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("%d volumes listed", n)
		}
	})
}

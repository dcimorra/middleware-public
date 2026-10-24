// Package relocate moves a site from this node to another of its network,
// and receives one: the site comes back up on the other machine with its
// folder, its HOME, its conversations, its cost and its history, and its
// names point there.
//
// The stop is short and said: a first copy goes while the site serves,
// then the site stops, and a second copy carries only what changed since
// the first (and the list of what was there, so what was deleted here is
// deleted there). On the far side the site is put in place only when the
// last byte arrived; until then it is a staging folder nobody serves, and
// throwing it away is deleting a folder. The order of the last steps is
// what keeps a mistake from losing anything:
//
//   - the source is stopped, never deleted, until the target answers
//     healthy: a target that does not come up is removed there and the
//     source started again here, as it was;
//   - the names move only after: the target publishes them over what
//     pointed here, and only then does this node take its own route down;
//   - the source folder is archived here, not deleted, so going back is a
//     restore.
package relocate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/archive"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Parts are what moves with a site: its folder, and what the house keeps
// of it under data/ by its name. The same list as a rename moves, plus the
// deploy record; logs stay, they are this machine's.
func Parts(root, domain string) []archive.Part {
	data := filepath.Join(root, "data")
	return []archive.Part{
		{Name: "site", Path: filepath.Join(root, "sites", domain)},
		{Name: "data/homes/" + domain, Path: filepath.Join(data, "homes", domain)},
		{Name: "data/chat/" + domain + ".json", Path: filepath.Join(data, "chat", domain+".json")},
		{Name: "data/chat/" + domain, Path: filepath.Join(data, "chat", domain)},
		{Name: "data/agents/cost/" + domain + ".json", Path: filepath.Join(data, "agents", "cost", domain+".json")},
		{Name: "data/history/" + domain, Path: filepath.Join(data, "history", domain)},
		{Name: "data/deploys/" + domain + ".json", Path: filepath.Join(data, "deploys", domain+".json")},
	}
}

// Errors the receiving side answers with.
var (
	ErrBadName    = errors.New("not a site's name")
	ErrExists     = errors.New("a site with that name already exists here")
	ErrNoStaging  = errors.New("nothing was received for that site")
	ErrNotAFolder = errors.New("the site's folder is a link to somewhere else; move what it points at instead")
)

// Receiver is the target's side.
type Receiver struct{ Root string }

func (r Receiver) staging(domain string) string {
	return filepath.Join(r.Root, "data", "relocate", domain)
}

func (r Receiver) check(domain string) error {
	if !manifest.ValidName(domain) {
		return ErrBadName
	}
	if _, err := os.Lstat(filepath.Join(r.Root, "sites", domain)); err == nil {
		return ErrExists
	}
	return nil
}

// Receive takes a whole bundle into the staging folder, replacing what
// an earlier attempt left there.
func (r Receiver) Receive(ctx context.Context, domain string, body io.Reader) (archive.Report, error) {
	if err := r.check(domain); err != nil {
		return archive.Report{}, err
	}
	dir := r.staging(domain)
	if err := os.RemoveAll(dir); err != nil {
		return archive.Report{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return archive.Report{}, err
	}
	return archive.UnpackFrom(ctx, body, dir)
}

// ReceiveDelta lays a partial bundle over the staging folder: what it
// carries replaces what was there, and whatever its index does not name
// is removed.
func (r Receiver) ReceiveDelta(ctx context.Context, domain string, body io.Reader) (archive.Report, error) {
	if err := r.check(domain); err != nil {
		return archive.Report{}, err
	}
	dir := r.staging(domain)
	if !fsx.Exists(dir) {
		return archive.Report{}, ErrNoStaging
	}
	delta := dir + ".delta"
	_ = os.RemoveAll(delta)
	rep, err := archive.UnpackFrom(ctx, body, delta)
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(delta)
	raw, err := os.ReadFile(filepath.Join(delta, archive.IndexName))
	if err != nil {
		return rep, errors.New("the last copy carries no index: it would leave deleted files behind")
	}
	keep := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		// Every folder on the way to a name is kept too: the index lists
		// the parts ("data/homes/x"), not the folders they sit in.
		for line != "" && line != "." && !keep[line] {
			keep[line] = true
			line = filepath.ToSlash(filepath.Dir(line))
		}
	}
	// What arrived replaces what was there, deepest last so a folder
	// exists before what goes in it.
	err = filepath.WalkDir(delta, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(delta, p)
		if rel == "." || rel == archive.IndexName {
			return nil
		}
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			if info, err := os.Lstat(dst); err == nil && !info.IsDir() {
				_ = os.RemoveAll(dst)
			}
			return os.MkdirAll(dst, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.RemoveAll(dst)
		return os.Rename(p, dst)
	})
	if err != nil {
		return rep, err
	}
	// Whatever the index does not name was deleted at the source.
	var gone []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if !keep[filepath.ToSlash(rel)] {
			gone = append(gone, p)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	for _, p := range gone {
		_ = os.RemoveAll(p)
	}
	return rep, nil
}

// Commit puts the received site in place: its data first, its folder
// last, because the engine serves a folder the moment it sees one with a
// manifest. manifestData, when not empty, is the manifest the site had
// before it was stopped for the move.
func (r Receiver) Commit(domain string, manifestData []byte) error {
	if err := r.check(domain); err != nil {
		return err
	}
	dir := r.staging(domain)
	site := filepath.Join(dir, "site")
	if !fsx.Exists(site) {
		return ErrNoStaging
	}
	if len(manifestData) > 0 {
		if _, err := manifest.Parse(manifestData); err != nil {
			return fmt.Errorf("the manifest sent with the move: %w", err)
		}
		if err := fsx.WriteAtomic(filepath.Join(site, manifest.FileName), manifestData, 0o644); err != nil {
			return err
		}
	}
	// The parts under data/, by the same list they were packed with.
	for _, part := range Parts(r.Root, domain) {
		if !strings.HasPrefix(part.Name, "data/") {
			continue
		}
		src := filepath.Join(dir, filepath.FromSlash(part.Name))
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		dst := filepath.Join(r.Root, filepath.FromSlash(part.Name))
		if _, err := os.Lstat(dst); err == nil {
			// Something of an earlier life of this name here: set aside,
			// never mixed with or lost under what arrived.
			_ = os.Rename(dst, dst+".before-"+time.Now().UTC().Format("20060102T150405Z"))
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(r.Root, "sites"), 0o755); err != nil {
		return err
	}
	if err := os.Rename(site, filepath.Join(r.Root, "sites", domain)); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// StagingSite is where a received site's folder waits for its commit.
func (r Receiver) StagingSite(domain string) string {
	return filepath.Join(r.staging(domain), "site")
}

// Discard throws away what was received for domain.
func (r Receiver) Discard(domain string) error {
	if !manifest.ValidName(domain) {
		return ErrBadName
	}
	dir := r.staging(domain)
	_ = os.RemoveAll(dir + ".delta")
	return os.RemoveAll(dir)
}

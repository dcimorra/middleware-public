package sites

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Entry is one item of a folder listing.
type Entry struct {
	Name  string    `json:"name"`
	Dir   bool      `json:"dir"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
}

// MaxFileSize bounds what ReadFile and WriteFile carry: the file endpoint
// is an editor, not a transfer tool, and a 2 GB upload through it would
// be held in memory twice.
const MaxFileSize = 16 << 20

// ErrTooLarge says the file exceeds MaxFileSize.
var ErrTooLarge = fmt.Errorf("file larger than %d bytes", MaxFileSize)

// Resolve maps a path relative to the site folder onto the filesystem and
// proves it stays inside. Symlinks are followed on the deepest existing
// ancestor and the result is compared with the folder's own resolved path,
// so neither "../" nor a link planted inside the folder can reach out. The
// path need not exist yet: WriteFile creates files.
func (m *Manager) Resolve(domain, rel string) (string, error) {
	dir, err := m.managedDirOrFolder(domain)
	if err != nil {
		return "", err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	slashed := strings.ReplaceAll(rel, "\\", "/")
	// Clean would root "../x" to "/x" and quietly serve the wrong file; a
	// caller that says ".." meant to leave and is told so.
	for _, seg := range strings.Split(slashed, "/") {
		if seg == ".." {
			return "", ErrOutside
		}
	}
	clean := path.Clean("/" + slashed)
	if strings.ContainsRune(clean, 0) {
		return "", ErrOutside
	}
	full := filepath.Join(realDir, filepath.FromSlash(clean))
	// Walk up to the first ancestor that exists, resolve that, and append
	// what is left unresolved.
	probe, rest := full, ""
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			resolved := real
			if rest != "" {
				resolved = filepath.Join(real, rest)
			}
			if resolved != realDir && !strings.HasPrefix(resolved, realDir+string(filepath.Separator)) {
				return "", ErrOutside
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", ErrOutside
		}
		rest = filepath.Join(filepath.Base(probe), rest)
		probe = parent
	}
}

// managedDirOrFolder is managedDir for files: an unmanaged folder can be
// browsed, that is how an operator adds the manifest it lacks.
func (m *Manager) managedDirOrFolder(domain string) (string, error) {
	if !manifest.ValidName(domain) {
		return "", ErrNotFound
	}
	dir := m.Dir(domain)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", ErrNotFound
	}
	return dir, nil
}

// Files lists a folder inside the site.
func (m *Manager) Files(domain, rel string) ([]Entry, error) {
	full, err := m.Resolve(domain, rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, ent := range entries {
		e := Entry{Name: ent.Name(), Dir: ent.IsDir()}
		if info, err := ent.Info(); err == nil {
			e.Size, e.Mtime = info.Size(), info.ModTime()
			if ent.Type()&os.ModeSymlink != 0 {
				if st, err := os.Stat(filepath.Join(full, ent.Name())); err == nil {
					e.Dir, e.Size = st.IsDir(), st.Size()
				}
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ReadFile returns a file's content.
func (m *Manager) ReadFile(domain, rel string) ([]byte, error) {
	full, err := m.Resolve(domain, rel)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a folder", rel)
	}
	if st.Size() > MaxFileSize {
		return nil, ErrTooLarge
	}
	return os.ReadFile(full)
}

// WriteFile replaces a file's content atomically, creating parents. The
// manifest gets the same validation as the API's own endpoints: an
// invalid site.json saved through the editor would otherwise sit there
// while the engine keeps the previous one in force, which is a state that
// looks fine from every screen.
func (m *Manager) WriteFile(domain, rel string, data []byte) error {
	if len(data) > MaxFileSize {
		return ErrTooLarge
	}
	full, err := m.Resolve(domain, rel)
	if err != nil {
		return err
	}
	if st, err := os.Stat(full); err == nil && st.IsDir() {
		return fmt.Errorf("%s is a folder", rel)
	}
	if filepath.Base(full) == manifest.FileName && filepath.Dir(full) == mustReal(m.Dir(domain)) {
		if _, err := manifest.Parse(data); err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
	}
	perm := os.FileMode(0o644)
	if st, err := os.Stat(full); err == nil {
		perm = st.Mode().Perm()
	}
	if err := fsx.WriteAtomic(full, data, perm); err != nil {
		return err
	}
	m.emit("file.written", domain, "", rel, map[string]any{"path": rel, "bytes": len(data)})
	return nil
}

func mustReal(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

package chat

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Attachments live in the site's own HOME, under uploads/<conversation>/:
// inside the jail already, so nothing is opened to the agent that was not
// its own. pending/ is what goes with the next message; after the turn the
// files move to sent/<turn>/ and are swept a day later. The folder is lent
// to the turn read-only: an attachment is something to read, not a place
// to write.
//
// That HOME is the site's to write too (a docker site has it mounted, the
// agent's Bash reaches it), so every file operation here goes through an
// os.Root on the HOME and no link in it leads out. They used to be plain
// paths: uploads/main/sent made a link to another site's folder had the
// day's sweep RemoveAll what was there, and a link at pending/<name> had
// an upload write through it.

// MaxUpload is one attachment's size limit.
const MaxUpload = 25 << 20

// ErrBadName says an attachment's name is not a plain file name.
var ErrBadName = errors.New("not a plain file name")

// Upload is one pending attachment.
type Upload struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// uploadsHome is the HOME the conversation's attachments live in, and
// uploadsRel their folder inside it, in the slash form os.Root takes.
func (m *Manager) uploadsHome(name string) string {
	domain, _ := split(name)
	return filepath.Join(m.opts.Root, "data", "homes", domain)
}

func uploadsRel(name string) string {
	_, id := split(name)
	conv := "main"
	if id != "" {
		conv = id
	}
	return path.Join("uploads", conv)
}

func (m *Manager) uploadsDir(name string) string {
	return filepath.Join(m.uploadsHome(name), filepath.FromSlash(uploadsRel(name)))
}

// openHome opens the HOME as a root; create makes it first.
func (m *Manager) openHome(name string, create bool) (*os.Root, error) {
	home := m.uploadsHome(name)
	if create {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, err
		}
	}
	return os.OpenRoot(home)
}

// AddUpload stores r as an attachment of the next message of name.
func (m *Manager) AddUpload(name, file string, r io.Reader) (Upload, error) {
	if _, err := m.workdir(name); err != nil {
		return Upload{}, err
	}
	file = filepath.Base(strings.TrimSpace(file))
	if file == "" || file == "." || file == ".." || strings.HasPrefix(file, ".") {
		return Upload{}, ErrBadName
	}
	root, err := m.openHome(name, true)
	if err != nil {
		return Upload{}, err
	}
	defer root.Close()
	dir := path.Join(uploadsRel(name), "pending")
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return Upload{}, err
	}
	p := path.Join(dir, file)
	// Whatever is there goes first, a link included, and the file is made
	// new: written over, a link would have been written through.
	_ = root.Remove(p)
	f, err := root.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Upload{}, err
	}
	n, err := io.Copy(f, io.LimitReader(r, MaxUpload+1))
	f.Close()
	if err == nil && n > MaxUpload {
		err = fmt.Errorf("an attachment is at most %d MB", MaxUpload>>20)
	}
	if err != nil {
		root.Remove(p)
		return Upload{}, err
	}
	return Upload{Name: file, Bytes: n}, nil
}

// readDir lists dir inside root, links not followed.
func readDir(root *os.Root, dir string) []fs.DirEntry {
	f, err := root.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	ents, _ := f.ReadDir(-1)
	return ents
}

// Uploads lists what goes with the next message.
func (m *Manager) Uploads(name string) []Upload {
	out := []Upload{}
	root, err := m.openHome(name, false)
	if err != nil {
		return out
	}
	defer root.Close()
	for _, e := range readDir(root, path.Join(uploadsRel(name), "pending")) {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			out = append(out, Upload{Name: e.Name(), Bytes: info.Size()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RemoveUpload drops a pending attachment.
func (m *Manager) RemoveUpload(name, file string) error {
	file = filepath.Base(file)
	if file == "." || file == ".." || strings.HasPrefix(file, ".") {
		return ErrBadName
	}
	root, err := m.openHome(name, false)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(path.Join(uploadsRel(name), "pending", file))
}

// removeUploads drops every attachment of name, for Purge.
func (m *Manager) removeUploads(name string) {
	root, err := m.openHome(name, false)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.RemoveAll(uploadsRel(name))
}

// attach adds the pending attachments to a prompt and returns the folder
// to lend.
func (m *Manager) attach(name, prompt string) (string, string) {
	ups := m.Uploads(name)
	if len(ups) == 0 {
		return prompt, ""
	}
	dir := filepath.Join(m.uploadsDir(name), "pending")
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\nAttached files (read them with the Read tool):\n")
	for _, u := range ups {
		b.WriteString("- " + filepath.Join(dir, u.Name) + "\n")
	}
	return b.String(), m.uploadsDir(name)
}

// sent moves what went with a turn to sent/<turn>/ and sweeps what was
// sent more than a day ago.
func (m *Manager) sent(name, turnID string) {
	root, err := m.openHome(name, false)
	if err != nil {
		return
	}
	defer root.Close()
	base := uploadsRel(name)
	pending := path.Join(base, "pending")
	if len(readDir(root, pending)) > 0 {
		if root.MkdirAll(path.Join(base, "sent"), 0o700) == nil {
			_ = root.Rename(pending, path.Join(base, "sent", turnID))
		}
	}
	for _, e := range readDir(root, path.Join(base, "sent")) {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = root.RemoveAll(path.Join(base, "sent", e.Name()))
		}
	}
}

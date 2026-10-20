package chat

import (
	"errors"
	"fmt"
	"io"
	"os"
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

// MaxUpload is one attachment's size limit.
const MaxUpload = 25 << 20

// ErrBadName says an attachment's name is not a plain file name.
var ErrBadName = errors.New("not a plain file name")

// Upload is one pending attachment.
type Upload struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

func (m *Manager) uploadsDir(name string) string {
	domain, id := split(name)
	conv := "main"
	if id != "" {
		conv = id
	}
	return filepath.Join(m.opts.Root, "data", "homes", domain, "uploads", conv)
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
	dir := filepath.Join(m.uploadsDir(name), "pending")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Upload{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return Upload{}, err
	}
	n, err := io.Copy(f, io.LimitReader(r, MaxUpload+1))
	f.Close()
	if err == nil && n > MaxUpload {
		err = fmt.Errorf("an attachment is at most %d MB", MaxUpload>>20)
	}
	if err != nil {
		os.Remove(filepath.Join(dir, file))
		return Upload{}, err
	}
	return Upload{Name: file, Bytes: n}, nil
}

// Uploads lists what goes with the next message.
func (m *Manager) Uploads(name string) []Upload {
	ents, _ := os.ReadDir(filepath.Join(m.uploadsDir(name), "pending"))
	out := []Upload{}
	for _, e := range ents {
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
	return os.Remove(filepath.Join(m.uploadsDir(name), "pending", file))
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
	base := m.uploadsDir(name)
	pending := filepath.Join(base, "pending")
	if ents, _ := os.ReadDir(pending); len(ents) > 0 {
		dest := filepath.Join(base, "sent", turnID)
		if os.MkdirAll(filepath.Dir(dest), 0o700) == nil {
			_ = os.Rename(pending, dest)
		}
	}
	old, _ := os.ReadDir(filepath.Join(base, "sent"))
	for _, e := range old {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(base, "sent", e.Name()))
		}
	}
}

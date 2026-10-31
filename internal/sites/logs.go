package sites

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var workerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// LogPath is where a site's or a worker's output goes, as the engine
// writes it.
func (m *Manager) LogPath(domain, worker string) (string, error) {
	if _, err := m.managedDirOrFolder(domain); err != nil {
		return "", err
	}
	if worker == "" {
		return filepath.Join(m.logsDir(domain), "site.log"), nil
	}
	// A service's output and its image's build are asked as
	// "service:<name>" and "build:<name>": one selector, so every reader
	// of a log (the panel, site logs, a follow) reaches them the same way.
	if name, ok := strings.CutPrefix(worker, "service:"); ok && workerNameRe.MatchString(name) {
		return filepath.Join(m.logsDir(domain), name+".log"), nil
	}
	if name, ok := strings.CutPrefix(worker, "build:"); ok && workerNameRe.MatchString(name) {
		return filepath.Join(m.logsDir(domain), "build-"+name+".log"), nil
	}
	if !workerNameRe.MatchString(worker) {
		return "", fmt.Errorf("worker %q is not a name", worker)
	}
	return filepath.Join(m.logsDir(domain), "worker-"+worker+".log"), nil
}

// Logs returns the last n lines of a log. A missing log is empty, not an
// error: a site that has never started has nothing to say yet.
func (m *Manager) Logs(domain, worker string, n int) (string, error) {
	p, err := m.LogPath(domain, worker)
	if err != nil {
		return "", err
	}
	data, _, err := tailFile(p, n)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// Follow writes the last n lines, then whatever is appended, until ctx
// ends. It polls the file size every half second rather than using
// inotify: the log is written by a process we do not control, through a
// descriptor it inherited, and size polling is the one method that sees
// every write regardless of how the writer buffers. Truncation (rotation)
// restarts from the top.
func (m *Manager) Follow(ctx context.Context, domain, worker string, n int, w io.Writer) error {
	p, err := m.LogPath(domain, worker)
	if err != nil {
		return err
	}
	flush := func() {
		if f, ok := w.(interface{ Flush() }); ok {
			f.Flush()
		}
	}
	data, offset, err := tailFile(p, n)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			return err
		}
		flush()
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st.Size() < offset {
			offset = 0
		}
		if st.Size() == offset {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			continue
		}
		chunk, err := io.ReadAll(io.LimitReader(f, 1<<20))
		f.Close()
		if err != nil {
			return err
		}
		offset += int64(len(chunk))
		if _, err := w.Write(chunk); err != nil {
			return err // the client went away
		}
		flush()
	}
}

// tailFile returns the last n lines of path and the file's size at the
// time of reading, which is where a follower continues from. It reads
// backwards in blocks so a gigabyte of log costs the same as a kilobyte.
func tailFile(path string, n int) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if n <= 0 || size == 0 {
		return nil, size, nil
	}
	const block = 32 << 10
	var buf []byte
	pos := size
	for pos > 0 {
		step := int64(block)
		if pos < step {
			step = pos
		}
		pos -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return nil, 0, err
		}
		buf = append(chunk, buf...)
		if bytes.Count(buf, []byte{'\n'}) > n {
			break
		}
	}
	// Keep the last n lines; a trailing newline does not count as a line.
	trimmed := bytes.TrimSuffix(buf, []byte{'\n'})
	lines := bytes.Count(trimmed, []byte{'\n'}) + 1
	for lines > n {
		i := bytes.IndexByte(trimmed, '\n')
		trimmed = trimmed[i+1:]
		lines--
	}
	if len(trimmed) > 0 && trimmed[len(trimmed)-1] != '\n' {
		trimmed = append(trimmed, '\n')
	}
	return trimmed, size, nil
}

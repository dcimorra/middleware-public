package guard

import (
	"bytes"
	"os"
	"strings"
)

// maxLogRead bounds what one sweep reads from one log: a site printing a
// gigabyte must not stall the guard; what is skipped is said.
const maxLogRead = 1 << 20

// newLines returns the lines appended to path since the last sweep. A log
// seen for the first time is read from its end: what is already in it is
// last week's, and waking someone for an old incident is the surest way
// for the next warning to go unread. A file that shrank was rotated, and
// is read from its start.
func (g *Guard) newLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	g.mu.Lock()
	off, seen := g.offsets[path]
	g.mu.Unlock()
	size := st.Size()
	switch {
	case !seen:
		g.setOffset(path, size)
		return nil
	case size < off:
		off = 0
	case size == off:
		return nil
	}
	start := off
	if size-start > maxLogRead {
		start = size - maxLogRead
	}
	buf := make([]byte, size-start)
	n, _ := f.ReadAt(buf, start)
	buf = buf[:n]
	// Only whole lines: a line half written is judged on the next sweep.
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return nil
	}
	g.setOffset(path, start+int64(end)+1)
	return strings.Split(string(buf[:end]), "\n")
}

func (g *Guard) setOffset(path string, off int64) {
	g.mu.Lock()
	g.offsets[path] = off
	g.mu.Unlock()
}

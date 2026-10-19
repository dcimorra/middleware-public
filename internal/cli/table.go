package cli

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Table writes rows under a header, columns padded to the widest cell,
// two spaces apart, no trailing spaces. Plain spaces rather than tabs:
// a terminal's tab stops are eight wide and a domain is often longer,
// which is how `pm2 list` style output ends up misaligned.
func Table(w io.Writer, header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && utf8.RuneCountInString(cell) > widths[i] {
				widths[i] = utf8.RuneCountInString(cell)
			}
		}
	}
	line := func(cells []string) {
		var b strings.Builder
		for i, cell := range cells {
			if i >= len(widths) {
				break
			}
			b.WriteString(cell)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(header)
	for _, row := range rows {
		line(row)
	}
}

// KeyValues writes "key  value" lines with the keys padded.
func KeyValues(w io.Writer, pairs [][2]string) {
	width := 0
	for _, p := range pairs {
		if utf8.RuneCountInString(p[0]) > width {
			width = utf8.RuneCountInString(p[0])
		}
	}
	for _, p := range pairs {
		fmt.Fprintf(w, "%-*s  %s\n", width, p[0], p[1])
	}
}

// yesNo is how a bool reads in a table.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ago renders a time relative to now, or "never" for the zero time.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// bytesHuman renders a byte count.
func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// Package logrotate keeps the logs the sites' processes write from growing
// without end. Those files are opened by the runtime with O_APPEND and
// held by the process for its whole life, so renaming one does nothing:
// the process keeps writing into the renamed file. The log is copied to
// .1 (the older ones shifted to .2, .3) and truncated in place; with
// O_APPEND the next write lands at the new, empty end. What is written
// between the copy and the truncation is lost: a few lines, once per
// rotation, which is the price of not restarting a site to rotate it.
package logrotate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Defaults: 10 MB a file and three old ones, so one log holds at most
// about 40 MB on disk.
const (
	DefaultMax  = 10 << 20
	DefaultKeep = 3
)

// Rotate rotates path when it is larger than max. It reports whether it did.
func Rotate(path string, max int64, keep int) (bool, error) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= max {
		return false, nil
	}
	for i := keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(from); err == nil {
			if err := os.Rename(from, fmt.Sprintf("%s.%d", path, i+1)); err != nil {
				return false, err
			}
		}
	}
	src, err := os.Open(path)
	if err != nil {
		return false, err
	}
	dst, err := os.OpenFile(path+".1", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		src.Close()
		return false, err
	}
	_, cerr := io.Copy(dst, src)
	src.Close()
	if err := dst.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return false, cerr
	}
	return true, os.Truncate(path, 0)
}

// Run rotates every log matched by the patterns once a minute; access.log
// is skipped, the traffic recorder rotates its own. onRotate is told of
// each rotation (an event, by the caller).
func Run(ctx context.Context, patterns []string, onRotate func(path string, err error)) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		for _, pat := range patterns {
			matches, _ := filepath.Glob(pat)
			for _, p := range matches {
				if filepath.Base(p) == "access.log" {
					continue
				}
				if did, err := Rotate(p, DefaultMax, DefaultKeep); (did || err != nil) && onRotate != nil {
					onRotate(p, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

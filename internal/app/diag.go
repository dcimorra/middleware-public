package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/fsx"
)

// Diagnostics on SIGUSR1: `kill -USR1 <pid>` writes data/diag/<time>.txt
// with what a hung or slow supervisor looks like from inside (memory,
// open files, each site's state, and every goroutine's stack) without
// restarting it, which would destroy exactly the state worth looking at.
// The ten newest are kept.

const keepDiag = 10

// Diagnose writes the report to w.
func (a *App) Diagnose(w io.Writer) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(w, "middleware diagnostics, %s\nroot %s\n\n", time.Now().UTC().Format(time.RFC3339), a.Root)
	fmt.Fprintf(w, "goroutines %d\nheap %d MB in use, %d MB from the OS\ngc runs %d, last pause %s\n", runtime.NumGoroutine(), ms.HeapInuse>>20, ms.Sys>>20, ms.NumGC, time.Duration(ms.PauseNs[(ms.NumGC+255)%256]))
	if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
		fmt.Fprintf(w, "open files %d\n", len(fds))
	}
	fmt.Fprintln(w, "\nsites:")
	if a.Engine != nil {
		sites := a.Engine.Sites()
		sort.Slice(sites, func(i, j int) bool { return sites[i].Domain < sites[j].Domain })
		for _, s := range sites {
			fmt.Fprintf(w, "  %-40s %-9s alive=%-5v port=%-5d health=%-8s failures=%d handing=%v %s\n", s.Domain, s.Mode, s.Alive, s.Port, s.Health, s.Failures, s.Handing, s.Error)
			for name, wk := range s.Workers {
				fmt.Fprintf(w, "    worker %-20s alive=%-5v port=%-5d %s\n", name, wk.Alive, wk.Port, wk.Error)
			}
		}
	}
	fmt.Fprintln(w, "\ngoroutines:")
	_ = pprof.Lookup("goroutine").WriteTo(w, 2)
}

// writeDiag writes one report and prunes the old ones.
func (a *App) writeDiag() (string, error) {
	dir := filepath.Join(a.Root, "data", "diag")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, time.Now().UTC().Format("20060102T150405")+".txt")
	// Written whole and renamed into place: whoever opens the report sees
	// all of it or none, never the first half.
	var buf bytes.Buffer
	a.Diagnose(&buf)
	if err := fsx.WriteAtomic(path, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	if old, _ := filepath.Glob(filepath.Join(dir, "*.txt")); len(old) > keepDiag {
		sort.Strings(old)
		for _, p := range old[:len(old)-keepDiag] {
			_ = os.Remove(p)
		}
	}
	return path, nil
}

// watchDiag answers SIGUSR1 until ctx ends.
func (a *App) watchDiag(ctx context.Context) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				path, err := a.writeDiag()
				if err != nil {
					a.Bus.Emit(events.Event{Type: "diag.error", Actor: "engine", Level: "warn", Message: err.Error()})
					continue
				}
				a.Bus.Emit(events.Event{Type: "diag.written", Actor: "engine", Message: path})
			}
		}
	}()
}

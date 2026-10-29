package relocate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/manifest"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// A worker's export that cannot work was found only once the site had
// stopped for it: a move.export written to run on the machine (docker
// exec, a client the machine has) runs inside the worker's container, and
// exited 2 there with the site down. So before the stop, while the site
// serves, each export that waits for the stop is tried: its program is
// looked for where it will run, and, unless this same command has worked
// on this house before, it is run once into a scratch file that is thrown
// away. A failure refuses the move with the command's own words, before
// any of it is down.
//
// An export is expected to only read (a dump of a database that keeps
// serving), so running it once more costs time and nothing else; what
// that means for the database it reads is the manifest's to say, whatever
// the database is.

// Rehearser is an Exporter that can try its workers' exports where they
// will run, with the site serving. trial false only looks for each
// export's program, for a check that has seconds (a request's).
type Rehearser interface {
	Rehearse(ctx context.Context, domain string, manifestData []byte, workers []string, trial bool) (string, error)
}

var rehearsedMu sync.Mutex

func rehearsedPath(root string) string {
	return filepath.Join(root, "data", "relocate", ".rehearsed.json")
}

// rehearsalKey names an export as it will run: its program and arguments
// as the worker resolves them, and the image it runs in. Another image
// may lack the program; another argument may be another database.
func rehearsalKey(argv []string, w *manifest.Worker) string {
	image := "process"
	if w.Docker != nil {
		image = "image:" + w.Docker.Image
		if w.Docker.Image == "" {
			image = "build:" + w.Docker.Build
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00") + "\x00\x00" + image))
	return hex.EncodeToString(sum[:])
}

func rehearsed(root, key string) bool {
	rehearsedMu.Lock()
	defer rehearsedMu.Unlock()
	rec := map[string]time.Time{}
	_ = fsx.ReadJSON(rehearsedPath(root), &rec)
	_, ok := rec[key]
	return ok
}

func markRehearsed(root, key string) error {
	rehearsedMu.Lock()
	defer rehearsedMu.Unlock()
	rec := map[string]time.Time{}
	_ = fsx.ReadJSON(rehearsedPath(root), &rec)
	rec[key] = time.Now().UTC()
	if err := os.MkdirAll(filepath.Dir(rehearsedPath(root)), 0o755); err != nil {
		return err
	}
	return fsx.WriteJSON(rehearsedPath(root), rec, 0o644)
}

// errNotThere is a program that is not where the command runs.
var errNotThere = errors.New("not found")

// findProgram looks for argv0 where the export will run: inside container
// when it is set (with sh's command -v, the image's own PATH), otherwise
// on this machine as the house starts a process. An image without sh
// cannot be asked, and the trial says what it says.
func findProgram(ctx context.Context, d runtime.Docker, container, cwd, argv0 string) error {
	if container != "" {
		bin := d.Bin
		if bin == "" {
			bin = "docker"
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, "exec", "-w", cwd, container, "sh", "-c", `command -v "$0" >/dev/null 2>&1 || exit 3`, argv0)
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 3 {
			return errNotThere
		}
		return nil
	}
	p := argv0
	if strings.Contains(p, "/") {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if info, err := os.Stat(p); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return errNotThere
		}
		return nil
	}
	if _, err := exec.LookPath(p); err != nil {
		return errNotThere
	}
	return nil
}

// RehearseHere tries the exports of workers where the site is a site of
// this house (root), as ExportHere will run them once it stops: inside
// each worker's running container, or as the site's processes run.
func RehearseHere(ctx context.Context, root, siteDir, domain string, m *manifest.Manifest, workers []string, trial bool, d runtime.Docker) (string, error) {
	var said []string
	data := filepath.Join(root, "data")
	for _, name := range workers {
		w := m.Workers[name]
		if w == nil || w.Move == nil || len(w.Move.Export) == 0 {
			return "", fmt.Errorf("%s: not a worker whose data moves by command", name)
		}
		var st runtime.State
		if err := fsx.ReadJSON(runtime.StatePath(data, domain+"/"+name), &st); err != nil || (st.Container == "" && st.PID == 0) {
			return "", fmt.Errorf("%s: not running here, so its export cannot run: start it, or move the site once it is up", name)
		}
		spec := commandSpec(domain, siteDir, m, name, w.Move.Export)
		argv, _ := spec.WithPort(st.Port)
		where := "on this machine"
		if st.Container != "" {
			where = "inside its container (" + st.Container + ")"
		}
		if err := findProgram(ctx, d, st.Container, spec.Cwd, argv[0]); err != nil {
			return "", fmt.Errorf("%s: its export's program, %s, is not %s, which is where move.export runs: write it for there", name, argv[0], where)
		}
		key := rehearsalKey(argv, w)
		if rehearsed(root, key) {
			said = append(said, name+": its export has worked here before")
			continue
		}
		if !trial {
			said = append(said, name+": its export will be tried while the site serves, before it stops")
			continue
		}
		logFile := filepath.Join(data, "logs", domain, "move-"+name+"-trial.log")
		_ = os.Remove(logFile)
		// Its output, when the house writes it, goes to a scratch file of
		// the house's, never into the folder the first copy is about to
		// send. One that writes its own files writes them where it will
		// once the site stops, and they are written again then.
		scratch := ""
		if w.Move.Output != "" {
			scratch = filepath.Join(data, "relocate", ".rehearsal", domain+"-"+name)
		}
		began := time.Now()
		err := runCommand(ctx, d, st.Container, spec, st.Port, logFile, filepath.Join(data, "homes", domain), moveTimeout(w.Move), scratch)
		if scratch != "" {
			_ = os.Remove(scratch)
			_ = os.Remove(scratch + ".part")
		}
		if err != nil {
			return "", fmt.Errorf("%s: its export, tried %s while the site serves, failed, so the site was not stopped for it: %w", name, where, err)
		}
		if err := markRehearsed(root, key); err != nil {
			return "", err
		}
		said = append(said, fmt.Sprintf("%s: its export worked, tried while the site serves (%s)", name, time.Since(began).Round(time.Second)))
	}
	return strings.Join(said, " · "), nil
}

package relocate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/runtime"
)

// A worker that keeps its data outside the folder (a database in a named
// volume) says in its manifest how that data moves: move.export where the
// site is, with the site stopped, leaving a dump inside the folder that
// the last copy carries; and on the target, before the commit, the worker
// brought up on its own, move.ready until it answers, move.import, and the
// worker stopped again. The site is then put in place and starts on a
// database that already has its data. Where it was, the database has only
// been read, so undoing the move is starting the site there again.

// Exporter is a Source that runs its site's workers' move.export.
type Exporter interface {
	Export(ctx context.Context, domain string, manifestData []byte) (string, error)
}

// Migrator is a target that brings the staged site's workers up and runs
// their move.ready and move.import before the commit.
type Migrator interface {
	Migrate(ctx context.Context, domain string, manifestData []byte) (string, error)
}

// Planner is a Source that says which manifest the move will commit
// before it stops the site (a V1 one, converted or its site.v2.json).
type Planner interface {
	Planned(domain string) []byte
}

// moveTime bounds each move command when the manifest says nothing.
const moveTime = 30 * time.Minute

// readyTime bounds the wait for a worker brought up for its import.
const readyTime = 3 * time.Minute

// MovingWorkers are the names of the workers whose data moves by command.
func MovingWorkers(m *manifest.Manifest) []string {
	if m == nil {
		return nil
	}
	var out []string
	for name, w := range m.Workers {
		if w != nil && w.Move != nil && !w.Disabled {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// UsesDocker says the site, or one of its workers, runs in a container.
func UsesDocker(m *manifest.Manifest) bool {
	if m == nil {
		return false
	}
	if m.Run != nil && m.Run.Runtime == "docker" {
		return true
	}
	for _, w := range m.Workers {
		if w != nil && !w.Disabled && w.Runtime == "docker" {
			return true
		}
	}
	return false
}

func moveTimeout(mv *manifest.WorkerMove) time.Duration {
	if mv.Timeout > 0 {
		return time.Duration(mv.Timeout)
	}
	return moveTime
}

// commandSpec is a move command as the house runs any command of a site:
// in its folder, with its environment (the .env that says where its
// database is) and the worker's on top, and $PORT the worker's.
func commandSpec(domain, dir string, m *manifest.Manifest, name string, cmd []string) *manifest.Spec {
	spec := &manifest.Spec{Domain: domain + "/" + name + "/move", SiteDir: dir, Cwd: dir, Env: map[string]string{}, Runtime: "process"}
	if m.Run != nil {
		if site, err := manifest.Resolve(domain, dir, m); err == nil {
			for k, v := range site.Env {
				spec.Env[k] = v
			}
		}
	}
	for k, v := range manifest.WorkerSpec(domain, dir, name, m.Workers[name]).Env {
		spec.Env[k] = v
	}
	vars := map[string]string{"DOMAIN": domain, "SITE_DIR": dir}
	spec.Command = make([]string, len(cmd))
	for i, a := range cmd {
		spec.Command[i] = manifest.Expand(a, vars)
	}
	spec.Rehash()
	return spec
}

// runCommand runs one move command to its end, its output in logFile:
// inside container when it is set (a docker worker's), otherwise as a
// process of the site, as a task runs.
func runCommand(ctx context.Context, d runtime.Docker, container string, spec *manifest.Spec, port int, logFile, home string, timeout time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	argv, _ := spec.WithPort(port)
	what := strings.Join(argv, " ")
	if container != "" {
		out, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer out.Close()
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := d.ExecIn(cctx, container, spec, port, home, out); err != nil {
			if cctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("%s: cut after %s (its output is in %s)", what, timeout, logFile)
			}
			return fmt.Errorf("%s: %w (its output is in %s)", what, err, logFile)
		}
		return nil
	}
	inst, err := runtime.Process{}.Start(ctx, spec, port, logFile, home)
	if err != nil {
		return err
	}
	select {
	case err := <-inst.Done():
		if err != nil {
			return fmt.Errorf("%s: %w (its output is in %s)", what, err, logFile)
		}
		return nil
	case <-time.After(timeout):
		_ = inst.Stop(context.Background(), 5*time.Second)
		return fmt.Errorf("%s: cut after %s (its output is in %s)", what, timeout, logFile)
	case <-ctx.Done():
		_ = inst.Stop(context.Background(), 5*time.Second)
		return ctx.Err()
	}
}

// runAsOperator runs a source's move.export as the person who ran the
// import, in their environment with the site's on top: the database it
// reads is one that person's machine runs (a V1 site's container, which no
// house manages), reached however they reach it.
func runAsOperator(ctx context.Context, spec *manifest.Spec, port int, logFile string, timeout time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv, _ := spec.WithPort(port)
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = spec.Cwd
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if port != 0 {
		cmd.Env = append(cmd.Env, "PORT="+strconv.Itoa(port))
	}
	cmd.Stdout, cmd.Stderr = out, out
	// Its own group, killed whole when it is cut: pg_dump under a shell
	// outlives a kill of the shell alone.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		what := strings.Join(argv, " ")
		if cctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s: cut after %s (its output is in %s)", what, timeout, logFile)
		}
		return fmt.Errorf("%s: %w (its output is in %s)", what, err, logFile)
	}
	return nil
}

// workerPort is the worker's written port, or a free one for the time of
// the move when it has none.
func workerPort(w *manifest.Worker) (int, error) {
	if w.Port != 0 {
		return w.Port, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Export runs each moving worker's move.export in dir, as the person who
// runs the import. root is where its log goes (data/logs).
func Export(ctx context.Context, root, dir, domain string, m *manifest.Manifest) (string, error) {
	var said []string
	for _, name := range MovingWorkers(m) {
		w := m.Workers[name]
		port := w.Port
		logFile := filepath.Join(root, "data", "logs", domain, "move-"+name+".log")
		spec := commandSpec(domain, dir, m, name, w.Move.Export)
		if err := runAsOperator(ctx, spec, port, logFile, moveTimeout(w.Move)); err != nil {
			return "", fmt.Errorf("%s: export: %w", name, err)
		}
		said = append(said, name+": exported")
	}
	return strings.Join(said, " · "), nil
}

// Planned implements Planner.
func (s *V1Source) Planned(string) []byte { return s.converted }

// Export implements Exporter: with the V1 site stopped, against the
// database it was using.
func (s *V1Source) Export(ctx context.Context, domain string, manifestData []byte) (string, error) {
	m, err := manifest.Parse(manifestData)
	if err != nil {
		return "", err
	}
	return Export(ctx, s.Root, filepath.Join(s.Root, "sites", domain), domain, m)
}

// volumesFile lists the volumes a migration created in a staging, so a
// move that is undone takes them out with the rest.
func (r Receiver) volumesFile(domain string) string {
	return filepath.Join(r.staging(domain), "volumes.json")
}

func (r Receiver) docker() runtime.Docker {
	if r.Docker != nil {
		return *r.Docker
	}
	return runtime.Docker{}
}

// Migrate brings each moving worker of the staged site up on its own, waits
// for it (its port, then move.ready), runs move.import, and stops it. The
// named volumes it creates are written down first: a migration that fails
// halfway leaves nothing behind once the move is undone.
func (r Receiver) Migrate(ctx context.Context, domain string, manifestData []byte) (string, error) {
	if !manifest.ValidName(domain) {
		return "", ErrBadName
	}
	dir := r.StagingSite(domain)
	if !fsx.Exists(dir) {
		return "", ErrNoStaging
	}
	m, err := manifest.Parse(manifestData)
	if err != nil {
		return "", fmt.Errorf("the manifest sent with the move: %w", err)
	}
	d := r.docker()
	var volumes []string
	for _, name := range MovingWorkers(m) {
		w := m.Workers[name]
		if w.Docker == nil {
			continue
		}
		for _, v := range w.Docker.Volumes {
			src, err := manifest.VolumeSource(v)
			if err != nil || !src.Named || src.Name == "" {
				continue
			}
			vol := runtime.NamedVolume(domain, src.Name)
			// One already here is data of an earlier life of the name, or
			// of an attempt that was not cleaned: importing over it would
			// mix the two, and removing it is not a move's decision.
			if d.VolumeExists(ctx, vol) {
				return "", fmt.Errorf("%s: the volume %s is already on this node (an earlier attempt, or a site that had this name): remove it with docker volume rm %s and move again", name, vol, vol)
			}
			volumes = append(volumes, vol)
		}
	}
	if err := fsx.WriteJSON(r.volumesFile(domain), volumes, 0o644); err != nil {
		return "", err
	}
	home := filepath.Join(r.Root, "data", "homes", domain)
	var said []string
	for _, name := range MovingWorkers(m) {
		w := m.Workers[name]
		port, err := workerPort(w)
		if err != nil {
			return "", err
		}
		logFile := filepath.Join(r.Root, "data", "logs", domain, "move-"+name+".log")
		spec := manifest.WorkerSpec(domain, dir, name, w)
		var rt runtime.Runtime = runtime.Process{}
		if spec.Runtime == "docker" {
			rt = d
		}
		if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
			return "", err
		}
		if err := os.MkdirAll(home, 0o700); err != nil {
			return "", err
		}
		inst, err := rt.Start(ctx, spec, port, logFile, home)
		if err != nil {
			return "", fmt.Errorf("%s: bringing it up for its import: %w", name, err)
		}
		// A docker worker's commands run inside its container: the machine
		// need not have the database's tools (the image has them), and the
		// command never holds the docker socket.
		container := ""
		if spec.Runtime == "docker" {
			container = runtime.ContainerName(spec.Domain, port)
		}
		cmdSpec := func(cmd []string) *manifest.Spec { return commandSpec(domain, dir, m, name, cmd) }
		err = func() error {
			if err := runtime.WaitReady(ctx, inst, manifest.Health{}, readyTime); err != nil {
				return fmt.Errorf("it did not answer on its port: %w (its output is in %s)", err, logFile)
			}
			if len(w.Move.Ready) > 0 {
				if err := r.untilReady(ctx, d, container, cmdSpec(w.Move.Ready), port, logFile, home); err != nil {
					return err
				}
			}
			if err := runCommand(ctx, d, container, cmdSpec(w.Move.Import), port, logFile, home, moveTimeout(w.Move)); err != nil {
				return fmt.Errorf("import: %w", err)
			}
			return nil
		}()
		// Stopped with time to close its files: a database killed after its
		// import comes up again replaying what it had not written.
		stopErr := inst.Stop(context.Background(), 60*time.Second)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if stopErr != nil {
			return "", fmt.Errorf("%s: stopping it after its import: %w", name, stopErr)
		}
		said = append(said, name+": imported")
	}
	return strings.Join(said, " · "), nil
}

// untilReady runs move.ready until it succeeds, for readyTime at most.
func (r Receiver) untilReady(ctx context.Context, d runtime.Docker, container string, spec *manifest.Spec, port int, logFile, home string) error {
	deadline := time.Now().Add(readyTime)
	for {
		err := runCommand(ctx, d, container, spec, port, logFile, home, time.Minute)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ready: still failing after %s: %w", readyTime, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// removeVolumes takes out the volumes listed in path, if it is there.
func (r Receiver) removeVolumes(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var vols []string
	if err := json.Unmarshal(raw, &vols); err != nil {
		return err
	}
	return r.removeVolumeList(vols)
}

func (r Receiver) removeVolumeList(vols []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var errs []string
	for _, v := range vols {
		if err := r.docker().RemoveVolume(ctx, v); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

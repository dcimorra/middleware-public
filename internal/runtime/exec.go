package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/middleware-public/internal/manifest"
)

// ExecOptions are a one-off command in a site's environment.
type ExecOptions struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// TTY asks docker for a terminal (an interactive shell).
	TTY bool
	// Env is added on top of the site's environment.
	Env map[string]string
}

// Exec runs a command where the site runs: same folder, same environment
// and HOME, and for a docker site inside a fresh container of its image,
// behind the same walls. It is what `npm install` or a migration needs: run
// on the host, a container site's native modules are built for the wrong
// system and its node_modules belong to the host's Node. The site's memory
// and CPU caps are not applied (an install needs more than serving does and
// a half-killed install leaves the dependencies broken); the walls are.
func Exec(ctx context.Context, spec *manifest.Spec, home string, o ExecOptions) (int, error) {
	if len(o.Args) == 0 {
		return 2, errors.New("exec: no command")
	}
	_, env := spec.WithPort(0)
	for k, v := range o.Env {
		env[k] = v
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return 1, err
	}
	var cmd *exec.Cmd
	inDocker := spec.Runtime == "docker" && spec.Docker != nil
	if inDocker {
		d := Docker{}
		image := spec.Docker.Image
		if spec.Docker.Build != "" {
			image = "middleware/" + strings.ReplaceAll(spec.Domain, ".", "-") + ":" + spec.Revision
		}
		name := fmt.Sprintf("mw-exec-%s-%d", strings.ReplaceAll(spec.Domain, ".", "-"), time.Now().UnixNano())
		args := []string{"run", "--rm", "-i", "--name", name}
		if o.TTY {
			args = append(args, "-t")
		}
		user := spec.Docker.User
		if user == "" {
			user = d.defaultUser()
		}
		args = append(args, "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "4096")
		if user != "" {
			args = append(args, "--user", user)
		}
		mounts, err := siteMounts(spec, home)
		if err != nil {
			return 1, err
		}
		args = append(args, mounts...)
		args = append(args, "-w", spec.Cwd)
		for _, kv := range containerEnv(env, 0, spec.Domain, spec.SiteDir, home) {
			if !strings.HasPrefix(kv, "PORT=") {
				args = append(args, "-e", kv)
			}
		}
		args = append(args, image)
		args = append(args, o.Args...)
		cmd = exec.CommandContext(ctx, d.bin(), args...)
		// Killing the docker client leaves the container running: on a
		// timeout or a cancel the container itself is killed.
		cmd.Cancel = func() error {
			_ = exec.Command(d.bin(), "kill", name).Run()
			return cmd.Process.Kill()
		}
	} else {
		bin := o.Args[0]
		if nb := withNode(ctx, spec.SiteDir, o.Args, env); nb != "" {
			bin = nb
		}
		cmd = exec.CommandContext(ctx, bin, o.Args[1:]...)
		cmd.Dir = spec.Cwd
		for _, kv := range buildEnv(env, 0, spec.Domain, spec.SiteDir, home) {
			if !strings.HasPrefix(kv, "PORT=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
	}
	if !inDocker {
		// Its own group, killed whole on a timeout or a cancel: killing
		// only the shell npm spawned leaves npm and its children running.
		// (CommandContext's own Cancel kills the one process only, which
		// is why this replaces it rather than filling a blank.)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	}
	// A step that leaves a daemon holding its output must not hang the
	// caller after the step itself ended.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin, cmd.Stdout, cmd.Stderr = o.Stdin, o.Stdout, o.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

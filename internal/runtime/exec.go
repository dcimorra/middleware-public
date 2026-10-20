package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

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
	if err := os.MkdirAll(home, 0o700); err != nil {
		return 1, err
	}
	var cmd *exec.Cmd
	if spec.Runtime == "docker" && spec.Docker != nil {
		d := Docker{}
		image := spec.Docker.Image
		if spec.Docker.Build != "" {
			image = "middleware/" + strings.ReplaceAll(spec.Domain, ".", "-") + ":" + spec.Revision
		}
		args := []string{"run", "--rm", "-i"}
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
	} else {
		cmd = exec.CommandContext(ctx, o.Args[0], o.Args[1:]...)
		cmd.Dir = spec.Cwd
		for _, kv := range buildEnv(env, 0, spec.Domain, spec.SiteDir, home) {
			if !strings.HasPrefix(kv, "PORT=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
	}
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

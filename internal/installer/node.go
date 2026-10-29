package installer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dcimorra/the-middleware/internal/nodeenv"
)

// nvm on every machine, with the current LTS. The house installs any Node
// a site asks for through nvm when it is first needed (nodeenv); doing the
// nvm part here means a site's first start, or a site moved in from
// another node, does not wait for a git clone, and a machine where nvm
// cannot be fetched says so at install, not in a site's log weeks later.
func prepareNode(root, as string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	m := &nodeenv.Manager{Root: root}
	if as != "" {
		run, err := runAs(as)
		if err != nil {
			return false, "", err
		}
		m.Run = run
	}
	return m.Prepare(ctx, "lts")
}

func (in *Installer) stepNode() (bool, string, error) {
	if in.Node == nil {
		return false, "skipped", nil
	}
	if !in.AsRoot {
		return in.Node(in.Params.Root, "")
	}
	// As root, nvm runs as the operator's user. It sources
	// modules/nvm/nvm.sh, which that user owns and every site running as
	// it can rewrite: run as root, the helper's requirements.install
	// (which the supervisor may ask for) ran whatever had been written
	// there, as root. What root made under modules/ on an older install
	// goes to the user first, or nvm could not write there now.
	if _, _, err := in.stepOwnership(); err != nil {
		return false, "", err
	}
	return in.Node(in.Params.Root, in.Params.User)
}

// runAs runs nodeenv's commands as the named user, with that user's
// groups and, where the environment brings root's HOME, the user's.
func runAs(name string) (func(ctx context.Context, env []string, cmd string, args ...string) ([]byte, error), error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil || uid == 0 {
		return nil, fmt.Errorf("%s is not a user nvm may run as", name)
	}
	var groups []uint32
	if ids, err := u.GroupIds(); err == nil {
		for _, id := range ids {
			if g, err := strconv.ParseUint(id, 10, 32); err == nil {
				groups = append(groups, uint32(g))
			}
		}
	}
	rootHome := os.Getenv("HOME")
	return func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		for i, kv := range env {
			if kv == "HOME="+rootHome {
				env[i] = "HOME=" + u.HomeDir
			}
		}
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if len(msg) > 400 {
				msg = msg[len(msg)-400:]
			}
			return out.Bytes(), fmt.Errorf("%v: %s", err, msg)
		}
		return out.Bytes(), nil
	}, nil
}

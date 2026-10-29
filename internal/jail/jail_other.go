//go:build !linux

package jail

import (
	"errors"
	"os/exec"
)

var errNoJail = errors.New("jail: only on Linux")

// Wrap is not available off Linux.
func Wrap(cmd *exec.Cmd, plan *Plan) error { return errNoJail }

// Available says no off Linux.
func Available() error { return errNoJail }

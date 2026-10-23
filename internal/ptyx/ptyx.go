// Package ptyx opens a pseudo-terminal and starts a process attached to
// it, using nothing but syscalls.
//
// A real PTY is not optional for what this is for. Claude Code and the
// Antigravity CLI, like vim or top, ask the terminal how big it is, switch to
// the alternate screen, read keys one at a time and paint with escape
// sequences. Given a pipe instead they turn into programs that print logs
// and ignore the keyboard, and a sign-in that waits for a pasted code
// never asks for it.
package ptyx

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// Linux ioctls. Same values on amd64 and arm64: they come from
// asm-generic.
const (
	tiocGPTN   = 0x80045430 // number of the slave
	tiocSPTLCK = 0x40045431 // lock/unlock the slave
	tiocSWINSZ = 0x5414     // set window size
)

// Winsize is the terminal size a program reads to lay itself out.
type Winsize struct {
	Rows, Cols     uint16
	XPixel, YPixel uint16
}

// Pty is the master side. Reading from it is what the program printed;
// writing to it is what the user typed.
type Pty struct {
	*os.File
	slaveName string
}

// Open creates a new pseudo-terminal pair and returns the master.
func Open() (*Pty, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// Unlocking the slave is a separate step, and forgetting it makes the
	// open below fail with EIO for no visible reason.
	var unlock int32
	if err := ioctl(master.Fd(), tiocSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		master.Close()
		return nil, fmt.Errorf("unlock the slave: %w", err)
	}
	var n uint32
	if err := ioctl(master.Fd(), tiocGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		master.Close()
		return nil, fmt.Errorf("slave number: %w", err)
	}
	return &Pty{File: master, slaveName: fmt.Sprintf("/dev/pts/%d", n)}, nil
}

// Resize tells the program on the other side how big the window is now.
func (p *Pty) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return nil
	}
	ws := Winsize{Rows: rows, Cols: cols}
	return ioctl(p.Fd(), tiocSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

// Start runs cmd attached to this terminal.
//
// The three flags matter together: Setsid makes the child a session
// leader, Setctty makes this terminal its controlling one, and Ctty says
// which of its descriptors that is. Without them there is no job control
// and Ctrl-C reaches nobody.
func (p *Pty) Start(cmd *exec.Cmd) error {
	slave, err := os.OpenFile(p.slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", p.slaveName, err)
	}
	defer slave.Close()

	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = 0

	return cmd.Start()
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

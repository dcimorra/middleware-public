//go:build linux

package helper

import (
	"errors"
	"net"
	"syscall"
)

// peerCredentials asks the kernel who is on the other end of a unix socket.
// SO_PEERCRED is the credential at connect time, which cannot be forged by
// the peer; the socket's file mode is the first line of defence and this
// is the second, so a mode mistake on the socket file is not a root shell.
func peerCredentials(conn net.Conn) (Peer, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Peer{}, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var cred *syscall.Ucred
	var cerr error
	err = raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return Peer{}, err
	}
	if cerr != nil {
		return Peer{}, cerr
	}
	return Peer{UID: cred.Uid, GID: cred.Gid, PID: cred.Pid}, nil
}

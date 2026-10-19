// Package ports hands out loopback ports for site instances. The range is
// reserved from the kernel's ephemeral range by the installer (through the
// privileged helper), because a port chosen here and then grabbed by an
// outgoing connection of some other process is a failure that gives no error
// anywhere: the new instance just fails to bind and the handover never ends.
package ports

import (
	"errors"
	"fmt"
	"net"
	"sync"
)

// Default range. 33000–47000 sits inside Linux's ephemeral range on purpose:
// nothing else on a typical machine listens there, and the installer reserves
// it so nothing ephemeral lands on it either.
const (
	DefaultFrom = 33000
	DefaultTo   = 47000
)

// Allocator hands out ports that are both unused by the kernel and not
// currently claimed by another instance of this process.
type Allocator struct {
	mu     sync.Mutex
	from   int
	to     int
	next   int
	inUse  map[int]string // port → owner (domain), for diagnostics
	listen func(int) error
}

// New builds an allocator over [from, to]. A zero range uses the default.
func New(from, to int) *Allocator {
	if from == 0 || to == 0 {
		from, to = DefaultFrom, DefaultTo
	}
	return &Allocator{from: from, to: to, next: from, inUse: map[int]string{}, listen: probe}
}

// Claim returns a free port and records owner as holding it. It walks the
// range from where the last claim left off, so a port just released by a
// draining instance is not handed to the next one immediately: the old
// process may still be closing its socket.
func (a *Allocator) Claim(owner string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	size := a.to - a.from + 1
	for i := 0; i < size; i++ {
		p := a.from + (a.next-a.from+i)%size
		if _, taken := a.inUse[p]; taken {
			continue
		}
		if err := a.listen(p); err != nil {
			continue
		}
		a.inUse[p] = owner
		a.next = p + 1
		if a.next > a.to {
			a.next = a.from
		}
		return p, nil
	}
	return 0, errors.New("no free port in range")
}

// Reserve marks a port as held without probing it: used when re-adopting an
// instance that is already listening there.
func (a *Allocator) Reserve(port int, owner string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inUse[port] = owner
}

// Release lets the port be claimed again.
func (a *Allocator) Release(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inUse, port)
}

// InUse returns a copy of the current claims.
func (a *Allocator) InUse() map[int]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[int]string, len(a.inUse))
	for k, v := range a.inUse {
		out[k] = v
	}
	return out
}

func probe(port int) error {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	return l.Close()
}

// Package brake is the one thing that slows password guessing down, for
// every door in the house: the panel's login and each site's and zone's
// password. It counts failures per key and, past a few, locks that key
// out for a while that doubles; a success forgets the key, so whoever
// knows the password never notices it. V1 had one copy of this for the
// panel and none for sites, where a `curl -u` loop filled four cores with
// key derivations; two copies would drift, so there is one.
//
// It also holds the house-wide cap on key derivations running at once
// (Derive), because a brake per key does nothing against guesses spread
// over many addresses.
package brake

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

// Policy is how forgiving a door is: Failures inside Window lock the key
// for Window, and every lock after that doubles, up to MaxLock.
type Policy struct {
	Failures int
	Window   time.Duration
	MaxLock  time.Duration
}

// Brake counts failures per key. The zero value is not usable; use New.
type Brake struct {
	// Now is the clock; tests move it.
	Now    func() time.Time
	policy Policy
	mu     sync.Mutex
	keys   map[string]*record
}

type record struct {
	failures    []time.Time
	lockedUntil time.Time
	lockLevel   int
	lastSeen    time.Time
}

// New returns an empty brake.
func New(p Policy) *Brake {
	return &Brake{Now: time.Now, policy: p, keys: map[string]*record{}}
}

// Key is what one guesser is counted as. An IPv4 address is one
// household; an IPv6 address is not, because any home connection has a
// whole /64 to pick from, and counting single addresses there would be
// counting nothing.
func Key(a netip.Addr) string {
	a = a.Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// KeyOf is Key for an address written as text; text that is not an
// address is its own key.
func KeyOf(s string) string {
	if a, err := netip.ParseAddr(s); err == nil {
		return Key(a)
	}
	return s
}

// Blocked reports whether key is locked out, and for how much longer.
func (b *Brake) Blocked(key string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep()
	r, ok := b.keys[key]
	if !ok {
		return false, 0
	}
	left := r.lockedUntil.Sub(b.Now())
	return left > 0, max(left, 0)
}

// Failed counts one failure against key.
func (b *Brake) Failed(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.Now()
	r, ok := b.keys[key]
	if !ok {
		r = &record{}
		b.keys[key] = r
	}
	r.lastSeen = now
	recent := r.failures[:0]
	for _, t := range r.failures {
		if now.Sub(t) < b.policy.Window {
			recent = append(recent, t)
		}
	}
	r.failures = append(recent, now)
	if len(r.failures) < b.policy.Failures {
		return
	}
	lock := b.policy.Window << r.lockLevel
	if lock > b.policy.MaxLock || lock <= 0 {
		lock = b.policy.MaxLock
	}
	r.lockedUntil = now.Add(lock)
	r.lockLevel++
	r.failures = nil
}

// Passed forgets key.
func (b *Brake) Passed(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.keys, key)
}

// sweep forgets keys nobody has tried for longer than the longest lock,
// so a scan from many addresses cannot grow the map without bound.
// Called under lock.
func (b *Brake) sweep() {
	if len(b.keys) < 1000 {
		return
	}
	now := b.Now()
	for k, r := range b.keys {
		if now.Sub(r.lastSeen) > b.policy.MaxLock && now.After(r.lockedUntil) {
			delete(b.keys, k)
		}
	}
}

// slots is how many key derivations run at once in the whole house. Each
// is tens of milliseconds of CPU; V1 measured four cores filled by about
// 140 wrong passwords a second, which took every site on the machine down
// with the one being guessed. With two slots, a parallel guesser waits its
// turn and the rest of the machine carries on.
var slots = make(chan struct{}, 2)

// slotWait is the longest a check waits for a slot before it counts as
// failed. Someone who knows the password rarely gets here: their pass and
// the doors' caches of good passwords spare them the derivation.
const slotWait = 3 * time.Second

// Derive runs check, a key derivation, once a slot is free, and returns
// its answer. ran is false when no slot freed up in time or ctx ended:
// the password was not checked, so the caller refuses the attempt
// without counting it against anyone, since the one waiting may be the
// owner caught behind somebody else's flood.
func Derive(ctx context.Context, check func() bool) (ok, ran bool) {
	t := time.NewTimer(slotWait)
	defer t.Stop()
	select {
	case slots <- struct{}{}:
	case <-t.C:
		return false, false
	case <-ctx.Done():
		return false, false
	}
	defer func() { <-slots }()
	return check(), true
}

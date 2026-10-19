package auth

import (
	"sync"
	"time"
)

// Rate limit on login failures: this many failures inside the window lock
// the address out for the window, and every further failure while locked
// doubles the lockout. Five per minute lets a person mistype a few times
// and makes an online guess of an 8-character password take centuries.
const (
	loginFailures = 5
	loginWindow   = time.Minute
	loginMaxLock  = 24 * time.Hour
)

type limiter struct {
	now func() time.Time
	mu  sync.Mutex
	ips map[string]*ipRecord
}

type ipRecord struct {
	failures    []time.Time
	lockedUntil time.Time
	lockLevel   int
	lastSeen    time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, ips: map[string]*ipRecord{}}
}

func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep()
	r, ok := l.ips[ip]
	return ok && l.now().Before(r.lockedUntil)
}

func (l *limiter) failed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	r, ok := l.ips[ip]
	if !ok {
		r = &ipRecord{}
		l.ips[ip] = r
	}
	r.lastSeen = now
	recent := r.failures[:0]
	for _, t := range r.failures {
		if now.Sub(t) < loginWindow {
			recent = append(recent, t)
		}
	}
	r.failures = append(recent, now)
	if len(r.failures) < loginFailures {
		return
	}
	lock := loginWindow << r.lockLevel
	if lock > loginMaxLock {
		lock = loginMaxLock
	}
	r.lockedUntil = now.Add(lock)
	r.lockLevel++
	r.failures = nil
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}

// sweep forgets addresses nobody has seen for a day, so the map cannot
// grow without bound under a scan from many addresses. Called under lock.
func (l *limiter) sweep() {
	if len(l.ips) < 1000 {
		return
	}
	now := l.now()
	for ip, r := range l.ips {
		if now.Sub(r.lastSeen) > loginMaxLock && now.After(r.lockedUntil) {
			delete(l.ips, ip)
		}
	}
}

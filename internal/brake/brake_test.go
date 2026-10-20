package brake

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFailuresInsideTheWindowLockAndTheLockDoubles(t *testing.T) {
	b := New(Policy{Failures: 3, Window: time.Minute, MaxLock: 3 * time.Minute})
	now := time.Now()
	b.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if blocked, _ := b.Blocked("k"); blocked {
			t.Fatalf("locked after %d failures", i)
		}
		b.Failed("k")
	}
	if blocked, left := b.Blocked("k"); !blocked || left != time.Minute {
		t.Fatalf("blocked=%v left=%v", blocked, left)
	}
	now = now.Add(time.Minute)
	for i := 0; i < 3; i++ {
		b.Failed("k")
	}
	if _, left := b.Blocked("k"); left != 2*time.Minute {
		t.Fatalf("second lock %v, want doubled", left)
	}
	now = now.Add(2 * time.Minute)
	for i := 0; i < 3; i++ {
		b.Failed("k")
	}
	if _, left := b.Blocked("k"); left != 3*time.Minute {
		t.Fatalf("third lock %v, want the cap", left)
	}
	b.Passed("k")
	if blocked, _ := b.Blocked("k"); blocked {
		t.Fatal("a success did not forget the key")
	}
}

func TestIPv6IsCountedPerSlash64(t *testing.T) {
	a := Key(netip.MustParseAddr("2001:db8:1:2::1"))
	if a != Key(netip.MustParseAddr("2001:db8:1:2:ffff::9")) || a == Key(netip.MustParseAddr("2001:db8:1:3::1")) {
		t.Fatalf("v6 keys: %s", a)
	}
	if Key(netip.MustParseAddr("::ffff:192.0.2.1")) != "192.0.2.1" || KeyOf("192.0.2.1") != "192.0.2.1" {
		t.Fatal("a mapped v4 address must count as itself")
	}
}

func TestNoMoreThanTwoDerivationsRunAtOnce(t *testing.T) {
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ran := Derive(context.Background(), func() bool {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				running.Add(-1)
				return true
			})
			if !ran {
				t.Error("a derivation did not get a slot within the wait")
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak %d derivations at once, want 2", p)
	}
}

func TestAWaiterGivesUpWithoutRunningWhenItsRequestEnds(t *testing.T) {
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Derive(context.Background(), func() bool { <-release; return true })
		}()
	}
	for len(slots) < 2 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ok, ran := Derive(ctx, func() bool { t.Error("ran without a slot"); return true })
	if ok || ran {
		t.Fatalf("ok=%v ran=%v", ok, ran)
	}
	close(release)
	wg.Wait()
}

package ports

import (
	"errors"
	"testing"
)

func TestClaimSkipsPortsTheKernelRefuses(t *testing.T) {
	a := New(40000, 40003)
	a.listen = func(p int) error {
		if p == 40000 {
			return errors.New("busy")
		}
		return nil
	}
	p, err := a.Claim("a.test")
	if err != nil || p != 40001 {
		t.Fatalf("got %d, %v", p, err)
	}
}

func TestClaimDoesNotReuseAJustReleasedPortFirst(t *testing.T) {
	a := New(40000, 40002)
	a.listen = func(int) error { return nil }
	p1, _ := a.Claim("a")
	a.Release(p1)
	p2, _ := a.Claim("a")
	if p2 == p1 {
		t.Fatalf("released port %d was handed out again immediately", p1)
	}
}

func TestClaimFailsWhenTheRangeIsFull(t *testing.T) {
	a := New(40000, 40001)
	a.listen = func(int) error { return nil }
	a.Claim("a")
	a.Claim("b")
	if _, err := a.Claim("c"); err == nil {
		t.Fatal("expected an error")
	}
}

// Package firewall is the unprivileged face of the layer-3 block list. The
// WAF decides who is blocked; this package forwards the decision to the
// helper when there is one and says nothing (once) when there is not, so
// an installation without the helper service behaves exactly like one with
// it, minus the kernel-level drop.
package firewall

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/helper"
)

// Policy is what Apply loads; it is the helper's type so there is one
// definition of a firewall policy in the whole program.
type Policy = helper.FirewallPolicy

// Firewall forwards blocks and policies to the helper.
type Firewall struct {
	client *helper.Client
	bus    *events.Bus
	warn   sync.Once
}

// New returns a firewall over client. A nil client means "no helper": every
// call is a no-op that reports ErrNoHelper, and one warn event is emitted
// the first time something would have reached the kernel.
func New(client *helper.Client, bus *events.Bus) *Firewall {
	return &Firewall{client: client, bus: bus}
}

// ErrNoHelper says the call did nothing because the helper is not there.
var ErrNoHelper = errors.New("firewall: helper is not available")

// Available reports whether the helper answers right now.
func (f *Firewall) Available() bool {
	return f.client != nil && f.client.Available()
}

func (f *Firewall) emit(e events.Event) {
	if f.bus != nil {
		f.bus.Emit(e)
	}
}

// unavailable handles the "no helper" case: one warning for the life of
// the process, so the operator learns about it, and silence afterwards, so
// a flood that blocks a thousand addresses does not write a thousand lines.
func (f *Firewall) unavailable(err error) error {
	f.warn.Do(func() {
		f.emit(events.Event{Type: "firewall.unavailable", Actor: "engine", Level: "warn",
			Message: "the helper socket is not reachable; blocks stay at layer 7 only"})
	})
	if err == nil {
		return ErrNoHelper
	}
	return errors.Join(ErrNoHelper, err)
}

// OnBlock is what the WAF calls when it blocks an address. The block is
// pushed to the kernel with the same duration so the flood stops before it
// reaches Go.
func (f *Firewall) OnBlock(ip string, d time.Duration) error {
	return f.Block(ip, d, "")
}

// Block adds ip to the kernel block set for d, with a reason for the log.
func (f *Firewall) Block(ip string, d time.Duration, reason string) error {
	if f.client == nil {
		return f.unavailable(nil)
	}
	err := f.client.FirewallBlock(context.Background(), ip, d, reason)
	if errors.Is(err, helper.ErrUnavailable) {
		return f.unavailable(err)
	}
	if err != nil {
		f.emit(events.Event{Type: "firewall.block.failed", Subject: ip, Actor: "engine", Level: "error", Message: err.Error()})
		return err
	}
	f.emit(events.Event{Type: "firewall.blocked", Subject: ip, Actor: "engine", Data: map[string]any{"duration": d.String(), "reason": reason}})
	return nil
}

// Unblock removes ip from the kernel block set.
func (f *Firewall) Unblock(ip string) error {
	if f.client == nil {
		return f.unavailable(nil)
	}
	err := f.client.FirewallUnblock(context.Background(), ip)
	if errors.Is(err, helper.ErrUnavailable) {
		return f.unavailable(err)
	}
	if err != nil {
		f.emit(events.Event{Type: "firewall.unblock.failed", Subject: ip, Actor: "engine", Level: "error", Message: err.Error()})
		return err
	}
	f.emit(events.Event{Type: "firewall.unblocked", Subject: ip, Actor: "engine"})
	return nil
}

// Apply loads the policy as the middleware table.
func (f *Firewall) Apply(p Policy) error {
	if f.client == nil {
		return f.unavailable(nil)
	}
	err := f.client.FirewallApply(context.Background(), p)
	if errors.Is(err, helper.ErrUnavailable) {
		return f.unavailable(err)
	}
	if err != nil {
		f.emit(events.Event{Type: "firewall.apply.failed", Actor: "engine", Level: "error", Message: err.Error()})
		return err
	}
	f.emit(events.Event{Type: "firewall.applied", Actor: "engine", Data: map[string]any{"vpn_only": p.VPNOnly, "allow_ports": p.AllowPorts, "panel_ports": p.PanelPorts}})
	return nil
}

// Status lists what the kernel currently blocks. Without the helper the
// list is empty and the error says why.
func (f *Firewall) Status() ([]helper.Blocked, error) {
	if f.client == nil {
		return nil, f.unavailable(nil)
	}
	st, err := f.client.FirewallStatus(context.Background())
	if errors.Is(err, helper.ErrUnavailable) {
		return nil, f.unavailable(err)
	}
	return st.Blocked, err
}

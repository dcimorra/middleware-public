package nodes

import (
	"context"

	"github.com/dcimorra/the-middleware/internal/helper"
	"github.com/dcimorra/the-middleware/internal/wireguard"
)

// HelperNetwork is the mesh's interface and firewall through the helper:
// the same WireGuard operations the VPN uses, on another interface, and a
// firewall table of its own.
type HelperNetwork struct{ Client *helper.Client }

// Apply writes the interface's config and brings it up, through a down
// first when its address or port changed (wg syncconf changes peers and
// keys, not those).
func (h HelperNetwork) Apply(ctx context.Context, cfg wireguard.ServerConfig, restart bool) error {
	if err := h.Client.VPNApply(ctx, helper.VPNApplyArgs{Config: cfg}); err != nil {
		return err
	}
	if restart {
		if err := h.Client.VPNDown(ctx, Interface); err != nil {
			return err
		}
	}
	return h.Client.VPNUp(ctx, Interface)
}

// Down takes the interface down.
func (h HelperNetwork) Down(ctx context.Context) error { return h.Client.VPNDown(ctx, Interface) }

// Firewall loads or removes the mesh's rules.
func (h HelperNetwork) Firewall(ctx context.Context, a helper.MeshFirewallArgs) error {
	return h.Client.MeshFirewall(ctx, a)
}

// Status reads the interface.
func (h HelperNetwork) Status(ctx context.Context) (helper.VPNStatus, error) {
	return h.Client.VPNStatus(ctx, Interface)
}

// LoopbackNetwork is no network at all: the mesh's addresses are loopback
// ones, which answer without an interface. It exists to run several
// installations on one machine without root (MIDDLEWARE_MESH_LOOPBACK=1).
type LoopbackNetwork struct{}

// Apply does nothing.
func (LoopbackNetwork) Apply(context.Context, wireguard.ServerConfig, bool) error { return nil }

// Down does nothing.
func (LoopbackNetwork) Down(context.Context) error { return nil }

// Firewall does nothing.
func (LoopbackNetwork) Firewall(context.Context, helper.MeshFirewallArgs) error { return nil }

// Status says up.
func (LoopbackNetwork) Status(context.Context) (helper.VPNStatus, error) {
	return helper.VPNStatus{Up: true}, nil
}

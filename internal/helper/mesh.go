package helper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/dcimorra/middleware-public/internal/wireguard"
)

// MeshTable is the nftables table of the nodes' mesh. Its own table, not a
// chain in the middleware one: that one is flushed and refilled by the
// VPN's policy, and two owners of one chain is each erasing the other.
const MeshTable = "middleware_mesh"

// meshTemplate closes the mesh interface to everything but the nodes' API.
// Being in the same network as another machine must not make its SSH, a
// database listening on every address or a site's port reachable from it:
// what the mesh is for is the middleware talking to the middleware.
// Replies to what this node opened (its own calls to the others) are let
// back in by the conntrack line.
var meshTemplate = template.Must(template.New("mesh").Parse(`table inet {{.Table}} {
	chain input {
		type filter hook input priority -5; policy accept;
	}
}
flush chain inet {{.Table}} input
table inet {{.Table}} {
	chain input {
		udp dport {{.ListenPort}} accept
		iifname "{{.Interface}}" ct state established,related accept
		iifname "{{.Interface}}" tcp dport {{.APIPort}} accept
		iifname "{{.Interface}}" drop
	}
}
`))

// RenderMeshRuleset produces the nft script for the mesh, validating first.
func RenderMeshRuleset(a MeshFirewallArgs) (string, error) {
	if !wireguard.InterfaceName.MatchString(a.Interface) {
		return "", fmt.Errorf("%w: interface %q must match %s", errBadArgument, a.Interface, wireguard.InterfaceName)
	}
	for _, p := range []int{a.APIPort, a.ListenPort} {
		if p < 1 || p > 65535 {
			return "", fmt.Errorf("%w: port %d out of range", errBadArgument, p)
		}
	}
	var b strings.Builder
	if err := meshTemplate.Execute(&b, struct {
		Table string
		MeshFirewallArgs
	}{MeshTable, a}); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (s *Server) meshFirewall(ctx context.Context, raw json.RawMessage) (any, error) {
	var a MeshFirewallArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if a.Down {
		if _, err := s.Exec.Run(ctx, "nft", []string{"delete", "table", "inet", MeshTable}, ""); err != nil && !strings.Contains(err.Error(), "No such file or directory") {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	script, err := RenderMeshRuleset(a)
	if err != nil {
		return nil, err
	}
	if _, err := s.Exec.Run(ctx, "nft", []string{"-f", "-"}, script); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

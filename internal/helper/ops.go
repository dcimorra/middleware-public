package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/wireguard"
)

// TableName is the nftables table the helper owns. Everything it does is
// inside this one table so an operator's own rules in other tables are
// never touched, and `nft delete table inet middleware` removes all of it.
const TableName = "middleware"

// ReservedPortsFile is the sysctl drop-in that keeps the site port range
// out of the kernel's ephemeral range. Without it an outgoing connection
// can grab a port the engine is about to hand to a site.
const ReservedPortsFile = "60-middleware-reserved-ports.conf"

// rulesetTemplate is the whole table. Two passes on purpose: the first
// `table` block creates the sets and the chain if they are missing (nft
// treats a declaration as "add"), then the chain is flushed and refilled.
// Flushing only the chain keeps the elements of the block sets, so applying
// a policy does not unblock every address that was blocked a minute ago.
// `delete table` + recreate, the usual idiom, would.
var rulesetTemplate = template.Must(template.New("ruleset").Funcs(template.FuncMap{"ports": joinPorts}).Parse(`table inet {{.Table}} {
	set blocked4 {
		type ipv4_addr
		flags timeout
	}
	set blocked6 {
		type ipv6_addr
		flags timeout
	}
	chain input {
		type filter hook input priority -10; policy accept;
	}
}
flush chain inet {{.Table}} input
table inet {{.Table}} {
	chain input {
		ip saddr @blocked4 drop
		ip6 saddr @blocked6 drop
		iifname "lo" accept
{{- if .AllowPorts}}
		tcp dport { {{ports .AllowPorts}} } accept
		udp dport { {{ports .AllowPorts}} } accept
{{- end}}
{{- if .VPNOnly}}
		iifname "{{.VPNInterface}}" tcp dport { {{ports .PanelPorts}} } accept
		tcp dport { {{ports .PanelPorts}} } drop
{{- end}}
	}
}
`))

func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}

func validPorts(what string, ports []int) ([]int, error) {
	out := make([]int, 0, len(ports))
	seen := map[int]bool{}
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("%w: %s port %d out of range", errBadArgument, what, p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out, nil
}

// RenderRuleset produces the nft script for a policy, validating it first.
// Exported so the unprivileged side can show the operator what will be
// loaded without asking the helper.
func RenderRuleset(p FirewallPolicy) (string, error) {
	var err error
	if p.AllowPorts, err = validPorts("allow", p.AllowPorts); err != nil {
		return "", err
	}
	if p.PanelPorts, err = validPorts("panel", p.PanelPorts); err != nil {
		return "", err
	}
	if p.VPNOnly {
		if !wireguard.InterfaceName.MatchString(p.VPNInterface) {
			return "", fmt.Errorf("%w: vpn interface %q must match %s", errBadArgument, p.VPNInterface, wireguard.InterfaceName)
		}
		if len(p.PanelPorts) == 0 {
			return "", fmt.Errorf("%w: vpn_only needs the panel ports to restrict", errBadArgument)
		}
	}
	var b strings.Builder
	err = rulesetTemplate.Execute(&b, struct {
		Table string
		FirewallPolicy
	}{TableName, p})
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

func (s *Server) firewallApply(ctx context.Context, raw json.RawMessage) (any, error) {
	var p FirewallPolicy
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	script, err := RenderRuleset(p)
	if err != nil {
		return nil, err
	}
	if _, err := s.Exec.Run(ctx, "nft", []string{"-f", "-"}, script); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func setFor(ip string) (string, string, error) {
	a, err := parseIP(ip)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", errBadArgument, errBadIP)
	}
	if a.Is4() {
		return "blocked4", a.String(), nil
	}
	return "blocked6", a.String(), nil
}

func (s *Server) firewallBlock(ctx context.Context, raw json.RawMessage) (any, error) {
	var a BlockArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	set, ip, err := setFor(a.IP)
	if err != nil {
		return nil, err
	}
	// One year is the ceiling: a permanent block belongs in the policy, not
	// in a set whose contents vanish on reboot.
	if a.Seconds < 1 || a.Seconds > 366*24*3600 {
		return nil, fmt.Errorf("%w: seconds must be between 1 and one year", errBadArgument)
	}
	elem := fmt.Sprintf("{ %s timeout %ds }", ip, a.Seconds)
	// Re-blocking an address already in the set fails with EEXIST unless it
	// is removed first; a longer block must replace a shorter one.
	_, _ = s.Exec.Run(ctx, "nft", []string{"delete", "element", "inet", TableName, set, "{ " + ip + " }"}, "")
	if _, err := s.Exec.Run(ctx, "nft", []string{"add", "element", "inet", TableName, set, elem}, ""); err != nil {
		return nil, err
	}
	s.Log.Printf("blocked %s for %ds: %s", ip, a.Seconds, a.Reason)
	return map[string]any{"ok": true}, nil
}

func (s *Server) firewallUnblock(ctx context.Context, raw json.RawMessage) (any, error) {
	var a UnblockArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	set, ip, err := setFor(a.IP)
	if err != nil {
		return nil, err
	}
	if _, err := s.Exec.Run(ctx, "nft", []string{"delete", "element", "inet", TableName, set, "{ " + ip + " }"}, ""); err != nil {
		if strings.Contains(err.Error(), "No such file or directory") {
			return map[string]any{"ok": true, "absent": true}, nil
		}
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) firewallStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return nil, err
	}
	st := FirewallStatus{Blocked: []Blocked{}}
	for _, set := range []string{"blocked4", "blocked6"} {
		out, err := s.Exec.Run(ctx, "nft", []string{"-j", "list", "set", "inet", TableName, set}, "")
		if err != nil {
			// Before the first apply the table does not exist, and that is an
			// empty block list, not a failure.
			if strings.Contains(err.Error(), "No such file or directory") {
				continue
			}
			return nil, err
		}
		entries, err := ParseSetElements(out)
		if err != nil {
			return nil, err
		}
		st.Blocked = append(st.Blocked, entries...)
	}
	return st, nil
}

// ParseSetElements reads `nft -j list set` output. An element with a
// timeout is an object {"elem": {"val", "timeout", "expires"}}; one without
// is the bare value. Both shapes appear in the same list.
func ParseSetElements(text string) ([]Blocked, error) {
	var doc struct {
		Nftables []struct {
			Set *struct {
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse nft output: %w", err)
	}
	var out []Blocked
	for _, item := range doc.Nftables {
		if item.Set == nil {
			continue
		}
		for _, raw := range item.Set.Elem {
			var plain string
			if err := json.Unmarshal(raw, &plain); err == nil {
				out = append(out, Blocked{IP: plain})
				continue
			}
			var timed struct {
				Elem struct {
					Val     string `json:"val"`
					Expires int64  `json:"expires"`
				} `json:"elem"`
			}
			if err := json.Unmarshal(raw, &timed); err != nil {
				return nil, fmt.Errorf("parse nft element %s: %w", raw, err)
			}
			out = append(out, Blocked{IP: timed.Elem.Val, Expires: time.Duration(timed.Elem.Expires) * time.Second})
		}
	}
	return out, nil
}

func (s *Server) confPath(iface string) (string, error) {
	if !wireguard.InterfaceName.MatchString(iface) {
		return "", fmt.Errorf("%w: interface %q must match %s", errBadArgument, iface, wireguard.InterfaceName)
	}
	return filepath.Join(s.WireguardDir, iface+".conf"), nil
}

func (s *Server) interfaceUp(iface string) bool {
	return fsx.Exists(filepath.Join(s.SysNetDir, iface))
}

func (s *Server) vpnApply(ctx context.Context, raw json.RawMessage) (any, error) {
	var a VPNApplyArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	text, err := wireguard.RenderServer(a.Config)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadArgument, err)
	}
	path, err := s.confPath(a.Config.Interface)
	if err != nil {
		return nil, err
	}
	// 0600: the file holds the server's private key.
	if err := fsx.WriteAtomic(path, []byte(text), 0o600); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	result := map[string]any{"ok": true, "path": path, "synced": false}
	if !s.interfaceUp(a.Config.Interface) {
		return result, nil
	}
	// wg syncconf refuses wg-quick's own keys (Address, DNS…), so the file
	// goes through `wg-quick strip` first. Passing the path instead of the
	// name lets the directory be anything, which is what the tests need.
	stripped, err := s.Exec.Run(ctx, "wg-quick", []string{"strip", path}, "")
	if err != nil {
		return nil, err
	}
	if _, err := s.Exec.Run(ctx, "wg", []string{"syncconf", a.Config.Interface, "/dev/stdin"}, stripped); err != nil {
		return nil, err
	}
	result["synced"] = true
	return result, nil
}

func (s *Server) vpnUp(ctx context.Context, raw json.RawMessage) (any, error) {
	var a InterfaceArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	path, err := s.confPath(a.Interface)
	if err != nil {
		return nil, err
	}
	if !fsx.Exists(path) {
		return nil, fmt.Errorf("%s does not exist; apply a config first", path)
	}
	// wg-quick up on an interface that exists fails; the operator asked
	// for "up" and it is, so that is success.
	if s.interfaceUp(a.Interface) {
		return map[string]any{"ok": true, "already": true}, nil
	}
	if _, err := s.Exec.Run(ctx, "wg-quick", []string{"up", path}, ""); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) vpnDown(ctx context.Context, raw json.RawMessage) (any, error) {
	var a InterfaceArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	path, err := s.confPath(a.Interface)
	if err != nil {
		return nil, err
	}
	if !s.interfaceUp(a.Interface) {
		return map[string]any{"ok": true, "already": true}, nil
	}
	if _, err := s.Exec.Run(ctx, "wg-quick", []string{"down", path}, ""); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) vpnStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var a InterfaceArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if _, err := s.confPath(a.Interface); err != nil {
		return nil, err
	}
	st := VPNStatus{Checked: time.Now(), Peers: []wireguard.PeerStatus{}}
	if !s.interfaceUp(a.Interface) {
		return st, nil
	}
	out, err := s.Exec.Run(ctx, "wg", []string{"show", a.Interface, "dump"}, "")
	if err != nil {
		return nil, err
	}
	d, err := wireguard.ParseDump(out)
	if err != nil {
		return nil, err
	}
	st.Up = true
	st.PublicKey = d.PublicKey
	st.ListenPort = d.ListenPort
	st.Peers = append(st.Peers, d.Peers...)
	return st, nil
}

func (s *Server) sysctlReservePorts(ctx context.Context, raw json.RawMessage) (any, error) {
	var a ReservePortsArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if a.From < 1024 || a.To > 65535 || a.From > a.To {
		return nil, fmt.Errorf("%w: range %d-%d must sit inside 1024-65535", errBadArgument, a.From, a.To)
	}
	path := filepath.Join(s.SysctlDir, ReservedPortsFile)
	content := fmt.Sprintf("# Managed by middleware: keep the site port range out of the ephemeral range.\nnet.ipv4.ip_local_reserved_ports = %d-%d\n", a.From, a.To)
	if err := fsx.WriteAtomic(path, []byte(content), 0o644); err != nil {
		return nil, err
	}
	if _, err := s.Exec.Run(ctx, "sysctl", []string{"-p", path}, ""); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "path": path}, nil
}

// ErrUnavailable is what the client returns when the helper socket cannot
// be reached: the installation runs without the helper service.
var ErrUnavailable = errors.New("helper: not available")

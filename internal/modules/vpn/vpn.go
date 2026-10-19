// Package vpn is the WireGuard module: a private door to the panel and the
// service port. The module keeps the server key (encrypted) and the list of
// peers (public keys and addresses only), renders the server config and
// hands it to the helper, which is the only thing that touches /etc or the
// interface. A client's private key exists in memory for the length of one
// request: it is rendered into the config the operator copies and is never
// written anywhere.
package vpn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/helper"
	"github.com/dcimorra/middleware-public/internal/settings"
	"github.com/dcimorra/middleware-public/internal/wireguard"
)

// Key is the module's identifier in the registry and in the API path.
const Key = "vpn"

// DefaultInterface is the WireGuard interface the module manages.
const DefaultInterface = "mw0"

// DefaultListenPort is WireGuard's conventional port.
const DefaultListenPort = 51820

// Need is one value the module asks the operator for.
type Need struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Secret  bool   `json:"secret"`
	Default string `json:"default,omitempty"`
}

// Line is one result of a verification or installation step.
type Line struct {
	Status string `json:"status"` // "ok", "skip" or "fail"
	Text   string `json:"text"`
}

// Module is the shape the registry expects. It is declared here so this
// package compiles on its own; the real modules package defines the same
// method set and the lead wires the two together.
type Module interface {
	Key() string
	Name() string
	Description() string
	Needs() []Need
	Verify(ctx context.Context, config map[string]string) []Line
	Install(ctx context.Context, config map[string]string) []Line
	Enable(ctx context.Context, config map[string]string) ([]Line, error)
	Disable(ctx context.Context) error
	Routes() map[string]http.Handler
}

// Secrets encrypts what must not sit in clear on disk. The modules package
// provides the AES-GCM implementation keyed from MIDDLEWARE_SECRET_KEY.
type Secrets interface {
	Encrypt(plain []byte) (string, error)
	Decrypt(stored string) ([]byte, error)
}

// Options configure the module.
type Options struct {
	// Root is the installation root; state lives in data/vpn/.
	Root string
	// Helper is the client for the privileged service.
	Helper *helper.Client
	// Secrets guards the server's private key and the preshared keys.
	Secrets Secrets
	// Bus receives an event for every change.
	Bus *events.Bus
	// Interface name; DefaultInterface when empty.
	Interface string
	// PanelPorts are what vpn_only restricts; when empty, the port of the
	// loopback listener in settings.json.
	PanelPorts []int
	// LookPath finds binaries; exec.LookPath when nil.
	LookPath func(string) (string, error)
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// state is data/vpn/state.json: everything the module decided.
type state struct {
	Enabled    bool          `json:"enabled"`
	Interface  string        `json:"interface"`
	Endpoint   string        `json:"endpoint"`
	ListenPort int           `json:"listen_port"`
	Subnet     netip.Prefix  `json:"subnet"`
	PublicKey  wireguard.Key `json:"public_key"`
	// PrivateKey is the encrypted form; the clear key only exists while a
	// config is being rendered.
	PrivateKey string `json:"private_key"`
	VPNOnly    bool   `json:"vpn_only"`
}

// peer is one entry of data/vpn/peers.json. There is no private key field
// on purpose: the struct cannot hold what must never be stored.
type peer struct {
	Name      string        `json:"name"`
	PublicKey wireguard.Key `json:"public_key"`
	// PresharedKey is encrypted: the server file needs it on every apply,
	// so it has to be kept, but not in clear next to the public keys.
	PresharedKey string     `json:"preshared_key"`
	Address      netip.Addr `json:"address"`
	Created      time.Time  `json:"created"`
}

// VPN is the module.
type VPN struct {
	opts  Options
	dir   string
	mu    sync.Mutex
	state state
	peers []peer
}

// New loads the module's state from disk. A missing state is a module that
// was never enabled, not an error.
func New(o Options) (*VPN, error) {
	if o.Root == "" {
		return nil, errors.New("vpn: root is required")
	}
	if o.Secrets == nil {
		return nil, errors.New("vpn: secrets are required")
	}
	if o.Helper == nil {
		o.Helper = helper.Dial("")
	}
	if o.Interface == "" {
		o.Interface = DefaultInterface
	}
	if !wireguard.InterfaceName.MatchString(o.Interface) {
		return nil, fmt.Errorf("vpn: interface %q must match %s", o.Interface, wireguard.InterfaceName)
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if len(o.PanelPorts) == 0 {
		s, err := settings.Load(o.Root)
		if err != nil {
			return nil, err
		}
		if _, p, err := net.SplitHostPort(s.Listen); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				o.PanelPorts = []int{n}
			}
		}
	}
	v := &VPN{opts: o, dir: filepath.Join(o.Root, "data", "vpn")}
	if err := v.load(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *VPN) load() error {
	if err := fsx.ReadJSON(filepath.Join(v.dir, "state.json"), &v.state); err != nil && !fsx.IsNotExist(err) {
		return err
	}
	if err := fsx.ReadJSON(filepath.Join(v.dir, "peers.json"), &v.peers); err != nil && !fsx.IsNotExist(err) {
		return err
	}
	if v.state.Interface == "" {
		v.state.Interface = v.opts.Interface
	}
	return nil
}

// save writes both files. state.json is 0600 because, although the private
// key inside is encrypted, nothing in it is for anyone but the service.
func (v *VPN) save() error {
	if err := v.ensureDir(); err != nil {
		return err
	}
	if err := fsx.WriteJSON(filepath.Join(v.dir, "state.json"), v.state, 0o600); err != nil {
		return err
	}
	if v.peers == nil {
		v.peers = []peer{}
	}
	return fsx.WriteJSON(filepath.Join(v.dir, "peers.json"), v.peers, 0o600)
}

func (v *VPN) emit(typ, subject string, level string, msg string, data map[string]any) {
	if v.opts.Bus == nil {
		return
	}
	v.opts.Bus.Emit(events.Event{Type: typ, Subject: subject, Actor: "operator", Level: level, Message: msg, Data: data})
}

// Key implements Module.
func (v *VPN) Key() string { return Key }

// Name implements Module.
func (v *VPN) Name() string { return "VPN" }

// Description implements Module.
func (v *VPN) Description() string {
	return "A WireGuard door to the panel and the service port, with one config per device."
}

// Needs implements Module.
func (v *VPN) Needs() []Need {
	return []Need{
		{Key: "endpoint", Label: "Public endpoint clients connect to (host:port)"},
		{Key: "listen_port", Label: "UDP port WireGuard listens on", Default: strconv.Itoa(DefaultListenPort)},
		{Key: "subnet", Label: "Subnet for the VPN", Default: wireguard.DefaultSubnet.String()},
	}
}

// settingsOf parses the operator's answers, applying the defaults.
func settingsOf(config map[string]string) (endpoint string, port int, subnet netip.Prefix, err error) {
	endpoint = strings.TrimSpace(config["endpoint"])
	if !wireguard.ValidEndpoint(endpoint) {
		return "", 0, subnet, fmt.Errorf("endpoint %q must be host:port", endpoint)
	}
	port = DefaultListenPort
	if s := strings.TrimSpace(config["listen_port"]); s != "" {
		port, err = strconv.Atoi(s)
		if err != nil || port < 1 || port > 65535 {
			return "", 0, subnet, fmt.Errorf("listen_port %q must be between 1 and 65535", s)
		}
	}
	subnet = wireguard.DefaultSubnet
	if s := strings.TrimSpace(config["subnet"]); s != "" {
		subnet, err = netip.ParsePrefix(s)
		if err != nil || !subnet.Addr().Is4() || subnet.Bits() > 30 {
			return "", 0, subnet, fmt.Errorf("subnet %q must be an IPv4 prefix of /30 or wider", s)
		}
		subnet = subnet.Masked()
	}
	return endpoint, port, subnet, nil
}

// Verify implements Module: every line is something that was actually
// probed, and what cannot be probed without side effects says so.
func (v *VPN) Verify(ctx context.Context, config map[string]string) []Line {
	var lines []Line
	if _, _, _, err := settingsOf(config); err != nil {
		lines = append(lines, Line{"fail", err.Error()})
	} else {
		lines = append(lines, Line{"ok", "endpoint, port and subnet are well formed"})
	}
	for _, bin := range []string{"wg", "wg-quick"} {
		if _, err := v.opts.LookPath(bin); err != nil {
			lines = append(lines, Line{"fail", bin + " is not in PATH: install wireguard-tools"})
		} else {
			lines = append(lines, Line{"ok", bin + " is installed"})
		}
	}
	if v.opts.Helper.Available() {
		lines = append(lines, Line{"ok", "the helper answers on " + v.opts.Helper.Path()})
	} else {
		lines = append(lines, Line{"fail", "the helper is not reachable on " + v.opts.Helper.Path() + "; is middleware-helper running?"})
	}
	lines = append(lines, Line{"skip", "whether the endpoint reaches this machine is only known once a client connects"})
	return lines
}

// Install implements Module. The helper's operation list is closed and has
// no package manager in it on purpose, so the module cannot install
// wireguard-tools: it reports whether the installer did.
func (v *VPN) Install(ctx context.Context, config map[string]string) []Line {
	if _, err := v.opts.LookPath("wg"); err != nil {
		return []Line{{"fail", "wireguard-tools is not installed; the installer (or apt/dnf) must install it, the helper does not run package managers"}}
	}
	return []Line{{"ok", "wireguard-tools is installed"}}
}

func failed(lines []Line) bool {
	for _, l := range lines {
		if l.Status == "fail" {
			return true
		}
	}
	return false
}

// Enable implements Module: verify, keep or create the server key, write
// and apply the config, bring the interface up. Re-enabling keeps the key
// so every client config already handed out stays valid.
func (v *VPN) Enable(ctx context.Context, config map[string]string) ([]Line, error) {
	lines := v.Verify(ctx, config)
	if failed(lines) {
		return lines, errors.New("vpn: verification failed")
	}
	endpoint, port, subnet, err := settingsOf(config)
	if err != nil {
		return lines, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state.PrivateKey == "" {
		priv, err := wireguard.GeneratePrivateKey()
		if err != nil {
			return lines, err
		}
		pub, err := priv.Public()
		if err != nil {
			return lines, err
		}
		enc, err := v.opts.Secrets.Encrypt(priv[:])
		if err != nil {
			return lines, fmt.Errorf("vpn: encrypt private key: %w", err)
		}
		v.state.PrivateKey, v.state.PublicKey = enc, pub
		lines = append(lines, Line{"ok", "generated the server key " + pub.String()})
	} else {
		lines = append(lines, Line{"ok", "kept the server key " + v.state.PublicKey.String()})
	}
	if v.state.Subnet.IsValid() && v.state.Subnet != subnet && len(v.peers) > 0 {
		return lines, fmt.Errorf("vpn: the subnet cannot change while %d peers hold addresses in %s", len(v.peers), v.state.Subnet)
	}
	v.state.Enabled = true
	v.state.Interface = v.opts.Interface
	v.state.Endpoint, v.state.ListenPort, v.state.Subnet = endpoint, port, subnet
	if err := v.save(); err != nil {
		return lines, err
	}
	if err := v.applyLocked(ctx); err != nil {
		return append(lines, Line{"fail", err.Error()}), err
	}
	if err := v.opts.Helper.VPNUp(ctx, v.state.Interface); err != nil {
		return append(lines, Line{"fail", err.Error()}), err
	}
	lines = append(lines, Line{"ok", "interface " + v.state.Interface + " is up on udp/" + strconv.Itoa(port)})
	if v.state.VPNOnly {
		if err := v.applyFirewallLocked(ctx); err != nil {
			return append(lines, Line{"fail", err.Error()}), err
		}
	}
	v.emit("vpn.enabled", v.state.Interface, "", "", map[string]any{"endpoint": endpoint, "listen_port": port, "subnet": subnet.String()})
	return lines, nil
}

// Disable implements Module: interface down, credentials kept. A vpn_only
// restriction is lifted first, otherwise the panel would be reachable from
// nowhere.
func (v *VPN) Disable(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state.VPNOnly {
		v.state.VPNOnly = false
		if err := v.applyFirewallLocked(ctx); err != nil {
			v.state.VPNOnly = true
			return err
		}
		v.emit("vpn.only", v.state.Interface, "warn", "vpn-only access lifted because the VPN was disabled", map[string]any{"enabled": false})
	}
	if err := v.opts.Helper.VPNDown(ctx, v.state.Interface); err != nil {
		return err
	}
	v.state.Enabled = false
	if err := v.save(); err != nil {
		return err
	}
	v.emit("vpn.disabled", v.state.Interface, "", "", nil)
	return nil
}

// Enabled reports whether the module is on.
func (v *VPN) Enabled() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state.Enabled
}

// serverConfigLocked builds what the helper writes. The private key is
// decrypted here and lives only inside the request.
func (v *VPN) serverConfigLocked() (wireguard.ServerConfig, error) {
	raw, err := v.opts.Secrets.Decrypt(v.state.PrivateKey)
	if err != nil || len(raw) != wireguard.KeySize {
		return wireguard.ServerConfig{}, fmt.Errorf("vpn: decrypt private key: %v", err)
	}
	var priv wireguard.Key
	copy(priv[:], raw)
	cfg := wireguard.ServerConfig{
		Interface:  v.state.Interface,
		PrivateKey: priv,
		Address:    netip.PrefixFrom(wireguard.ServerAddress(v.state.Subnet), v.state.Subnet.Bits()),
		ListenPort: v.state.ListenPort,
	}
	for _, p := range v.peers {
		psk, err := v.opts.Secrets.Decrypt(p.PresharedKey)
		if err != nil || len(psk) != wireguard.KeySize {
			return cfg, fmt.Errorf("vpn: decrypt preshared key of %s: %v", p.Name, err)
		}
		var k wireguard.Key
		copy(k[:], psk)
		cfg.Peers = append(cfg.Peers, wireguard.Peer{
			Name: p.Name, PublicKey: p.PublicKey, PresharedKey: k,
			AllowedIPs: []netip.Prefix{netip.PrefixFrom(p.Address, 32)},
		})
	}
	return cfg, nil
}

func (v *VPN) applyLocked(ctx context.Context) error {
	cfg, err := v.serverConfigLocked()
	if err != nil {
		return err
	}
	if err := v.opts.Helper.VPNApply(ctx, helper.VPNApplyArgs{Config: cfg}); err != nil {
		v.emit("vpn.apply.failed", v.state.Interface, "error", err.Error(), nil)
		return err
	}
	return nil
}

func (v *VPN) policyLocked() helper.FirewallPolicy {
	return helper.FirewallPolicy{
		AllowPorts:   []int{v.state.ListenPort},
		VPNOnly:      v.state.VPNOnly,
		VPNInterface: v.state.Interface,
		PanelPorts:   v.opts.PanelPorts,
	}
}

func (v *VPN) applyFirewallLocked(ctx context.Context) error {
	if err := v.opts.Helper.FirewallApply(ctx, v.policyLocked()); err != nil {
		v.emit("firewall.apply.failed", v.state.Interface, "error", err.Error(), nil)
		return err
	}
	v.emit("firewall.applied", v.state.Interface, "", "", map[string]any{"vpn_only": v.state.VPNOnly, "panel_ports": v.opts.PanelPorts})
	return nil
}

// ErrNotEnabled says the VPN has to be enabled before peers can be added.
var ErrNotEnabled = errors.New("vpn: the module is not enabled")

// ErrPeerExists says the name is taken.
var ErrPeerExists = errors.New("vpn: a peer with that name exists")

// ErrPeerNotFound says the name is unknown.
var ErrPeerNotFound = errors.New("vpn: no peer with that name")

// AddPeer creates a client and returns its config text, the only time the
// client's private key is ever available.
func (v *VPN) AddPeer(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if !wireguard.PeerName.MatchString(name) {
		return "", fmt.Errorf("vpn: peer name %q must match %s", name, wireguard.PeerName)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.state.Enabled {
		return "", ErrNotEnabled
	}
	used := make([]netip.Addr, 0, len(v.peers))
	for _, p := range v.peers {
		if p.Name == name {
			return "", ErrPeerExists
		}
		used = append(used, p.Address)
	}
	addr, err := wireguard.NextAddress(v.state.Subnet, used)
	if err != nil {
		return "", err
	}
	priv, err := wireguard.GeneratePrivateKey()
	if err != nil {
		return "", err
	}
	pub, err := priv.Public()
	if err != nil {
		return "", err
	}
	psk, err := wireguard.GeneratePresharedKey()
	if err != nil {
		return "", err
	}
	encPSK, err := v.opts.Secrets.Encrypt(psk[:])
	if err != nil {
		return "", err
	}
	text, err := wireguard.RenderClient(wireguard.ClientConfig{
		PrivateKey:      priv,
		Address:         netip.PrefixFrom(addr, 32),
		ServerPublicKey: v.state.PublicKey,
		PresharedKey:    psk,
		Endpoint:        v.state.Endpoint,
		// Only the VPN subnet is routed: this is a door to the panel, not a
		// tunnel for all of the client's traffic.
		AllowedIPs:          []netip.Prefix{v.state.Subnet},
		PersistentKeepalive: 25,
	})
	if err != nil {
		return "", err
	}
	v.peers = append(v.peers, peer{Name: name, PublicKey: pub, PresharedKey: encPSK, Address: addr, Created: v.opts.Now()})
	if err := v.applyLocked(ctx); err != nil {
		v.peers = v.peers[:len(v.peers)-1]
		return "", err
	}
	if err := v.save(); err != nil {
		return "", err
	}
	v.emit("vpn.peer.added", name, "", "", map[string]any{"address": addr.String(), "public_key": pub.String()})
	return text, nil
}

// RemovePeer forgets a client; the kernel stops accepting it on the next
// handshake once the config is re-applied.
func (v *VPN) RemovePeer(ctx context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	idx := -1
	for i, p := range v.peers {
		if p.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		return ErrPeerNotFound
	}
	removed := v.peers[idx]
	v.peers = append(v.peers[:idx:idx], v.peers[idx+1:]...)
	if v.state.Enabled {
		if err := v.applyLocked(ctx); err != nil {
			v.peers = append(v.peers[:idx], append([]peer{removed}, v.peers[idx:]...)...)
			return err
		}
	}
	if err := v.save(); err != nil {
		return err
	}
	v.emit("vpn.peer.removed", name, "", "", map[string]any{"address": removed.Address.String()})
	return nil
}

// SetOnly restricts the panel ports to the VPN interface (or lifts the
// restriction). It refuses to lock the door when the VPN is down: the
// decision would survive the restart, the way in would not.
func (v *VPN) SetOnly(ctx context.Context, enabled bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if enabled && !v.state.Enabled {
		return ErrNotEnabled
	}
	if enabled && len(v.peers) == 0 {
		return errors.New("vpn: add a peer before making the panel vpn-only, or nobody can reach it")
	}
	was := v.state.VPNOnly
	v.state.VPNOnly = enabled
	if err := v.applyFirewallLocked(ctx); err != nil {
		v.state.VPNOnly = was
		return err
	}
	if err := v.save(); err != nil {
		return err
	}
	v.emit("vpn.only", v.state.Interface, "", "", map[string]any{"enabled": enabled})
	return nil
}

// PeerStatus is one client as the panel shows it.
type PeerStatus struct {
	Name          string    `json:"name"`
	Address       string    `json:"address"`
	PublicKey     string    `json:"public_key"`
	Created       time.Time `json:"created"`
	LastHandshake time.Time `json:"last_handshake,omitempty"`
	Online        bool      `json:"online"`
	Endpoint      string    `json:"endpoint,omitempty"`
	ReceiveBytes  int64     `json:"rx"`
	TransmitBytes int64     `json:"tx"`
}

// Status is GET /modules/vpn.
type Status struct {
	Enabled    bool         `json:"enabled"`
	Up         bool         `json:"up"`
	Interface  string       `json:"interface"`
	Address    string       `json:"address,omitempty"`
	Endpoint   string       `json:"endpoint,omitempty"`
	ListenPort int          `json:"listen_port,omitempty"`
	PublicKey  string       `json:"public_key,omitempty"`
	VPNOnly    bool         `json:"vpn_only"`
	Helper     bool         `json:"helper"`
	Peers      []PeerStatus `json:"peers"`
	Error      string       `json:"error,omitempty"`
}

// Status merges what is stored with what the kernel reports. A helper that
// cannot be reached is reported in the result, not as an error: the panel
// must still show the peers.
func (v *VPN) Status(ctx context.Context) Status {
	v.mu.Lock()
	defer v.mu.Unlock()
	st := Status{
		Enabled: v.state.Enabled, Interface: v.state.Interface, Endpoint: v.state.Endpoint,
		ListenPort: v.state.ListenPort, VPNOnly: v.state.VPNOnly, Peers: []PeerStatus{},
	}
	if v.state.Subnet.IsValid() {
		st.Address = netip.PrefixFrom(wireguard.ServerAddress(v.state.Subnet), v.state.Subnet.Bits()).String()
	}
	if !v.state.PublicKey.IsZero() {
		st.PublicKey = v.state.PublicKey.String()
	}
	live := map[wireguard.Key]wireguard.PeerStatus{}
	if v.state.Enabled {
		ks, err := v.opts.Helper.VPNStatus(ctx, v.state.Interface)
		if err != nil {
			st.Error = err.Error()
		} else {
			st.Helper, st.Up = true, ks.Up
			for _, p := range ks.Peers {
				live[p.PublicKey] = p
			}
		}
	}
	now := v.opts.Now()
	for _, p := range v.peers {
		ps := PeerStatus{Name: p.Name, Address: p.Address.String(), PublicKey: p.PublicKey.String(), Created: p.Created}
		if l, ok := live[p.PublicKey]; ok {
			ps.LastHandshake, ps.Online, ps.Endpoint = l.LastHandshake, l.Online(now), l.Endpoint
			ps.ReceiveBytes, ps.TransmitBytes = l.ReceiveBytes, l.TransmitBytes
		}
		st.Peers = append(st.Peers, ps)
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].Name < st.Peers[j].Name })
	return st
}

// Dir is where the module keeps its files.
func (v *VPN) Dir() string { return v.dir }

// ensureDir exists so a fresh installation gets the folder with the right
// mode before the first state file lands in it.
func (v *VPN) ensureDir() error { return os.MkdirAll(v.dir, 0o700) }

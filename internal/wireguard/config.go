package wireguard

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"text/template"
)

// InterfaceName is what the kernel accepts (at most 15 characters), narrowed
// to lower-case letters and digits because the name ends up inside nft rules
// and file names, where a dash or a dot would need quoting.
var InterfaceName = regexp.MustCompile(`^[a-z0-9]{1,15}$`)

// PeerName is what the operator calls a client. It becomes a URL segment and
// a comment in the server file, hence the restriction.
var PeerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Peer is one client as the server sees it.
type Peer struct {
	Name         string         `json:"name"`
	PublicKey    Key            `json:"public_key"`
	PresharedKey Key            `json:"preshared_key,omitempty"`
	AllowedIPs   []netip.Prefix `json:"allowed_ips"`
	// Endpoint is only set for peers reachable on their own; the clients of
	// a server have none.
	Endpoint string `json:"endpoint,omitempty"`
	// PersistentKeepalive in seconds; zero disables it.
	PersistentKeepalive int `json:"persistent_keepalive,omitempty"`
}

// ServerConfig is what /etc/wireguard/<iface>.conf is rendered from.
type ServerConfig struct {
	Interface  string       `json:"interface"`
	PrivateKey Key          `json:"private_key"`
	Address    netip.Prefix `json:"address"`
	ListenPort int          `json:"listen_port"`
	Peers      []Peer       `json:"peers"`
}

// ClientConfig is what a client receives, once.
type ClientConfig struct {
	PrivateKey          Key
	Address             netip.Prefix
	DNS                 string
	ServerPublicKey     Key
	PresharedKey        Key
	Endpoint            string
	AllowedIPs          []netip.Prefix
	PersistentKeepalive int
}

// Validate rejects anything wg-quick would choke on before it reaches a file
// the helper writes as root. The helper trusts these checks, so they live
// here and not only in the panel.
func (c ServerConfig) Validate() error {
	if !InterfaceName.MatchString(c.Interface) {
		return fmt.Errorf("wireguard: interface %q must match %s", c.Interface, InterfaceName)
	}
	if c.PrivateKey.IsZero() {
		return errors.New("wireguard: private key is empty")
	}
	if !c.Address.IsValid() || !c.Address.Addr().Is4() {
		return errors.New("wireguard: address must be an IPv4 prefix like 10.77.0.1/24")
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("wireguard: listen port %d out of range", c.ListenPort)
	}
	seen := map[Key]bool{}
	for _, p := range c.Peers {
		if err := p.validate(); err != nil {
			return err
		}
		if seen[p.PublicKey] {
			return fmt.Errorf("wireguard: peer %q repeats a public key", p.Name)
		}
		seen[p.PublicKey] = true
	}
	return nil
}

func (p Peer) validate() error {
	if p.Name != "" && !PeerName.MatchString(p.Name) {
		return fmt.Errorf("wireguard: peer name %q must match %s", p.Name, PeerName)
	}
	if p.PublicKey.IsZero() {
		return fmt.Errorf("wireguard: peer %q has no public key", p.Name)
	}
	if len(p.AllowedIPs) == 0 {
		return fmt.Errorf("wireguard: peer %q has no allowed ips", p.Name)
	}
	for _, a := range p.AllowedIPs {
		if !a.IsValid() {
			return fmt.Errorf("wireguard: peer %q has an invalid allowed ip", p.Name)
		}
	}
	if p.Endpoint != "" && !ValidEndpoint(p.Endpoint) {
		return fmt.Errorf("wireguard: peer %q endpoint %q is not host:port", p.Name, p.Endpoint)
	}
	if p.PersistentKeepalive < 0 || p.PersistentKeepalive > 65535 {
		return fmt.Errorf("wireguard: peer %q keepalive out of range", p.Name)
	}
	return nil
}

// ValidEndpoint reports whether s is an IP address or a host name followed by
// a port. Only letters, digits, dots and dashes are allowed in the host so
// the value can go into a config file with no chance of smuggling a new line
// or a new key into it.
func ValidEndpoint(s string) bool {
	if _, err := netip.ParseAddrPort(s); err == nil {
		return true
	}
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return false
	}
	host, port := s[:i], s[i+1:]
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var funcs = template.FuncMap{
	"join": func(ps []netip.Prefix) string {
		parts := make([]string, len(ps))
		for i, p := range ps {
			parts[i] = p.String()
		}
		return strings.Join(parts, ", ")
	},
}

var serverTemplate = template.Must(template.New("server").Funcs(funcs).Parse(`# Managed by middleware. Edits are overwritten on the next apply.
[Interface]
PrivateKey = {{.PrivateKey}}
Address = {{.Address}}
ListenPort = {{.ListenPort}}
{{range .Peers}}
[Peer]
{{if .Name}}# {{.Name}}
{{end}}PublicKey = {{.PublicKey}}
{{if not .PresharedKey.IsZero}}PresharedKey = {{.PresharedKey}}
{{end}}AllowedIPs = {{join .AllowedIPs}}
{{if .Endpoint}}Endpoint = {{.Endpoint}}
{{end}}{{if .PersistentKeepalive}}PersistentKeepalive = {{.PersistentKeepalive}}
{{end}}{{end}}`))

var clientTemplate = template.Must(template.New("client").Funcs(funcs).Parse(`[Interface]
PrivateKey = {{.PrivateKey}}
Address = {{.Address}}
{{if .DNS}}DNS = {{.DNS}}
{{end}}
[Peer]
PublicKey = {{.ServerPublicKey}}
{{if not .PresharedKey.IsZero}}PresharedKey = {{.PresharedKey}}
{{end}}Endpoint = {{.Endpoint}}
AllowedIPs = {{join .AllowedIPs}}
{{if .PersistentKeepalive}}PersistentKeepalive = {{.PersistentKeepalive}}
{{end}}`))

// RenderServer produces the wg-quick file. It validates first so the
// template never sees a value nobody checked.
func RenderServer(c ServerConfig) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	if err := serverTemplate.Execute(&b, c); err != nil {
		return "", err
	}
	return b.String(), nil
}

// RenderClient produces the file a client imports.
func RenderClient(c ClientConfig) (string, error) {
	if c.PrivateKey.IsZero() || c.ServerPublicKey.IsZero() {
		return "", errors.New("wireguard: client config needs both keys")
	}
	if !c.Address.IsValid() {
		return "", errors.New("wireguard: client config needs an address")
	}
	if !ValidEndpoint(c.Endpoint) {
		return "", fmt.Errorf("wireguard: endpoint %q is not host:port", c.Endpoint)
	}
	if len(c.AllowedIPs) == 0 {
		return "", errors.New("wireguard: client config needs allowed ips")
	}
	var b strings.Builder
	if err := clientTemplate.Execute(&b, c); err != nil {
		return "", err
	}
	return b.String(), nil
}

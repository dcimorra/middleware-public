package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/inbound"
	"github.com/dcimorra/middleware-public/internal/manifest"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// setPanelDomain gives the panel its name and opens the ports door (80 and
// 443, with a certificate the house obtains for that name), so the first
// sign-in can be at https://<domain> and not through an SSH tunnel. The
// settings go through the running supervisor when there is one, because
// it reads its settings once at start; without one, into the file it will
// read. The door's file is read live either way.
func (e *Env) setPanelDomain(root, domain string) error {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if !manifest.ValidDomain(domain) {
		return fmt.Errorf("%q is not a domain name", domain)
	}
	// The installer starts the service just before this: give its socket
	// a moment, or the settings would go to a file the running process
	// read already and will not read again until it restarts.
	cl, err := e.Connect(root)
	for i := 0; err != nil && i < 20 && fileExists(filepath.Join(root, "data", "api.sock")); i++ {
		time.Sleep(500 * time.Millisecond)
		cl, err = e.Connect(root)
	}
	running := err == nil
	if running {
		var raw []byte
		if err := cl.Do("GET", "/api/v1/settings", nil, &raw); err != nil {
			return err
		}
		var cur map[string]any
		if err := json.Unmarshal(raw, &cur); err != nil {
			return err
		}
		cur["panel_domain"] = domain
		if err := cl.Do("PUT", "/api/v1/settings", cur, nil); err != nil {
			return err
		}
	} else {
		st, err := settings.Load(root)
		if err != nil {
			return err
		}
		st.PanelDomain = domain
		if err := settings.Save(root, st); err != nil {
			return err
		}
	}
	doors, err := inbound.LoadState(root)
	if err != nil {
		return err
	}
	if !doors.Ports.Open {
		doors.Ports.Open = true
		if err := inbound.SaveState(root, doors); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.Stdout, "Panel domain: %s; the ports door (80 and 443) is open.\n", domain)
	if !running && fileExists(filepath.Join(root, "data", "api.sock")) {
		fmt.Fprintln(e.Stdout, "The service did not answer: restart it (systemctl restart middleware) for the domain to apply.")
	}
	fmt.Fprintln(e.Stdout, dnsLine(domain))
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// dnsLine says whether the name already points at this machine, because
// the certificate cannot be obtained until it does, and a sign-in page
// that does not load says nothing about why.
func dnsLine(domain string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound || (err == nil && len(addrs) == 0) {
		return fmt.Sprintf("DNS: %s does not resolve yet. Point an A record (and AAAA, if this machine has IPv6) at this machine; the certificate follows on its own once it does.", domain)
	}
	if err != nil {
		return "DNS: could not check (" + err.Error() + ")."
	}
	mine := map[netip.Addr]bool{}
	if ifs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				mine[p.Addr().Unmap()] = true
			}
		}
	}
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && mine[ip.Unmap()] {
			return fmt.Sprintf("DNS: %s points here (%s).", domain, a)
		}
	}
	return fmt.Sprintf("DNS: %s points at %s, which is not an address of this machine. Behind a NAT or Cloudflare's proxy that can be right; otherwise point it here.", domain, strings.Join(addrs, ", "))
}

package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/inbound"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// DNSInfo is what a DNS provider knows about one name.
type DNSInfo struct {
	// DNS is the record as "A 1.2.3.4" or "CNAME x.cfargotunnel.com";
	// empty when there is none or nobody can tell.
	DNS string `json:"dns"`
	// TunnelRoute says an ingress rule exists for the name.
	TunnelRoute bool `json:"tunnel_route"`
}

// DNSProvider is what the cloudflare module offers the inbound section:
// the two extra columns of GET /inbound, and publishing a domain on a
// door. Nil, or Enabled false, leaves the columns empty and the move
// without a DNS step.
type DNSProvider interface {
	Enabled() bool
	// Describe fills a row per domain; names it knows nothing about are
	// left out. One call for all rows, because the provider lists zones
	// once and the panel asks for forty domains at a time.
	Describe(ctx context.Context, domains []string) (map[string]DNSInfo, error)
	// Publish points domain at this machine through door ("tunnel" or
	// "ports"); ip is the machine's public address, needed for ports.
	Publish(ctx context.Context, domain, door, ip string) error
}

// InboundOptions wire the inbound section.
type InboundOptions struct {
	Hook *inbound.Hook
	DNS  DNSProvider
}

// InboundRow is one domain of GET /inbound.
type InboundRow struct {
	inbound.DomainStatus
	DNS         string `json:"dns"`
	TunnelRoute string `json:"tunnel_route"`
}

// RegisterInbound adds the /inbound routes.
func (s *Server) RegisterInbound(o InboundOptions) {
	h := o.Hook
	p := Prefix + "/inbound"
	s.Handle("GET", p, "The doors and every domain's door, certificate and DNS", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st := h.Status()
		rows := make([]InboundRow, 0, len(st.Domains))
		names := make([]string, 0, len(st.Domains))
		for _, d := range st.Domains {
			rows = append(rows, InboundRow{DomainStatus: d})
			names = append(names, d.Domain)
		}
		var dnsErr string
		if o.DNS != nil && o.DNS.Enabled() && len(names) > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			info, err := o.DNS.Describe(ctx, names)
			if err != nil {
				// The doors are the answer; a DNS provider that is down
				// is a column that says so, not a 502 over the whole page.
				dnsErr = err.Error()
			}
			for i := range rows {
				if d, ok := info[rows[i].Domain]; ok {
					rows[i].DNS = d.DNS
					if d.TunnelRoute {
						rows[i].TunnelRoute = "yes"
					} else {
						rows[i].TunnelRoute = "no"
					}
				}
			}
		}
		out := map[string]any{"doors": st.Doors, "domains": rows}
		if dnsErr != "" {
			out["dns_error"] = dnsErr
		}
		return out, nil
	})
	s.Handle("POST", p+"/move", "Put domains on a door, obtaining certificates and publishing DNS as needed", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var body struct {
			Domains []string `json:"domains"`
			To      string   `json:"to"`
			IP      string   `json:"ip"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		door, err := inbound.ParseDoor(body.To)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		if len(body.Domains) == 0 {
			return nil, Errorf(http.StatusBadRequest, "domains: none given")
		}
		if body.IP != "" && net.ParseIP(body.IP) == nil {
			return nil, Errorf(http.StatusBadRequest, "ip: %q is not an address", body.IP)
		}
		// Obtaining a certificate takes the CA's time, not the client's:
		// the move continues if the panel's tab goes away.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		lines := h.Move(ctx, body.Domains, door)
		if o.DNS != nil && o.DNS.Enabled() {
			lines = append(lines, publishMoved(ctx, o.DNS, h.State(), body.Domains, door, body.IP)...)
		}
		ok := true
		for _, l := range lines {
			if l.Status == "fail" {
				ok = false
			}
		}
		return map[string]any{"ok": ok, "lines": lines}, nil
	})
	s.Handle("POST", p+"/doors/{door}", "Open or close a door", func(w http.ResponseWriter, r *http.Request) (any, error) {
		door, err := inbound.ParseDoor(r.PathValue("door"))
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		var body struct {
			Open bool `json:"open"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		if err := h.SetDoor(door, body.Open); err != nil {
			return nil, err
		}
		return h.Status().Doors, nil
	})
	s.Handle("GET", p+"/certificates", "The certificates on disk", func(w http.ResponseWriter, r *http.Request) (any, error) {
		list := h.Store.List()
		if list == nil {
			return []struct{}{}, nil
		}
		return list, nil
	})
	s.Handle("POST", p+"/certificates/{domain}/renew", "Renew a certificate now", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := h.Store.Renew(ctx, domain); err != nil {
			return nil, Errorf(http.StatusBadGateway, "%v", err)
		}
		return OK, nil
	})
}

// publishMoved writes DNS for the domains the move marked, and only
// those: a domain whose certificate failed must not be pointed at a door
// that cannot serve it. What was marked is read back from the state, not
// parsed out of the lines.
func publishMoved(ctx context.Context, dns DNSProvider, st inbound.State, domains []string, door inbound.Door, ip string) []inbound.Line {
	var out []inbound.Line
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if !manifest.ValidDomain(d) || st.DoorOf(d) != door {
			continue
		}
		if door == inbound.Ports && ip == "" {
			out = append(out, inbound.Line{Status: "skip", Text: d + ": DNS not written, the ports door needs the machine's public ip (send \"ip\" with the move)"})
			continue
		}
		if err := dns.Publish(ctx, d, string(door), ip); err != nil {
			out = append(out, inbound.Line{Status: "fail", Text: d + ": DNS not written: " + err.Error()})
			continue
		}
		out = append(out, inbound.Line{Status: "ok", Text: d + ": DNS points at the " + string(door) + " door"})
	}
	return out
}

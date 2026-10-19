package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/dcimorra/middleware-public/internal/acme"
	"github.com/dcimorra/middleware-public/internal/app"
	"github.com/dcimorra/middleware-public/internal/certs"
	"github.com/dcimorra/middleware-public/internal/inbound"
	"github.com/dcimorra/middleware-public/internal/settings"
)

// inboundHook is the doors of the running process, for the API routes.
var inboundHook *inbound.Hook

func init() {
	hookBuilders = append(hookBuilders, hookBuilder{Name: "inbound", Order: orderInbound, Build: buildInbound})
	subcommands["inbound"] = inboundCmd
	extraUsage += `  inbound      status | open <tunnel|ports> | close <tunnel|ports>
`
}

// buildInbound wires the doors into the app. Certificates are only ever
// requested for names the engine serves or the panel's own, so the CA's
// quota is spent on this installation and nobody else.
func buildInbound(a *app.App) app.Hook {
	certDir := filepath.Join(a.Root, "data", "certs")
	solver := acme.NewHTTP01()
	alpn := acme.NewTLSALPN01()
	st, err := inbound.LoadState(a.Root)
	if err != nil {
		// A broken file must not take the doors down with it: the hook
		// keeps re-reading and the defaults apply until it parses.
		fmt.Fprintln(os.Stderr, "middleware: inbound:", err)
		st = inbound.DefaultState()
	}
	store := certs.New(certDir, &certs.ACMEIssuer{
		Dir:       certDir,
		Directory: st.ACME.Directory,
		Email:     st.ACME.Email,
		HTTP01:    solver,
		TLSALPN:   alpn,
		UserAgent: "middleware/" + Version,
	})
	store.ALPN = alpn
	store.Bus = a.Bus
	panel := a.Settings.PanelDomain
	store.Allowed = func(domain string) bool {
		if panel != "" && domain == panel {
			return true
		}
		_, ok := a.Engine.Lookup(domain)
		return ok
	}
	inboundHook = inbound.New(a.Root, store, solver)
	return inboundHook
}

// inboundCmd works on the state file alone, so it needs no running
// process and no credentials: whoever can write data/ owns the doors.
// The running process notices the file's new mtime on its next request.
func inboundCmd(args []string) error {
	fs := flag.NewFlagSet("inbound", flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: middleware inbound status | open <door> | close <door>")
	}
	dir := settings.Root(*root)
	switch rest[0] {
	case "status":
		return inboundStatus(dir)
	case "open", "close":
		if len(rest) != 2 {
			return fmt.Errorf("usage: middleware inbound %s <tunnel|ports>", rest[0])
		}
		door, err := inbound.ParseDoor(rest[1])
		if err != nil {
			return err
		}
		st, err := inbound.SetDoor(dir, door, rest[0] == "open")
		if err != nil {
			return err
		}
		state := "closed"
		if (door == inbound.Tunnel && st.Tunnel.Open) || (door == inbound.Ports && st.Ports.Open) {
			state = "open"
		}
		fmt.Printf("%s door %s (%s)\n", door, state, inbound.Path(dir))
		return nil
	}
	return fmt.Errorf("unknown inbound command %q", rest[0])
}

func inboundStatus(dir string) error {
	st, err := inbound.LoadState(dir)
	if err != nil {
		return err
	}
	onOff := func(b bool) string {
		if b {
			return "open"
		}
		return "closed"
	}
	fmt.Printf("tunnel  %s\n", onOff(st.Tunnel.Open))
	fmt.Printf("ports   %s  http %s  https %s\n", onOff(st.Ports.Open), st.Ports.HTTP, st.Ports.HTTPS)
	if st.ACME.Directory != "" {
		fmt.Printf("acme    %s\n", st.ACME.Directory)
	}
	store := certs.New(filepath.Join(dir, "data", "certs"), nil)
	rows := map[string]inbound.DomainStatus{}
	for d, door := range st.Domains {
		rows[d] = inbound.DomainStatus{Domain: d, Door: door}
	}
	for _, info := range store.List() {
		row := rows[info.Domain]
		row.Domain = info.Domain
		exp := info.Expires
		row.CertExpires = &exp
		if row.Door == "" {
			row.Door = inbound.Ports
		}
		rows[info.Domain] = row
	}
	if len(rows) == 0 {
		return nil
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DOMAIN\tDOOR\tCERTIFICATE")
	names := make([]string, 0, len(rows))
	for d := range rows {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		row := rows[d]
		cert := "-"
		if row.CertExpires != nil {
			left := time.Until(*row.CertExpires).Round(24*time.Hour) / (24 * time.Hour)
			cert = fmt.Sprintf("expires %s (%dd)", row.CertExpires.Format("2006-01-02"), left)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", row.Domain, row.Door, cert)
	}
	return w.Flush()
}

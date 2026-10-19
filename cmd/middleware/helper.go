package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dcimorra/middleware-public/internal/events"
	"github.com/dcimorra/middleware-public/internal/helper"
	"github.com/dcimorra/middleware-public/internal/modules/vpn"
	"github.com/dcimorra/middleware-public/internal/settings"
)

func init() {
	subcommands["helper"] = helperCommand
	subcommands["vpn"] = vpnCommand
	extraUsage += `  helper       run the privileged helper (what middleware-helper.service runs)
               flags: --socket, --allow-uid, --allow-gid, --socket-group
  helper ping  check the helper answers
  vpn status   the interface and its peers
  vpn peer add <name>      create a client and print its config (shown once)
  vpn peer remove <name>   forget a client
`
}

// helperCommand runs the root-side service or pings it.
func helperCommand(args []string) error {
	if len(args) > 0 && args[0] == "ping" {
		fs := flag.NewFlagSet("helper ping", flag.ContinueOnError)
		socket := fs.String("socket", helper.DefaultSocket, "helper socket")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := helper.Dial(*socket).Ping(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("helper ok: protocol %s build %s\n", r.Version, r.Build)
		return nil
	}
	fs := flag.NewFlagSet("helper", flag.ContinueOnError)
	socket := fs.String("socket", helper.DefaultSocket, "unix socket to listen on")
	allowUID := fs.String("allow-uid", "", "uid (or user name) allowed to call; root always is")
	allowGID := fs.String("allow-gid", "middleware", "gid (or group name) allowed to call")
	socketGroup := fs.String("socket-group", "", "group the socket file is chowned to (default: --allow-gid)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	uid, err := lookupID(*allowUID, func(s string) (string, error) {
		u, err := user.Lookup(s)
		if err != nil {
			return "", err
		}
		return u.Uid, nil
	})
	if err != nil {
		return fmt.Errorf("--allow-uid: %w", err)
	}
	gid, err := lookupID(*allowGID, func(s string) (string, error) {
		g, err := user.LookupGroup(s)
		if err != nil {
			return "", err
		}
		return g.Gid, nil
	})
	if err != nil {
		return fmt.Errorf("--allow-gid: %w", err)
	}
	if *socketGroup == "" {
		*socketGroup = *allowGID
	}
	sg, err := lookupID(*socketGroup, func(s string) (string, error) {
		g, err := user.LookupGroup(s)
		if err != nil {
			return "", err
		}
		return g.Gid, nil
	})
	if err != nil {
		return fmt.Errorf("--socket-group: %w", err)
	}
	if uid < 0 && gid < 0 {
		return errors.New("nothing would be allowed to call the helper: set --allow-uid or --allow-gid")
	}
	srv := &helper.Server{Socket: *socket, AllowUID: uid, AllowGID: gid, SocketGroup: sg, Build: Version}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ln, err := srv.Listen()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "helper: listening on %s (uid %d, gid %d)\n", *socket, uid, gid)
	return srv.Serve(ctx, ln)
}

// parseInterleaved lets flags follow the verbs (`vpn peer add x --socket
// …`), which the flag package refuses on its own: it stops at the first
// word without a dash and leaves the flags in Args.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// lookupID accepts a number or a name; empty means -1 (disabled).
func lookupID(s string, byName func(string) (string, error)) (int, error) {
	if s == "" {
		return -1, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	id, err := byName(s)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(id)
}

// vpnCommand manages peers from the terminal. It opens the same files the
// running service uses, so a peer added here shows in the panel after its
// next read; the service re-reads nothing by itself, which is why the panel
// route is the preferred way and this one is for recovery (the panel is
// what the VPN protects, after all).
func vpnCommand(args []string) error {
	fs := flag.NewFlagSet("vpn", flag.ContinueOnError)
	root := fs.String("root", "", "installation root (default $MIDDLEWARE_ROOT or /srv/middleware)")
	socket := fs.String("socket", helper.DefaultSocket, "helper socket")
	asJSON := fs.Bool("json", false, "print status as JSON")
	args, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: middleware vpn status | peer add <name> | peer remove <name>")
	}
	r := settings.Root(*root)
	secrets, err := vpn.NewFileSecrets(r)
	if err != nil {
		return err
	}
	bus := events.New("")
	m, err := vpn.New(vpn.Options{Root: r, Helper: helper.Dial(*socket), Secrets: secrets, Bus: bus})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch {
	case args[0] == "status":
		st := m.Status(ctx)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(st)
		}
		fmt.Printf("interface %s  enabled=%v up=%v vpn-only=%v\n", st.Interface, st.Enabled, st.Up, st.VPNOnly)
		if st.Enabled {
			fmt.Printf("endpoint %s  address %s  listen udp/%d\npublic key %s\n", st.Endpoint, st.Address, st.ListenPort, st.PublicKey)
		}
		if st.Error != "" {
			fmt.Printf("helper: %s\n", st.Error)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "PEER\tADDRESS\tLAST HANDSHAKE\tENDPOINT")
		for _, p := range st.Peers {
			hs := "never"
			if !p.LastHandshake.IsZero() {
				hs = time.Since(p.LastHandshake).Round(time.Second).String() + " ago"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, p.Address, hs, p.Endpoint)
		}
		return tw.Flush()
	case args[0] == "peer" && len(args) == 3 && args[1] == "add":
		text, err := m.AddPeer(ctx, args[2])
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "client config for", args[2], "(shown once, not stored):")
		fmt.Print(text)
		return nil
	case args[0] == "peer" && len(args) == 3 && args[1] == "remove":
		if err := m.RemovePeer(ctx, args[2]); err != nil {
			return err
		}
		fmt.Println("removed", args[2])
		return nil
	}
	return errors.New("usage: middleware vpn status | peer add <name> | peer remove <name>")
}

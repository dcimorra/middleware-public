// Package cloudflare is the subset of the Cloudflare API this house uses:
// zones, DNS records and the ingress rules of a tunnel. It is written on
// net/http because a module may install a binary but never an import, and
// the subset is small: five endpoints, token auth, JSON in and out.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DefaultBaseURL is the public API.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// Client talks to the API with one token. AccountID and TunnelID name the
// tunnel the ingress calls operate on; the module fills them from the
// store, discovering them once when the operator left them empty.
type Client struct {
	Token     string
	AccountID string
	TunnelID  string
	// Service is what a tunnel route points at: the loopback listener.
	Service string
	// ProxyPorts says whether A records written for the ports door go
	// through Cloudflare's proxy. Off by default: a site that obtains its
	// own certificates on 80/443 wants visitors to reach it, and behind the
	// proxy the certificate the visitor sees is Cloudflare's anyway.
	ProxyPorts bool
	BaseURL    string
	HTTP       *http.Client
}

// Zone is a DNS zone.
type Zone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Account struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"account"`
}

// Record is a DNS record.
type Record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"`
}

// Account is a Cloudflare account.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Tunnel is a cloudflared tunnel.
type Tunnel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"account_tag"`
}

// Ingress is one rule of a remotely managed tunnel. A rule without a
// hostname is the catch-all and must be last.
type Ingress struct {
	Hostname string `json:"hostname,omitempty"`
	Path     string `json:"path,omitempty"`
	Service  string `json:"service"`
}

// CatchAll is the rule a tunnel configuration must end with.
const CatchAll = "http_status:404"

// APIError is a non-success answer, with the API's own messages.
type APIError struct {
	Status   int
	Messages []string
}

func (e *APIError) Error() string {
	if len(e.Messages) == 0 {
		return fmt.Sprintf("cloudflare: http %d", e.Status)
	}
	return "cloudflare: " + strings.Join(e.Messages, "; ")
}

// IsForbidden reports whether err is the API saying the token lacks a
// permission, so verify lines can say "cannot" instead of "error".
func IsForbidden(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized)
}

type envelope struct {
	Success  bool            `json:"success"`
	Errors   []apiMessage    `json:"errors"`
	Messages []apiMessage    `json:"messages"`
	Result   json.RawMessage `json:"result"`
	Info     *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// do performs one call and decodes result into out (may be nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) (*envelope, error) {
	if c.Token == "" {
		return nil, errors.New("cloudflare: no token")
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, &APIError{Status: resp.StatusCode, Messages: []string{fmt.Sprintf("%s %s: not a JSON answer (%d)", method, path, resp.StatusCode)}}
	}
	if resp.StatusCode >= 400 || !env.Success {
		e := &APIError{Status: resp.StatusCode}
		for _, m := range env.Errors {
			e.Messages = append(e.Messages, fmt.Sprintf("%s (%d)", m.Message, m.Code))
		}
		return nil, e
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return nil, fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
		}
	}
	return &env, nil
}

// list fetches every page of a collection endpoint. The API caps per_page
// at 50 for most collections, and an account with sixty zones must not
// see ten of them missing from "can list zones".
func (c *Client) list(ctx context.Context, path string, query url.Values, out any) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", "50")
	var all []json.RawMessage
	for page := 1; page < 100; page++ {
		query.Set("page", fmt.Sprint(page))
		var items []json.RawMessage
		env, err := c.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &items)
		if err != nil {
			return err
		}
		all = append(all, items...)
		if env.Info == nil || env.Info.Page >= env.Info.TotalPages || len(items) == 0 {
			break
		}
	}
	data, _ := json.Marshal(all)
	return json.Unmarshal(data, out)
}

// TokenStatus is what Cloudflare's verify endpoint says: "active",
// "disabled", "expired". A token is owned by a user or by an account, and
// each kind is verified at its own address: an account token (cfat_…)
// answers "Invalid API Token" on /user/tokens/verify. Found against the
// real API on 20 October 2026; the fake had only the user endpoint.
func (c *Client) TokenStatus(ctx context.Context) (string, error) {
	var r struct {
		Status string `json:"status"`
	}
	_, err := c.do(ctx, http.MethodGet, "/user/tokens/verify", nil, &r)
	if err != nil && c.AccountID != "" {
		if _, aerr := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(c.AccountID)+"/tokens/verify", nil, &r); aerr == nil {
			return r.Status, nil
		}
	}
	if err != nil {
		return "", err
	}
	return r.Status, nil
}

// Zones lists every zone the token can see, by name.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	var zones []Zone
	if err := c.list(ctx, "/zones", nil, &zones); err != nil {
		return nil, err
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
	return zones, nil
}

// FindZone returns the zone that holds domain: the longest zone name that
// is domain itself or a suffix of it after a dot, so "api.shop.example.com"
// lands in "shop.example.com" when that zone exists and in "example.com"
// otherwise.
func (c *Client) FindZone(ctx context.Context, domain string) (Zone, error) {
	zones, err := c.Zones(ctx)
	if err != nil {
		return Zone{}, err
	}
	return MatchZone(zones, domain)
}

// MatchZone is FindZone over an already fetched list.
func MatchZone(zones []Zone, domain string) (Zone, error) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	var best Zone
	for _, z := range zones {
		name := strings.ToLower(z.Name)
		if domain == name || strings.HasSuffix(domain, "."+name) {
			if len(name) > len(best.Name) {
				best = z
			}
		}
	}
	if best.ID == "" {
		return Zone{}, fmt.Errorf("cloudflare: no zone holds %s", domain)
	}
	return best, nil
}

// DNSRecords lists a zone's records.
func (c *Client) DNSRecords(ctx context.Context, zoneID string) ([]Record, error) {
	var recs []Record
	if err := c.list(ctx, "/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// UpsertCNAME writes name → target, replacing whatever record of any
// address type sits on name.
func (c *Client) UpsertCNAME(ctx context.Context, zoneID, name, target string, proxied bool) (Record, error) {
	return c.upsert(ctx, zoneID, Record{Type: "CNAME", Name: name, Content: target, Proxied: proxied, TTL: 1})
}

// UpsertA writes name → ip the same way.
func (c *Client) UpsertA(ctx context.Context, zoneID, name, ip string, proxied bool) (Record, error) {
	return c.upsert(ctx, zoneID, Record{Type: "A", Name: name, Content: ip, Proxied: proxied, TTL: 1})
}

// upsert replaces in place. A name can carry an A or a CNAME, never both,
// so switching a domain between the doors must change the type of the
// record that is there: the API's PUT overwrites type and content in one
// call, which leaves no moment where the name resolves to nothing. Extra
// records on the same name (two A records from a round-robin) are deleted
// after, since the one left now answers.
func (c *Client) upsert(ctx context.Context, zoneID string, want Record) (Record, error) {
	existing, err := c.DNSRecords(ctx, zoneID)
	if err != nil {
		return Record{}, err
	}
	var same []Record
	for _, r := range existing {
		if !strings.EqualFold(r.Name, want.Name) {
			continue
		}
		switch r.Type {
		case "A", "AAAA", "CNAME":
			same = append(same, r)
		}
	}
	zone := "/zones/" + url.PathEscape(zoneID) + "/dns_records"
	var out Record
	if len(same) == 0 {
		if _, err := c.do(ctx, http.MethodPost, zone, want, &out); err != nil {
			return Record{}, err
		}
		return out, nil
	}
	// Prefer the record of the wanted type so a no-op stays a no-op.
	keep := same[0]
	for _, r := range same {
		if r.Type == want.Type {
			keep = r
			break
		}
	}
	if keep.Type == want.Type && keep.Content == want.Content && keep.Proxied == want.Proxied && len(same) == 1 {
		return keep, nil
	}
	if _, err := c.do(ctx, http.MethodPut, zone+"/"+url.PathEscape(keep.ID), want, &out); err != nil {
		return Record{}, err
	}
	for _, r := range same {
		if r.ID != keep.ID {
			if err := c.DeleteRecord(ctx, zoneID, r.ID); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// DeleteRecord removes one record.
func (c *Client) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), nil, nil)
	return err
}

// Accounts lists the accounts the token can see.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var accts []Account
	if err := c.list(ctx, "/accounts", nil, &accts); err != nil {
		return nil, err
	}
	return accts, nil
}

// Tunnels lists the live tunnels of c.AccountID.
func (c *Client) Tunnels(ctx context.Context) ([]Tunnel, error) {
	if c.AccountID == "" {
		return nil, errors.New("cloudflare: no account id")
	}
	var ts []Tunnel
	q := url.Values{"is_deleted": {"false"}}
	if err := c.list(ctx, "/accounts/"+url.PathEscape(c.AccountID)+"/cfd_tunnel", q, &ts); err != nil {
		return nil, err
	}
	return ts, nil
}

func (c *Client) tunnelPath(tunnelID string) (string, error) {
	if tunnelID == "" {
		tunnelID = c.TunnelID
	}
	if c.AccountID == "" || tunnelID == "" {
		return "", errors.New("cloudflare: no tunnel configured (account id and tunnel id are needed)")
	}
	return "/accounts/" + url.PathEscape(c.AccountID) + "/cfd_tunnel/" + url.PathEscape(tunnelID), nil
}

// TunnelToken returns the token cloudflared runs the tunnel with. It is a
// read of something that already exists, but what it returns is a secret.
func (c *Client) TunnelToken(ctx context.Context, tunnelID string) (string, error) {
	p, err := c.tunnelPath(tunnelID)
	if err != nil {
		return "", err
	}
	var token string
	if _, err := c.do(ctx, http.MethodGet, p+"/token", nil, &token); err != nil {
		return "", err
	}
	return token, nil
}

type tunnelConfig struct {
	Config struct {
		Ingress []Ingress `json:"ingress"`
	} `json:"config"`
}

// TunnelIngress returns the rules of a remotely managed tunnel. An empty
// tunnelID means c.TunnelID.
func (c *Client) TunnelIngress(ctx context.Context, tunnelID string) ([]Ingress, error) {
	p, err := c.tunnelPath(tunnelID)
	if err != nil {
		return nil, err
	}
	var cfg tunnelConfig
	if _, err := c.do(ctx, http.MethodGet, p+"/configurations", nil, &cfg); err != nil {
		return nil, err
	}
	return cfg.Config.Ingress, nil
}

// SetTunnelIngress replaces the rules, appending the catch-all when the
// caller forgot it: the API rejects a configuration without one, and the
// error it gives does not say so.
func (c *Client) SetTunnelIngress(ctx context.Context, rules []Ingress) error {
	p, err := c.tunnelPath("")
	if err != nil {
		return err
	}
	rules = withCatchAll(rules)
	var cfg tunnelConfig
	cfg.Config.Ingress = rules
	_, err = c.do(ctx, http.MethodPut, p+"/configurations", cfg, nil)
	return err
}

// AddIngress routes hostname to service, replacing an existing rule for
// the same hostname and keeping the catch-all last.
func (c *Client) AddIngress(ctx context.Context, hostname, service string) error {
	hostname = strings.ToLower(hostname)
	if hostname == "" || service == "" {
		return errors.New("cloudflare: hostname and service are required")
	}
	rules, err := c.TunnelIngress(ctx, "")
	if err != nil {
		return err
	}
	var out []Ingress
	replaced := false
	for _, r := range rules {
		if r.Hostname == "" {
			continue
		}
		if strings.EqualFold(r.Hostname, hostname) && r.Path == "" {
			if replaced {
				continue
			}
			r.Service = service
			replaced = true
		}
		out = append(out, r)
	}
	if !replaced {
		out = append(out, Ingress{Hostname: hostname, Service: service})
	}
	return c.SetTunnelIngress(ctx, out)
}

// RemoveIngress drops every rule for hostname. Removing what is not there
// is not an error: the goal is "no route", and it is met.
func (c *Client) RemoveIngress(ctx context.Context, hostname string) error {
	rules, err := c.TunnelIngress(ctx, "")
	if err != nil {
		return err
	}
	var out []Ingress
	found := false
	for _, r := range rules {
		if r.Hostname != "" && strings.EqualFold(r.Hostname, hostname) {
			found = true
			continue
		}
		if r.Hostname != "" {
			out = append(out, r)
		}
	}
	if !found {
		return nil
	}
	return c.SetTunnelIngress(ctx, out)
}

// HasIngress reports whether hostname has a rule.
func (c *Client) HasIngress(ctx context.Context, hostname string) (bool, error) {
	rules, err := c.TunnelIngress(ctx, "")
	if err != nil {
		return false, err
	}
	for _, r := range rules {
		if strings.EqualFold(r.Hostname, hostname) {
			return true, nil
		}
	}
	return false, nil
}

func withCatchAll(rules []Ingress) []Ingress {
	out := make([]Ingress, 0, len(rules)+1)
	for _, r := range rules {
		if r.Hostname != "" {
			out = append(out, r)
		}
	}
	return append(out, Ingress{Service: CatchAll})
}

// TunnelTarget is the CNAME target of a tunnel.
func (c *Client) TunnelTarget() string { return c.TunnelID + ".cfargotunnel.com" }

// Publish points domain at this machine through one of the two doors.
//
// Going to the tunnel: the ingress rule is written first and the CNAME
// after, so the moment DNS answers the tunnel already knows the hostname;
// the other order shows Cloudflare's own 404 page until the rule lands.
// Leaving the tunnel for the ports door: the A record first, then the
// ingress rule is removed, for the same reason in reverse: resolvers that
// still hold the CNAME keep working until it expires.
func (c *Client) Publish(ctx context.Context, domain, mode, ip string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return errors.New("cloudflare: no domain")
	}
	zone, err := c.FindZone(ctx, domain)
	if err != nil {
		return err
	}
	switch mode {
	case "tunnel":
		if c.TunnelID == "" {
			return errors.New("cloudflare: the tunnel door needs a tunnel id")
		}
		service := c.Service
		if service == "" {
			service = "http://127.0.0.1:46100"
		}
		if err := c.AddIngress(ctx, domain, service); err != nil {
			return err
		}
		_, err := c.UpsertCNAME(ctx, zone.ID, domain, c.TunnelTarget(), true)
		return err
	case "ports":
		if ip == "" {
			return errors.New("cloudflare: the ports door needs the machine's public ip")
		}
		if _, err := c.UpsertA(ctx, zone.ID, domain, ip, c.ProxyPorts); err != nil {
			return err
		}
		if c.TunnelID == "" {
			return nil
		}
		return c.RemoveIngress(ctx, domain)
	default:
		return fmt.Errorf("cloudflare: mode %q is not tunnel or ports", mode)
	}
}

// Unpublish undoes Publish for domain: the tunnel rule goes, and the A or
// CNAME record goes only when it points at this house (the tunnel's
// target, or ip). A record pointing anywhere else was written by someone
// else, and removing it would take down whatever it serves.
func (c *Client) Unpublish(ctx context.Context, domain, ip string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if c.TunnelID != "" {
		if err := c.RemoveIngress(ctx, domain); err != nil {
			return err
		}
	}
	zone, err := c.FindZone(ctx, domain)
	if err != nil {
		return err
	}
	recs, err := c.DNSRecords(ctx, zone.ID)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if !strings.EqualFold(r.Name, domain) {
			continue
		}
		ours := (r.Type == "CNAME" && c.TunnelID != "" && strings.EqualFold(r.Content, c.TunnelTarget())) ||
			(r.Type == "A" && ip != "" && r.Content == ip)
		if !ours {
			continue
		}
		if err := c.DeleteRecord(ctx, zone.ID, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// Covers reports whether domain is in a zone the token sees: a name in a
// zone elsewhere has to be pointed here by hand.
func (c *Client) Covers(ctx context.Context, domain string) (bool, error) {
	zones, err := c.Zones(ctx)
	if err != nil {
		return false, err
	}
	_, err = MatchZone(zones, domain)
	return err == nil, nil
}

// SetTXT writes a TXT record (an ACME dns-01 challenge) and returns how to
// remove exactly that record, leaving any other TXT on the name alone.
func (c *Client) SetTXT(ctx context.Context, name, value string) (func(context.Context) error, error) {
	zone, err := c.FindZone(ctx, name)
	if err != nil {
		return nil, err
	}
	var out Record
	if _, err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone.ID)+"/dns_records", Record{Type: "TXT", Name: name, Content: value, TTL: 60}, &out); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error { return c.DeleteRecord(ctx, zone.ID, out.ID) }, nil
}

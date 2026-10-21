package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// The edge's cache, a zone's records, the Access applications, the token
// itself and the domains the account owns: what V1's panel did in
// Cloudflare's own dashboard's place. Each is a few calls on the same
// client, and each was written against the API's documented shapes with a
// fake standing in; ROADMAP.md says which of them have not met the real
// API yet.

// PurgeHosts empties the edge's cache for every URL of these hosts.
func (c *Client) PurgeHosts(ctx context.Context, zoneID string, hosts []string) error {
	_, err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/purge_cache", map[string]any{"hosts": hosts}, nil)
	return err
}

// CacheRule is one rule of a zone's cache-settings phase.
type CacheRule struct {
	ID          string         `json:"id,omitempty"`
	Description string         `json:"description,omitempty"`
	Expression  string         `json:"expression"`
	Action      string         `json:"action,omitempty"`
	Params      map[string]any `json:"action_parameters,omitempty"`
	Enabled     *bool          `json:"enabled,omitempty"`
	// Scope is filled here, not by the API: "site" when the expression
	// names the domain asked about, "other" when it names another host,
	// "zone" when it names none. A cache rule belongs to the zone, not to
	// a site, and a zone holds many sites: the one difficult thing about
	// this screen is that deleting the neighbour's rule errors nowhere,
	// its site just starts serving differently.
	Scope string `json:"scope,omitempty"`
}

// CacheRules is a zone's cache phase.
type CacheRules struct {
	ZoneID    string      `json:"zone_id"`
	RulesetID string      `json:"ruleset_id,omitempty"`
	Rules     []CacheRule `json:"rules"`
}

const cachePhase = "/rulesets/phases/http_request_cache_settings/entrypoint"

// GetCacheRules reads the zone's cache rules and scopes them for domain.
// A zone with no ruleset yet has none (the API answers 404).
func (c *Client) GetCacheRules(ctx context.Context, zoneID, domain string) (CacheRules, error) {
	out := CacheRules{ZoneID: zoneID, Rules: []CacheRule{}}
	var rs struct {
		ID    string      `json:"id"`
		Rules []CacheRule `json:"rules"`
	}
	_, err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+cachePhase, nil, &rs)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.RulesetID = rs.ID
	for _, r := range rs.Rules {
		r.Scope = ScopeOf(r.Expression, domain)
		out.Rules = append(out.Rules, r)
	}
	return out, nil
}

// ScopeOf says whom an expression reaches, seen from domain.
func ScopeOf(expr, domain string) string {
	lower := strings.ToLower(expr)
	if !strings.Contains(lower, "http.host") {
		return "zone"
	}
	if strings.Contains(lower, `"`+strings.ToLower(domain)+`"`) {
		return "site"
	}
	return "other"
}

// AddCacheRule appends a rule for one host and path prefix: ttl seconds at
// the edge whatever the origin says, or, with ttl 0, bypass the cache.
// New rules are born scoped to the host, because the mistake that happens
// is writing a bare path and reaching every site in the zone. Rules are
// added one by one, never by rewriting the list: two additions at once
// must not overwrite each other. Only an empty list is written whole,
// when the zone has none yet.
func (c *Client) AddCacheRule(ctx context.Context, zoneID, host, path string, ttl int) error {
	cur, err := c.GetCacheRules(ctx, zoneID, host)
	if err != nil {
		return err
	}
	if cur.RulesetID == "" {
		var rs struct {
			ID string `json:"id"`
		}
		if _, err := c.do(ctx, http.MethodPut, "/zones/"+url.PathEscape(zoneID)+cachePhase, map[string]any{"rules": []any{}}, &rs); err != nil {
			return err
		}
		cur.RulesetID = rs.ID
	}
	expr := fmt.Sprintf(`(http.host eq "%s")`, host)
	if p := strings.TrimSuffix(path, "/"); p != "" {
		expr = fmt.Sprintf(`(http.host eq "%s" and starts_with(http.request.uri.path, "%s"))`, host, p)
	}
	params := map[string]any{"cache": false}
	desc := "middleware: bypass " + host + path
	if ttl > 0 {
		// override_origin needs its seconds; without them Cloudflare's
		// error does not say which field is missing.
		params = map[string]any{"cache": true, "edge_ttl": map[string]any{"mode": "override_origin", "default": ttl}}
		desc = fmt.Sprintf("middleware: %s%s for %ds", host, path, ttl)
	}
	rule := map[string]any{"expression": expr, "description": desc, "action": "set_cache_settings", "action_parameters": params, "enabled": true}
	_, err = c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/rulesets/"+url.PathEscape(cur.RulesetID)+"/rules", rule, nil)
	return err
}

// DeleteCacheRule removes one rule.
func (c *Client) DeleteCacheRule(ctx context.Context, zoneID, rulesetID, ruleID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zoneID)+"/rulesets/"+url.PathEscape(rulesetID)+"/rules/"+url.PathEscape(ruleID), nil, nil)
	return err
}

// CreateRecord adds a record as given.
func (c *Client) CreateRecord(ctx context.Context, zoneID string, r Record) (Record, error) {
	var out Record
	_, err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", r, &out)
	return out, err
}

// UpdateRecord replaces a record.
func (c *Client) UpdateRecord(ctx context.Context, zoneID string, r Record) (Record, error) {
	var out Record
	_, err := c.do(ctx, http.MethodPut, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(r.ID), r, &out)
	return out, err
}

// AccessApp is one Cloudflare Access application.
type AccessApp struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	Type   string `json:"type"`
}

// AccessApps lists the account's Access applications: the domains that
// Cloudflare asks to log in before anything reaches this house.
func (c *Client) AccessApps(ctx context.Context) ([]AccessApp, error) {
	if c.AccountID == "" {
		return nil, errors.New("cloudflare: no account id")
	}
	var apps []AccessApp
	err := c.list(ctx, "/accounts/"+url.PathEscape(c.AccountID)+"/access/apps", nil, &apps)
	sort.Slice(apps, func(i, j int) bool { return apps[i].Domain < apps[j].Domain })
	return apps, err
}

// RegistrarDomain is a domain the account owns at Cloudflare's registrar.
type RegistrarDomain struct {
	Name      string `json:"name"`
	ExpiresAt string `json:"expires_at"`
	AutoRenew bool   `json:"auto_renew"`
	Locked    bool   `json:"locked"`
}

// RegistrarDomains lists the account's registered domains, with when each
// expires and whether it renews by itself: where "this expires next week"
// comes from.
func (c *Client) RegistrarDomains(ctx context.Context) ([]RegistrarDomain, error) {
	if c.AccountID == "" {
		return nil, errors.New("cloudflare: no account id")
	}
	var out []RegistrarDomain
	err := c.list(ctx, "/accounts/"+url.PathEscape(c.AccountID)+"/registrar/domains", nil, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt < out[j].ExpiresAt })
	return out, err
}

// The permissions this house uses, by the names Cloudflare gives its
// permission groups. A token with these and nothing else is the narrowest
// that runs every feature here.
var NeededPermissions = []string{
	"Zone Read", "DNS Write", "Cloudflare Tunnel Write", "Cache Purge",
	"Cache Settings Write", "Access: Apps and Policies Read", "Registrar Domains Read",
}

// groupKey compares permission names as Cloudflare's screens and its API
// disagree on them: the token page lists "Registrar Domains Read", the
// docs "Registrar: Domains Read". An exact match called a token that had
// it short of it.
func groupKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TokenScope is what the current token may do.
type TokenScope struct {
	ID          string   `json:"id"`
	Permissions []string `json:"permissions"`
	// Extra are permissions beyond what the house uses: a wide token.
	Extra []string `json:"extra"`
	// Missing are permissions the house uses that it lacks.
	Missing []string `json:"missing"`
}

// Scope reads the current account token's permissions. It needs the token
// to be allowed to read itself; when it is not, the error says so and
// nothing is guessed.
func (c *Client) Scope(ctx context.Context) (TokenScope, error) {
	if c.AccountID == "" {
		return TokenScope{}, errors.New("cloudflare: no account id")
	}
	var v struct {
		ID string `json:"id"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(c.AccountID)+"/tokens/verify", nil, &v); err != nil {
		return TokenScope{}, err
	}
	var tok struct {
		Policies []struct {
			Groups []struct {
				Name string `json:"name"`
			} `json:"permission_groups"`
		} `json:"policies"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(c.AccountID)+"/tokens/"+url.PathEscape(v.ID), nil, &tok); err != nil {
		return TokenScope{}, err
	}
	have := map[string]bool{}
	for _, p := range tok.Policies {
		for _, g := range p.Groups {
			have[g.Name] = true
		}
	}
	haveKey := map[string]bool{}
	for n := range have {
		haveKey[groupKey(n)] = true
	}
	out := TokenScope{ID: v.ID, Permissions: []string{}, Extra: []string{}, Missing: []string{}}
	need := map[string]bool{}
	for _, n := range NeededPermissions {
		need[groupKey(n)] = true
		if !haveKey[groupKey(n)] {
			out.Missing = append(out.Missing, n)
		}
	}
	for n := range have {
		out.Permissions = append(out.Permissions, n)
		if !need[groupKey(n)] {
			out.Extra = append(out.Extra, n)
		}
	}
	sort.Strings(out.Permissions)
	sort.Strings(out.Extra)
	return out, nil
}

// Narrow mints a new account token with exactly NeededPermissions over the
// account, and returns its secret. The caller stores it in place of the
// current one; the old token is left for the operator to revoke, because
// revoking the one in use before the new one is proven is how a house
// loses its DNS.
func (c *Client) Narrow(ctx context.Context, name string) (string, []string, error) {
	if c.AccountID == "" {
		return "", nil, errors.New("cloudflare: no account id")
	}
	var groups []struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(c.AccountID)+"/tokens/permission_groups", nil, &groups); err != nil {
		return "", nil, err
	}
	type group struct {
		id   string
		zone bool
	}
	byName := map[string]group{}
	for _, g := range groups {
		zone := false
		for _, sc := range g.Scopes {
			if strings.Contains(sc, ".zone") {
				zone = true
			}
		}
		byName[groupKey(g.Name)] = group{id: g.ID, zone: zone}
	}
	// Zone permissions and account permissions take different resources,
	// so they go in two policies: the zones of this account, and the
	// account itself.
	var zoneIDs, accountIDs []map[string]string
	var missing []string
	for _, n := range NeededPermissions {
		g, ok := byName[groupKey(n)]
		switch {
		case !ok:
			missing = append(missing, n)
		case g.zone:
			zoneIDs = append(zoneIDs, map[string]string{"id": g.id})
		default:
			accountIDs = append(accountIDs, map[string]string{"id": g.id})
		}
	}
	if len(missing) > 0 {
		// Cloudflare renamed a group: say which, do not mint a token
		// that quietly cannot do part of the job.
		return "", missing, fmt.Errorf("cloudflare: no permission group named %s", strings.Join(missing, ", "))
	}
	account := "com.cloudflare.api.account." + c.AccountID
	var policies []any
	if len(zoneIDs) > 0 {
		policies = append(policies, map[string]any{"effect": "allow",
			"resources":         map[string]any{account: map[string]string{"com.cloudflare.api.account.zone.*": "*"}},
			"permission_groups": zoneIDs})
	}
	if len(accountIDs) > 0 {
		policies = append(policies, map[string]any{"effect": "allow",
			"resources":         map[string]string{account: "*"},
			"permission_groups": accountIDs})
	}
	body := map[string]any{"name": name, "policies": policies}
	var out struct {
		Value string `json:"value"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/accounts/"+url.PathEscape(c.AccountID)+"/tokens", body, &out); err != nil {
		return "", nil, err
	}
	if out.Value == "" {
		return "", nil, errors.New("cloudflare: the new token came back without its secret")
	}
	return out.Value, nil, nil
}

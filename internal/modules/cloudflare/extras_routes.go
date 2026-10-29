package cloudflare

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/dcimorra/the-middleware/internal/modules"
)

// extraRoutes are the panel's edge screens: records, edge cache, Access,
// the token and the registrar.
func (m *Module) extraRoutes() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /zones/{zone}/records":                   m.withClient(m.listRecords),
		"POST /zones/{zone}/records":                  m.withClient(m.createRecord),
		"PUT /zones/{zone}/records/{id}":              m.withClient(m.updateRecord),
		"DELETE /zones/{zone}/records/{id}":           m.withClient(m.deleteRecord),
		"POST /purge":                                 m.withClient(m.purge),
		"GET /cache-rules":                            m.withClient(m.cacheRules),
		"POST /cache-rules":                           m.withClient(m.addCacheRule),
		"DELETE /cache-rules/{zone}/{ruleset}/{rule}": m.withClient(m.deleteCacheRule),
		"GET /access":                                 m.withClient(m.access),
		"GET /token":                                  m.withClient(m.token),
		"POST /token/narrow":                          m.withClient(m.narrow),
		"GET /registrar":                              m.withClient(m.registrar),
	}
}

func (m *Module) withClient(f func(c *Client, w http.ResponseWriter, r *http.Request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := m.Client()
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		v, err := f(c, w, r)
		if err != nil {
			status := http.StatusBadGateway
			var ue userError
			if errors.As(err, &ue) {
				status = ue.status
			} else if IsForbidden(err) {
				status = http.StatusForbidden
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, v)
	})
}

// userError is a mistake in the request, not the API's.
type userError struct {
	status int
	msg    string
}

func (e userError) Error() string { return e.msg }

func readBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return userError{http.StatusBadRequest, "body: " + err.Error()}
	}
	return nil
}

func (m *Module) listRecords(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	recs, err := c.DNSRecords(r.Context(), r.PathValue("zone"))
	if recs == nil {
		recs = []Record{}
	}
	return recs, err
}

func (m *Module) createRecord(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	var rec Record
	if err := readBody(r, &rec); err != nil {
		return nil, err
	}
	return c.CreateRecord(r.Context(), r.PathValue("zone"), rec)
}

func (m *Module) updateRecord(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	var rec Record
	if err := readBody(r, &rec); err != nil {
		return nil, err
	}
	rec.ID = r.PathValue("id")
	return c.UpdateRecord(r.Context(), r.PathValue("zone"), rec)
}

func (m *Module) deleteRecord(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]bool{"ok": true}, c.DeleteRecord(r.Context(), r.PathValue("zone"), r.PathValue("id"))
}

// zoneFor is the zone a domain lives in, by the longest suffix: with
// example.com and sub.example.com in the account, a record for
// www.sub.example.com belongs to the second.
func zoneFor(c *Client, r *http.Request, domain string) (Zone, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return Zone{}, userError{http.StatusBadRequest, "domain: required"}
	}
	return c.FindZone(r.Context(), domain)
}

func (m *Module) purge(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		Domain string   `json:"domain"`
		Hosts  []string `json:"hosts"`
	}
	if err := readBody(r, &body); err != nil {
		return nil, err
	}
	z, err := zoneFor(c, r, body.Domain)
	if err != nil {
		return nil, err
	}
	hosts := body.Hosts
	if len(hosts) == 0 {
		hosts = []string{body.Domain}
	}
	return map[string]any{"ok": true, "zone": z.Name, "hosts": hosts}, c.PurgeHosts(r.Context(), z.ID, hosts)
}

func (m *Module) cacheRules(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	domain := r.URL.Query().Get("domain")
	z, err := zoneFor(c, r, domain)
	if err != nil {
		return nil, err
	}
	rules, err := c.GetCacheRules(r.Context(), z.ID, domain)
	return map[string]any{"zone": z.Name, "zone_id": z.ID, "ruleset_id": rules.RulesetID, "rules": rules.Rules}, err
}

func (m *Module) addCacheRule(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		Domain string `json:"domain"`
		Path   string `json:"path"`
		TTL    int    `json:"ttl"`
	}
	if err := readBody(r, &body); err != nil {
		return nil, err
	}
	if body.TTL < 0 {
		return nil, userError{http.StatusBadRequest, "ttl: seconds at the edge, or 0 to bypass"}
	}
	z, err := zoneFor(c, r, body.Domain)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, c.AddCacheRule(r.Context(), z.ID, strings.ToLower(body.Domain), body.Path, body.TTL)
}

// deleteCacheRule removes a rule; one that is not the asking site's needs
// ?other=1, said on purpose: rules of the zone's other domains are
// editable (half a zone may be names this house does not serve) but not
// in passing.
func (m *Module) deleteCacheRule(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	if domain := r.URL.Query().Get("domain"); domain != "" && r.URL.Query().Get("other") != "1" {
		cur, err := c.GetCacheRules(r.Context(), r.PathValue("zone"), domain)
		if err != nil {
			return nil, err
		}
		for _, rule := range cur.Rules {
			if rule.ID == r.PathValue("rule") && rule.Scope != "site" {
				return nil, userError{http.StatusConflict, "this rule is not " + domain + "'s (" + rule.Scope + "): it changes how other sites are served. Ask again with other=1 to delete it anyway"}
			}
		}
	}
	return map[string]bool{"ok": true}, c.DeleteCacheRule(r.Context(), r.PathValue("zone"), r.PathValue("ruleset"), r.PathValue("rule"))
}

func (m *Module) access(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	apps, err := c.AccessApps(r.Context())
	if apps == nil {
		apps = []AccessApp{}
	}
	return apps, err
}

func (m *Module) registrar(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	ds, err := c.RegistrarDomains(r.Context())
	if ds == nil {
		ds = []RegistrarDomain{}
	}
	return ds, err
}

func (m *Module) token(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	return c.Scope(r.Context())
}

// narrow mints the narrow token, proves it, and only then stores it in
// place of the current one. The old one is the operator's to revoke.
func (m *Module) narrow(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	old, _ := c.Scope(r.Context())
	tok, missing, err := c.Narrow(r.Context(), "middleware (narrowed)")
	if err != nil {
		if len(missing) > 0 {
			return nil, userError{http.StatusConflict, err.Error()}
		}
		return nil, err
	}
	probe := *c
	probe.Token = tok
	if st, err := probe.TokenStatus(r.Context()); err != nil || st != "active" {
		return nil, errors.New("the new token does not verify yet; the current one stays in use")
	}
	if err := m.mgr.Patch(r.Context(), Key, modules.Config{"token": tok}); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "revoke": old.ID, "permissions": NeededPermissions}, nil
}

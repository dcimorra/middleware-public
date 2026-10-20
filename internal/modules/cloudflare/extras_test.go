package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// edgeFake answers the extras' endpoints and records what was asked.
type edgeFake struct {
	mu       sync.Mutex
	calls    []string
	bodies   []map[string]any
	ruleset  bool
	rules    []map[string]any
	groups   []map[string]any
	tokenOut string
}

func (f *edgeFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		json.Unmarshal(b, &body)
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.bodies = append(f.bodies, body)
	ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/entrypoint") && r.Method == "GET":
		if !f.ruleset {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 10003, "message": "not found"}}})
			return
		}
		ok(map[string]any{"id": "rs1", "rules": f.rules})
	case strings.HasSuffix(p, "/entrypoint") && r.Method == "PUT":
		f.ruleset = true
		ok(map[string]any{"id": "rs1", "rules": []any{}})
	case strings.HasSuffix(p, "/rulesets/rs1/rules") && r.Method == "POST":
		body["id"] = "r" + string(rune('0'+len(f.rules)))
		f.rules = append(f.rules, body)
		ok(map[string]any{"id": "rs1"})
	case strings.HasSuffix(p, "/purge_cache"):
		ok(map[string]any{"id": "x"})
	case strings.HasSuffix(p, "/tokens/verify"):
		ok(map[string]any{"id": "tok1", "status": "active"})
	case strings.HasSuffix(p, "/tokens/tok1"):
		ok(map[string]any{"policies": []any{map[string]any{"permission_groups": []any{map[string]any{"name": "DNS Write"}, map[string]any{"name": "Account Settings Write"}, map[string]any{"name": "Zone Read"}}}}})
	case strings.HasSuffix(p, "/tokens/permission_groups"):
		ok(f.groups)
	case strings.HasSuffix(p, "/tokens") && r.Method == "POST":
		ok(map[string]any{"id": "tok2", "value": f.tokenOut})
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"success": false})
	}
}

func newEdgeFake(t *testing.T) (*edgeFake, *Client) {
	f := &edgeFake{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &Client{Token: "t", AccountID: "acct1", BaseURL: srv.URL}
}

func TestACacheRuleIsBornScopedToItsHostAndAddedOneByOne(t *testing.T) {
	f, c := newEdgeFake(t)
	ctx := context.Background()
	if err := c.AddCacheRule(ctx, "z1", "shop.example.com", "/product/", 3600); err != nil {
		t.Fatal(err)
	}
	// A zone with no ruleset: an empty list first, then the rule by itself.
	if !strings.Contains(strings.Join(f.calls, "\n"), "PUT /zones/z1/rulesets/phases/http_request_cache_settings/entrypoint\nPOST /zones/z1/rulesets/rs1/rules") {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	if put := f.bodies[1]; len(put["rules"].([]any)) != 0 {
		t.Fatalf("the list was written with rules in it: %v", put)
	}
	rule := f.rules[0]
	if rule["expression"] != `(http.host eq "shop.example.com" and starts_with(http.request.uri.path, "/product"))` {
		t.Fatalf("expression %v", rule["expression"])
	}
	edge := rule["action_parameters"].(map[string]any)["edge_ttl"].(map[string]any)
	if edge["mode"] != "override_origin" || edge["default"] != float64(3600) {
		t.Fatalf("edge ttl %v", edge)
	}
	// A bypass, and no rewrite of the list now that it exists.
	f.calls = nil
	c.AddCacheRule(ctx, "z1", "shop.example.com", "/admin", 0)
	for _, call := range f.calls {
		if strings.HasPrefix(call, "PUT") {
			t.Fatal("the list was rewritten to add a rule")
		}
	}
	if f.rules[1]["action_parameters"].(map[string]any)["cache"] != false {
		t.Fatalf("bypass %v", f.rules[1])
	}
	f.rules = append(f.rules, map[string]any{"id": "zz", "expression": `(starts_with(http.request.uri.path, "/static"))`}, map[string]any{"id": "nb", "expression": `(http.host eq "blog.example.com")`})
	cr, err := c.GetCacheRules(ctx, "z1", "shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	scopes := []string{}
	for _, r := range cr.Rules {
		scopes = append(scopes, r.Scope)
	}
	if strings.Join(scopes, ",") != "site,site,zone,other" {
		t.Fatalf("scopes %v", scopes)
	}
}

func TestPurgeAskByHost(t *testing.T) {
	f, c := newEdgeFake(t)
	if err := c.PurgeHosts(context.Background(), "z1", []string{"shop.example.com", "www.shop.example.com"}); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "POST /zones/z1/purge_cache" || len(f.bodies[0]["hosts"].([]any)) != 2 {
		t.Fatalf("%v %v", f.calls, f.bodies)
	}
}

func TestATokenIsReadAndNarrowedToWhatTheHouseUses(t *testing.T) {
	f, c := newEdgeFake(t)
	ctx := context.Background()
	sc, err := c.Scope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sc.Extra, ",") != "Account Settings Write" || len(sc.Missing) != len(NeededPermissions)-2 {
		t.Fatalf("scope %+v", sc)
	}
	// A group missing from Cloudflare's list: refuse, mint nothing.
	f.groups = []map[string]any{{"id": "g1", "name": "Zone Read", "scopes": []string{"com.cloudflare.api.account.zone"}}}
	if _, missing, err := c.Narrow(ctx, "middleware"); err == nil || len(missing) == 0 {
		t.Fatalf("narrowed with groups missing: %v %v", missing, err)
	}
	for _, call := range f.calls {
		if call == "POST /accounts/acct1/tokens" {
			t.Fatal("a token was minted without every permission")
		}
	}
	f.groups = nil
	for i, n := range NeededPermissions {
		scope := "com.cloudflare.api.account"
		if n == "Zone Read" || n == "DNS Write" || strings.HasPrefix(n, "Cache") {
			scope = "com.cloudflare.api.account.zone"
		}
		f.groups = append(f.groups, map[string]any{"id": "g" + string(rune('a'+i)), "name": n, "scopes": []string{scope}})
	}
	f.tokenOut = "secret-new"
	tok, _, err := c.Narrow(ctx, "middleware")
	if err != nil || tok != "secret-new" {
		t.Fatalf("narrow: %q %v", tok, err)
	}
	body := f.bodies[len(f.bodies)-1]
	pols := body["policies"].([]any)
	if len(pols) != 2 {
		t.Fatalf("policies %v", pols)
	}
	zone := pols[0].(map[string]any)
	if _, nested := zone["resources"].(map[string]any)["com.cloudflare.api.account.acct1"].(map[string]any); !nested || len(zone["permission_groups"].([]any)) != 4 {
		t.Fatalf("the zone policy: %v", zone)
	}
}

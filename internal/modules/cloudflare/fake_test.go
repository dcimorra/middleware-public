package cloudflare

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// fakeAPI is the slice of api.cloudflare.com the client uses, with a
// permission set so a test can take one scope away and watch the line
// that fails. It records every write in order, because the order of the
// writes is what Publish is about.
type fakeAPI struct {
	mu       sync.Mutex
	token    string
	denied   map[string]bool // "zones.read", "dns.read", "dns.write", "accounts.read", "tunnel.read", "tunnel.write"
	zones    []Zone
	records  map[string][]Record // by zone id
	accounts []Account
	tunnels  []Tunnel
	ingress  []Ingress
	writes   []string
	nextID   int
	srv      *httptest.Server
}

func newFakeAPI() *fakeAPI {
	f := &fakeAPI{token: "good-token", denied: map[string]bool{}, records: map[string][]Record{}, nextID: 1}
	f.zones = []Zone{{ID: "z1", Name: "example.com", Status: "active"}, {ID: "z2", Name: "shop.example.com", Status: "active"}}
	f.zones[0].Account.ID = "acct1"
	f.zones[1].Account.ID = "acct1"
	f.accounts = []Account{{ID: "acct1", Name: "Main"}}
	f.tunnels = []Tunnel{{ID: "tun1", Name: "house", AccountID: "acct1"}}
	f.ingress = []Ingress{{Hostname: "old.example.com", Service: "http://127.0.0.1:1"}, {Service: CatchAll}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *fakeAPI) client() *Client {
	return &Client{Token: f.token, AccountID: "acct1", TunnelID: "tun1", Service: "http://127.0.0.1:46100", BaseURL: f.srv.URL}
}

func (f *fakeAPI) ok(w http.ResponseWriter, result any, page, pages int) {
	w.Header().Set("Content-Type", "application/json")
	env := map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result}
	if pages > 0 {
		env["result_info"] = map[string]int{"page": page, "total_pages": pages}
	}
	_ = json.NewEncoder(w).Encode(env)
}

func (f *fakeAPI) fail(w http.ResponseWriter, status int, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": code, "message": msg}}, "result": nil})
}

func (f *fakeAPI) page(w http.ResponseWriter, r *http.Request, items []any) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	per := 50
	pages := (len(items) + per - 1) / per
	if pages == 0 {
		pages = 1
	}
	start := (page - 1) * per
	if start > len(items) {
		start = len(items)
	}
	end := start + per
	if end > len(items) {
		end = len(items)
	}
	f.ok(w, items[start:end], page, pages)
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.fail(w, http.StatusBadRequest, 6003, "Invalid request headers")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.Split(p, "/")
	switch {
	case p == "user/tokens/verify":
		f.ok(w, map[string]string{"id": "t", "status": "active"}, 0, 0)
	case p == "zones":
		if f.denied["zones.read"] {
			f.fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
			return
		}
		items := make([]any, len(f.zones))
		for i, z := range f.zones {
			items[i] = z
		}
		f.page(w, r, items)
	case len(parts) == 3 && parts[0] == "zones" && parts[2] == "dns_records":
		zone := parts[1]
		switch r.Method {
		case http.MethodGet:
			if f.denied["dns.read"] {
				f.fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
				return
			}
			items := make([]any, len(f.records[zone]))
			for i, rec := range f.records[zone] {
				items[i] = rec
			}
			f.page(w, r, items)
		case http.MethodPost:
			if f.denied["dns.write"] {
				f.fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
				return
			}
			var rec Record
			_ = json.NewDecoder(r.Body).Decode(&rec)
			rec.ID = fmt.Sprintf("r%d", f.nextID)
			f.nextID++
			f.records[zone] = append(f.records[zone], rec)
			f.writes = append(f.writes, "dns.create "+rec.Type+" "+rec.Name+" "+rec.Content)
			f.ok(w, rec, 0, 0)
		default:
			http.NotFound(w, r)
		}
	case len(parts) == 4 && parts[0] == "zones" && parts[2] == "dns_records":
		zone, id := parts[1], parts[3]
		if f.denied["dns.write"] {
			f.fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
			return
		}
		recs := f.records[zone]
		idx := -1
		for i, rec := range recs {
			if rec.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			f.fail(w, http.StatusNotFound, 81044, "Record does not exist.")
			return
		}
		switch r.Method {
		case http.MethodPut:
			var rec Record
			_ = json.NewDecoder(r.Body).Decode(&rec)
			rec.ID = id
			recs[idx] = rec
			f.writes = append(f.writes, "dns.overwrite "+rec.Type+" "+rec.Name+" "+rec.Content)
			f.ok(w, rec, 0, 0)
		case http.MethodDelete:
			f.writes = append(f.writes, "dns.delete "+recs[idx].Type+" "+recs[idx].Name)
			f.records[zone] = append(recs[:idx], recs[idx+1:]...)
			f.ok(w, map[string]string{"id": id}, 0, 0)
		default:
			http.NotFound(w, r)
		}
	case p == "accounts":
		if f.denied["accounts.read"] {
			f.fail(w, http.StatusForbidden, 9109, "Unauthorized to access requested resource")
			return
		}
		items := make([]any, len(f.accounts))
		for i, a := range f.accounts {
			items[i] = a
		}
		f.page(w, r, items)
	case len(parts) == 3 && parts[0] == "accounts" && parts[2] == "cfd_tunnel":
		if f.denied["tunnel.read"] {
			f.fail(w, http.StatusForbidden, 10000, "Authentication error")
			return
		}
		items := make([]any, len(f.tunnels))
		for i, t := range f.tunnels {
			items[i] = t
		}
		f.page(w, r, items)
	case len(parts) == 5 && parts[0] == "accounts" && parts[2] == "cfd_tunnel" && parts[4] == "configurations":
		if parts[3] != "tun1" {
			f.fail(w, http.StatusNotFound, 1000, "tunnel not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			if f.denied["tunnel.read"] {
				f.fail(w, http.StatusForbidden, 10000, "Authentication error")
				return
			}
			f.ok(w, map[string]any{"tunnel_id": "tun1", "config": map[string]any{"ingress": f.ingress}}, 0, 0)
		case http.MethodPut:
			if f.denied["tunnel.write"] {
				f.fail(w, http.StatusForbidden, 10000, "Authentication error")
				return
			}
			var cfg tunnelConfig
			_ = json.NewDecoder(r.Body).Decode(&cfg)
			rules := cfg.Config.Ingress
			if len(rules) == 0 || rules[len(rules)-1].Hostname != "" {
				f.fail(w, http.StatusBadRequest, 1001, "last ingress rule must be a catch-all")
				return
			}
			f.ingress = rules
			hosts := make([]string, 0, len(rules))
			for _, rule := range rules {
				if rule.Hostname != "" {
					hosts = append(hosts, rule.Hostname)
				}
			}
			f.writes = append(f.writes, "ingress.set "+strings.Join(hosts, ","))
			f.ok(w, map[string]any{"config": cfg.Config}, 0, 0)
		default:
			http.NotFound(w, r)
		}
	case len(parts) == 5 && parts[0] == "accounts" && parts[2] == "cfd_tunnel" && parts[4] == "token":
		if f.denied["tunnel.read"] {
			f.fail(w, http.StatusForbidden, 10000, "Authentication error")
			return
		}
		f.ok(w, "eyJ-tunnel-token", 0, 0)
	default:
		f.fail(w, http.StatusNotFound, 7000, "No route for that URI: "+p)
	}
}

package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bakery is the case services were asked for: a mock, a Next.js app, a
// Drupal and its database, three names.
const bakery = `{
  "version": 2,
  "services": {
    "mock":   { "run": { "command": ["node","server.js"], "health": {"path":"/healthz"} },
                "domains": ["onion-nest-bakery.ddev.es"] },
    "next":   { "run": { "runtime":"docker", "docker": {"build":"deploy/next/Dockerfile","memory":"768m"},
                         "command": ["npm","start"], "env_files":["deploy/next/.env"], "health": {"path":"/api/health"} },
                "domains": ["onion-nest-bakery-next.ddev.es"] },
    "drupal": { "run": { "runtime":"docker", "docker": {"build":"deploy/drupal/Dockerfile","memory":"1g",
                         "volumes":["drupal:/var/lib/app"]}, "health": {"path":"/user/login","timeout":"10m"},
                         "env": {"DB_HOST": "$SERVICE_db_HOST", "DB_PORT": "$SERVICE_db_PORT"} },
                "domains": ["onion-nest-bakery-drupal.ddev.es"], "depends_on": ["db"] },
    "db":     { "run": { "runtime":"docker", "docker": {"image":"mariadb:11.4", "port": 3306} }, "internal": true }
  }
}`

func TestTheBakeryManifestParsesWithThreeNamesOnThreeServices(t *testing.T) {
	m, err := Parse([]byte(bakery))
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode() != ModeServices {
		t.Fatalf("mode %s, want services", m.Mode())
	}
	want := map[string]string{
		"onion-nest-bakery.ddev.es":        "mock",
		"onion-nest-bakery-next.ddev.es":   "next",
		"onion-nest-bakery-drupal.ddev.es": "drupal",
	}
	for host, svc := range want {
		if got, ok := m.ServiceFor(host, "/"); !ok || got != svc {
			t.Errorf("%s → %q, %v; want %s", host, got, ok, svc)
		}
	}
	if _, ok := m.ServiceFor("elsewhere.test", "/"); ok {
		t.Error("a name no service lists must answer nothing, not a service at random")
	}
	if m.Services["db"].HandsOver() || !m.Services["next"].HandsOver() {
		t.Error("a service without names restarts; one with names hands over")
	}
	hosts := strings.Join(m.Hosts("onion-nest-bakery.ddev.es"), " ")
	for host := range want {
		if !strings.Contains(hosts, host) {
			t.Errorf("Hosts leaves out %s, which would go unpublished: %s", host, hosts)
		}
	}
}

func TestServicesAreRefusedWhereTheyCannotWork(t *testing.T) {
	cases := map[string]string{
		"run and services":    `{"version":2,"run":{"command":["x"]},"services":{"a":{"run":{"command":["x"]},"domains":["a.test"]}}}`,
		"aliases":             `{"version":2,"aliases":["b.test"],"services":{"a":{"run":{"command":["x"]},"domains":["a.test"]}}}`,
		"one name twice":      `{"version":2,"services":{"a":{"run":{"command":["x"]},"domains":["a.test"]},"b":{"run":{"command":["y"]},"domains":["A.test"]}}}`,
		"no run":              `{"version":2,"services":{"a":{"domains":["a.test"]}}}`,
		"bad name":            `{"version":2,"services":{"Web_1":{"run":{"command":["x"]}}}}`,
		"worker clash":        `{"version":2,"workers":{"a":{"command":["x"]}},"services":{"a":{"run":{"command":["x"]}}}}`,
		"internal + names":    `{"version":2,"services":{"a":{"run":{"command":["x"]},"domains":["a.test"],"internal":true}}}`,
		"unknown dep":         `{"version":2,"services":{"a":{"run":{"command":["x"]},"depends_on":["b"]}}}`,
		"self dep":            `{"version":2,"services":{"a":{"run":{"command":["x"]},"depends_on":["a"]}}}`,
		"cycle":               `{"version":2,"services":{"a":{"run":{"command":["x"]},"depends_on":["b"]},"b":{"run":{"command":["x"]},"depends_on":["a"]}}}`,
		"latest":              `{"version":2,"services":{"a":{"run":{"runtime":"docker","docker":{"image":"mariadb:latest"}}}}}`,
		"no tag":              `{"version":2,"services":{"a":{"run":{"runtime":"docker","docker":{"image":"ghcr.io/acme/db"}}}}}`,
		"unknown ref":         `{"version":2,"services":{"a":{"run":{"command":["x","$SERVICE_nope_PORT"]}}}}`,
		"process → moving":    `{"version":2,"services":{"a":{"run":{"command":["x"],"env":{"U":"http://127.0.0.1:${SERVICE_b_PORT}"}}},"b":{"run":{"command":["y"]},"domains":["b.test"]}}}`,
		"prefix wildcard":     `{"version":2,"services":{"a":{"run":{"command":["x"]},"domains":["a.test/api/*"]}}}`,
		"bad domain":          `{"version":2,"services":{"a":{"run":{"command":["x"]},"domains":["not a name"]}}}`,
		"bad hold":            `{"version":2,"services":{"a":{"run":{"command":["x"]},"hold":{"enabled":true,"mode":"418"}}}}`,
		"bad access":          `{"version":2,"services":{"a":{"run":{"command":["x"]},"access":{"allow":["10.0.0/8"]}}}}`,
		"two allows":          `{"version":2,"access":{"allow":["10.0.0.0/8"]},"services":{"a":{"run":{"command":["x"]},"access":{"allow":["10.1.0.0/16"]}}}}`,
		"two passwords":       `{"version":2,"access":{"password":"h1"},"services":{"a":{"run":{"command":["x"]},"access":{"password":"h2"}}}}`,
		"one zone twice":      `{"version":2,"access":{"routes":{"/admin":{"password":"h"}}},"services":{"a":{"run":{"command":["x"]},"access":{"routes":{"/admin":{"password":"h"}}}}}}`,
		"handover + port":     `{"version":2,"services":{"a":{"run":{"command":["x"],"port":8080},"handover":true}}}`,
		"port in a run":       `{"version":2,"run":{"command":["x"],"runtime":"docker","docker":{"image":"a:1","port":80}}}`,
		"container → process": `{"version":2,"services":{"a":{"run":{"runtime":"docker","docker":{"image":"a:1"},"env":{"B":"$SERVICE_b_HOST"}}},"b":{"run":{"command":["y"]},"handover":false}}}`,
		"path on a hidden db": `{"version":2,"services":{"db":{"run":{"runtime":"docker","docker":{"image":"mariadb:11.4"},"health":{"path":"/"}}}}}`,
		"health cmd a run":    `{"version":2,"run":{"command":["x"],"health":{"command":["true"]}}}`,
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: parsed, want refused", name)
		}
	}
}

func TestAServiceMayReachAnotherThatDoesNotMove(t *testing.T) {
	ok := []string{
		// From a container, by name: the port inside does not move.
		`{"version":2,"services":{"a":{"run":{"runtime":"docker","docker":{"image":"a:1"},"env":{"B":"$SERVICE_b_HOST:$SERVICE_b_PORT"}}},"b":{"run":{"runtime":"docker","docker":{"build":"Dockerfile"}},"domains":["b.test"]}}}`,
		// From a process, a service that restarts in place.
		`{"version":2,"services":{"a":{"run":{"command":["x","${SERVICE_my_db_PORT}"]}},"my-db":{"run":{"runtime":"docker","docker":{"image":"postgres:16@sha256:abc"}}}}}`,
		// Several services on one name, by prefix, and the www an apex brings.
		`{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["shop.test"]},"api":{"run":{"command":["y"]},"domains":["shop.test/api"]}}}`,
	}
	for _, in := range ok {
		if _, err := Parse([]byte(in)); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
}

func TestTheLongestPrefixWinsByWholeSegments(t *testing.T) {
	m, err := Parse([]byte(`{"version":2,"services":{
		"web":{"run":{"command":["x"]},"domains":["shop.test"]},
		"api":{"run":{"command":["y"]},"domains":["shop.test/api"]},
		"v2":{"run":{"command":["z"]},"domains":["shop.test/api/v2/"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"/": "web", "/apix": "web", "/api": "api", "/api/users": "api",
		"/api/v2": "v2", "/api/v2/x": "v2", "//api/../api/v2": "v2",
	}
	for p, want := range cases {
		if got, _ := m.ServiceFor("shop.test", p); got != want {
			t.Errorf("%s → %s, want %s", p, got, want)
		}
	}
	if got, _ := m.ServiceFor("www.shop.test", "/api"); got != "api" {
		t.Errorf("www.shop.test/api → %q: an apex's www goes where the apex goes", got)
	}
}

func TestTheFolderNameMustBeOneOfTheServicesNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bakery.test")
	os.MkdirAll(dir, 0o755)
	write := func(s string) error {
		os.WriteFile(filepath.Join(dir, FileName), []byte(s), 0o644)
		_, err := Load(dir)
		return err
	}
	if err := write(`{"version":2,"services":{"a":{"run":{"command":["x"]},"domains":["other.test"]}}}`); err == nil {
		t.Error("a site whose own name no service answers must be refused")
	}
	if err := write(`{"version":2,"services":{"a":{"run":{"command":["x"]},"domains":["bakery.test/api"]}}}`); err == nil {
		t.Error("a site whose own name has only a prefix must be refused")
	}
	if err := write(`{"version":2,"services":{"a":{"run":{"command":["x"]},"domains":["bakery.test"]}}}`); err != nil {
		t.Error(err)
	}
	if err := write(`{"version":2,"services":{"a":{"run":{"command":["x"],"env_files":["../x/.env"]},"domains":["bakery.test"]}}}`); err == nil {
		t.Error("a service's env file outside the folder must be refused")
	}
	if err := write(`{"version":2,"services":{"a":{"run":{"runtime":"docker","docker":{"build":"../Dockerfile"}},"domains":["bakery.test"]}}}`); err == nil {
		t.Error("a service's Dockerfile outside the folder must be refused")
	}
	if err := write(`{"version":2,"services":{"a":{"run":{"runtime":"docker","docker":{"image":"a:1","volumes":["/:/host"]}},"domains":["bakery.test"]}}}`); err == nil {
		t.Error("a service's mount of the machine must be refused")
	}
	proj := filepath.Join(t.TempDir(), "tools")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, FileName), []byte(`{"version":2,"services":{"a":{"run":{"command":["x"]},"domains":["a.test"]}}}`), 0o644)
	if _, err := Load(proj); err == nil {
		t.Error("a project answers no name, a service's included")
	}
}

func TestAServicesRulesReplaceTheSitesExceptTheOnesThatNarrow(t *testing.T) {
	m, err := Parse([]byte(`{"version":2,"headers":{"X-Site":"1"},"waf":{"max_body":"1m"},
		"access":{"deny":["198.51.100.0/24"]},"services":{
		"web":{"run":{"command":["x"]},"domains":["shop.test"]},
		"api":{"run":{"command":["y"]},"domains":["api.shop.test"],"headers":{"X-Api":"1"},"waf":{"max_body":"50m"},
			"access":{"allow":["10.0.0.0/8"]},"hold":{"enabled":true}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	web, api := m.ForService("web"), m.ForService("api")
	if web.Headers["X-Site"] != "1" || web.WAF.MaxBody != "1m" || web.Hold != nil {
		t.Errorf("web without rules of its own has the site's: %+v", web)
	}
	if api.Headers["X-Api"] != "1" || api.Headers["X-Site"] != "" || api.WAF.MaxBody != "50m" || api.Hold == nil {
		t.Errorf("api's replace the site's: %+v", api)
	}
	// Access is never replaced: the site's stays in place and the
	// service's is judged after it.
	if api.Access != m.Access || len(api.Access.Allow) != 0 {
		t.Errorf("api's access replaced the site's: %+v", api.Access)
	}
	if m.Headers["X-Api"] != "" || m.Hold != nil {
		t.Error("the loaded manifest was changed")
	}
}

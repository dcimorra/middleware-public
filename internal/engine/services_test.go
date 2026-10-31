package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/ports"
	"github.com/dcimorra/the-middleware/internal/runtime"
)

// writeServices writes a site with three services run by the test binary:
// web on the site's name, api on another, and db with no name that web
// depends on and reaches through $SERVICE_db_PORT. Each answers what its
// own pointer file says.
func writeServices(t *testing.T, sites, domain string, apiEnv string) string {
	t.Helper()
	dir := filepath.Join(sites, domain)
	os.MkdirAll(dir, 0o755)
	for _, n := range []string{"web", "api", "db"} {
		if _, err := os.Stat(filepath.Join(dir, ".current-"+n)); err != nil {
			os.WriteFile(filepath.Join(dir, ".current-"+n), []byte(n+"-1\n"), 0o644)
		}
	}
	svc := func(name, extra string) string {
		return fmt.Sprintf(`{"command":[%q],"env":{"MW_TEST_SERVER":"1"%s},"env_from_file":{"BUILD":".current-%s"},"drain":"300ms","health":{"timeout":"10s"}}`, os.Args[0], extra, name)
	}
	m := fmt.Sprintf(`{"version":2,"services":{
		"web":{"run":%s,"domains":[%q],"depends_on":["db"]},
		"api":{"run":%s,"domains":[%q]},
		"db":{"run":%s,"internal":true}}}`,
		svc("web", `,"DB":"127.0.0.1:${SERVICE_db_PORT}"`), domain,
		svc("api", apiEnv), "api."+domain,
		svc("db", `,"MW_TEST_SLOW_START":"500ms"`))
	if err := os.WriteFile(filepath.Join(dir, "site.json"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func getBody(t *testing.T, port int) string {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

func waitPath(t *testing.T, e *Engine, host string, cond func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if sn, ok := e.LookupPath(host, "/"); ok && cond(sn) {
			return sn
		}
		time.Sleep(50 * time.Millisecond)
	}
	sn, _ := e.LookupPath(host, "/")
	t.Fatalf("condition not met for %s: %+v", host, sn)
	return sn
}

func TestEachNameOfASiteAnswersItsOwnService(t *testing.T) {
	e, sites := newTestEngine(t)
	writeServices(t, sites, "bakery.test", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	web := waitPath(t, e, "bakery.test", func(sn Snapshot) bool { return sn.Alive })
	api := waitPath(t, e, "api.bakery.test", func(sn Snapshot) bool { return sn.Alive })
	if web.Service != "web" || api.Service != "api" || web.Port == api.Port {
		t.Fatalf("web %+v api %+v: two names, two services, two ports", web, api)
	}
	if got := getBody(t, web.Port); got != "web-1" {
		t.Errorf("bakery.test answered %q", got)
	}
	if got := getBody(t, api.Port); got != "api-1" {
		t.Errorf("api.bakery.test answered %q", got)
	}
	if _, ok := e.LookupPath("db.bakery.test", "/"); ok {
		t.Error("an internal service answers no name")
	}
	site, _ := e.Site("bakery.test")
	if !site.Alive || len(site.Services) != 3 {
		t.Fatalf("the site's own snapshot: %+v", site)
	}
	// web waited for db, and was born knowing where it is.
	db := site.Services["db"]
	if !db.Alive || db.Port == 0 {
		t.Fatalf("db %+v", db)
	}
	if hosts := e.Hosts(); hosts["api.bakery.test"] != "bakery.test" {
		t.Errorf("a service's name is published as the site's: %v", hosts)
	}
}

func TestChangingOneServiceHandsOverThatOneOnly(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeServices(t, sites, "bakery.test", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	web := waitPath(t, e, "bakery.test", func(sn Snapshot) bool { return sn.Alive })
	api := waitPath(t, e, "api.bakery.test", func(sn Snapshot) bool { return sn.Alive })
	db, _ := e.Site("bakery.test")

	os.WriteFile(filepath.Join(dir, ".current-api"), []byte("api-2\n"), 0o644)
	api2 := waitPath(t, e, "api.bakery.test", func(sn Snapshot) bool { return sn.Alive && sn.Revision != api.Revision })
	if got := getBody(t, api2.Port); got != "api-2" {
		t.Errorf("api after its deploy answered %q", got)
	}
	web2, _ := e.LookupPath("bakery.test", "/")
	if web2.Port != web.Port || web2.Revision != web.Revision {
		t.Errorf("web was touched by api's deploy: %+v → %+v", web, web2)
	}
	after, _ := e.Site("bakery.test")
	if after.Services["db"].Port != db.Services["db"].Port {
		t.Error("db was touched by api's deploy")
	}

	// A db that restarts in place keeps its port, so web, which was born
	// with it, never has to restart.
	if err := e.RestartService(ctx, "bakery.test", "db"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		sn, _ := e.Site("bakery.test")
		if v := sn.Services["db"]; v.Alive && !v.Starting && v.Revision != "" {
			if v.Port != db.Services["db"].Port {
				t.Fatalf("db moved from port %d to %d on a restart", db.Services["db"].Port, v.Port)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if w, _ := e.LookupPath("bakery.test", "/"); w.Revision != web.Revision {
		t.Error("web restarted because db did")
	}
}

func TestABrokenServiceKeepsItsPreviousGenerationAndTheOthersCarryOn(t *testing.T) {
	e, sites := newTestEngine(t)
	writeServices(t, sites, "bakery.test", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	api := waitPath(t, e, "api.bakery.test", func(sn Snapshot) bool { return sn.Alive })
	web := waitPath(t, e, "bakery.test", func(sn Snapshot) bool { return sn.Alive })

	// The new api crashes before it listens.
	writeServices(t, sites, "bakery.test", `,"MW_TEST_CRASH":"1"`)
	broken := waitPath(t, e, "api.bakery.test", func(sn Snapshot) bool { return sn.Error != "" })
	if !broken.Alive || broken.Port != api.Port || getBody(t, broken.Port) != "api-1" {
		t.Fatalf("the failed api must keep serving its previous generation: %+v", broken)
	}
	if w, _ := e.LookupPath("bakery.test", "/"); !w.Alive || w.Port != web.Port || w.Error != "" {
		t.Errorf("web must not hear of api's failure: %+v", w)
	}
	site, _ := e.Site("bakery.test")
	if !strings.HasPrefix(site.Error, "api: ") {
		t.Errorf("the site says which service failed: %q", site.Error)
	}
}

func TestAServiceStoppedOrRemovedLeavesTheOthersAndItsNameGoesToThePage(t *testing.T) {
	e, sites := newTestEngine(t)
	dir := writeServices(t, sites, "bakery.test", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	web := waitPath(t, e, "bakery.test", func(sn Snapshot) bool { return sn.Alive })
	waitPath(t, e, "api.bakery.test", func(sn Snapshot) bool { return sn.Alive })

	b, _ := os.ReadFile(filepath.Join(dir, "site.json"))
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(strings.Replace(string(b), `"domains":["api.bakery.test"]`, `"domains":["api.bakery.test"],"disabled":true`, 1)), 0o644)
	sn := waitPath(t, e, "api.bakery.test", func(sn Snapshot) bool { return sn.Mode == "disabled" })
	if sn.Alive {
		t.Error("a disabled service runs nothing")
	}
	if w, _ := e.LookupPath("bakery.test", "/"); w.Port != web.Port {
		t.Error("web was touched by api's stop")
	}
}

func TestASiteMovingFromRunToServicesKeepsAnsweringMeanwhile(t *testing.T) {
	e, sites := newTestEngine(t)
	writeSite(t, sites, "bakery.test", "old", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	old := waitFor(t, e, "bakery.test", func(sn Snapshot) bool { return sn.Alive })

	writeServices(t, sites, "bakery.test", "")
	// The first moments: web waits for db, and the old generation answers.
	sn, _ := e.LookupPath("bakery.test", "/")
	if sn.Alive && sn.Port != old.Port && sn.Service == "" {
		t.Fatalf("%+v", sn)
	}
	web := waitPath(t, e, "bakery.test", func(sn Snapshot) bool { return sn.Alive && sn.Port != old.Port })
	if got := getBody(t, web.Port); got != "web-1" {
		t.Errorf("answered %q", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if getBody(t, old.Port) != "old" {
			return // the old generation was retired
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("the run the site had before was never stopped")
}

func TestServicesAreReAdoptedByANewEngine(t *testing.T) {
	e, sites := newTestEngine(t)
	writeServices(t, sites, "bakery.test", "")
	ctx, cancel := context.WithCancel(context.Background())
	go e.Run(ctx)
	web := waitPath(t, e, "bakery.test", func(sn Snapshot) bool { return sn.Alive })
	api := waitPath(t, e, "api.bakery.test", func(sn Snapshot) bool { return sn.Alive })
	before, _ := e.Site("bakery.test")
	cancel()
	time.Sleep(300 * time.Millisecond)

	e2 := New(Options{SitesDir: e.opts.SitesDir, DataDir: e.opts.DataDir, Ports: ports.New(41000, 41100), Bus: events.New(""), Tick: 200 * time.Millisecond,
		Runtimes: map[string]runtime.Runtime{"process": runtime.Process{}}})
	t.Cleanup(func() { stopEverything(e2) })
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go e2.Run(ctx2)
	web2 := waitPath(t, e2, "bakery.test", func(sn Snapshot) bool { return sn.Alive })
	api2 := waitPath(t, e2, "api.bakery.test", func(sn Snapshot) bool { return sn.Alive })
	// A few ticks: an adopted service whose spec came out different
	// would be handed over on one of them.
	time.Sleep(time.Second)
	after, _ := e2.Site("bakery.test")
	for _, n := range []string{"web", "api", "db"} {
		if after.Services[n].Port != before.Services[n].Port || after.Services[n].Revision != before.Services[n].Revision {
			t.Errorf("%s: %+v, was %+v: started again instead of adopted", n, after.Services[n], before.Services[n])
		}
	}
	if web2.Port != web.Port || api2.Port != api.Port {
		t.Error("the names were not routed to the adopted services")
	}
}

// A one-off command in a service runs in what the service runs: the
// running generation's spec, with the other services' addresses in it.
func TestAServicesSpecForACommandIsTheRunningGenerations(t *testing.T) {
	e, sites := newTestEngine(t)
	writeServices(t, sites, "bakery.test", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	web := waitPath(t, e, "bakery.test", func(sn Snapshot) bool { return sn.Alive })
	spec, err := e.ServiceSpec("bakery.test", "web")
	if err != nil {
		t.Fatal(err)
	}
	site, _ := e.Site("bakery.test")
	if spec.Service != "web" || spec.Revision != web.Revision || spec.Env["DB"] != "127.0.0.1:"+strconv.Itoa(site.Services["db"].Port) {
		t.Fatalf("spec %+v, db on %d", spec, site.Services["db"].Port)
	}
	if _, err := e.ServiceSpec("bakery.test", "nope"); err == nil {
		t.Error("a service the site does not have")
	}
}

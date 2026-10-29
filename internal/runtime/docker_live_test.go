package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// TestADockerSiteSeesItsOwnPathsBehindItsWalls runs a real container, so
// it only runs with DOCKER=1 and an image the machine already has. What
// it proves is what reading the code could not: the folder and HOME at
// their own paths, $SITE_DIR meaning the same inside, the port reachable
// on loopback with HOST=0.0.0.0 inside, the walls in place, files written
// inside owned by the supervisor's user, and the reaper taking only
// unclaimed leftovers of this house.
func TestADockerSiteSeesItsOwnPathsBehindItsWalls(t *testing.T) {
	if os.Getenv("DOCKER") != "1" {
		t.Skip("DOCKER=1 runs a real container")
	}
	image := os.Getenv("DOCKER_IMAGE")
	if image == "" {
		image = "python:3.12-slim"
	}
	root := t.TempDir()
	site := filepath.Join(root, "sites", "d.test")
	home := filepath.Join(root, "data", "homes", "d.test")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	os.WriteFile(filepath.Join(site, "public", "index.html"), []byte("inside"), 0o644)
	m, err := manifest.Parse([]byte(`{"version":2,"run":{"command":["sh","-c","touch $SITE_DIR/written-inside && echo $HOME > $SITE_DIR/home-inside && exec python3 -m http.server $PORT --bind $HOST --directory $SITE_DIR/public"],"runtime":"docker","docker":{"image":"` + image + `","memory":"128m","pids":64}}}`))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := manifest.Resolve("d.test", site, m)
	if err != nil {
		t.Fatal(err)
	}
	d := Docker{House: filepath.Join(root, "sites")}
	port := 47911
	inst, err := d.Start(context.Background(), spec, port, filepath.Join(root, "site.log"), home)
	if err != nil {
		t.Fatal(err)
	}
	name := inst.State().Container
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	if err := WaitReady(context.Background(), inst, manifest.Health{Path: "/"}, 60*time.Second); err != nil {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		t.Fatalf("not ready: %v\n%s", err, out)
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "inside" {
		t.Fatalf("served %q", body)
	}
	st, err := os.Stat(filepath.Join(site, "written-inside"))
	if err != nil {
		t.Fatalf("$SITE_DIR inside is not the site folder: %v", err)
	}
	if owner := st.Sys().(*syscall.Stat_t).Uid; int(owner) != os.Getuid() {
		t.Fatalf("a file written inside belongs to uid %d, not the supervisor's %d", owner, os.Getuid())
	}
	if got, _ := os.ReadFile(filepath.Join(site, "home-inside")); strings.TrimSpace(string(got)) != home {
		t.Fatalf("HOME inside is %q, want %q", got, home)
	}
	raw, err := exec.Command("docker", "inspect", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	var info []struct {
		HostConfig struct {
			CapDrop     []string
			SecurityOpt []string
			PidsLimit   int64
			Memory      int64
		}
	}
	json.Unmarshal(raw, &info)
	hc := info[0].HostConfig
	if strings.Join(hc.CapDrop, ",") != "ALL" || !strings.Contains(strings.Join(hc.SecurityOpt, ","), "no-new-privileges") || hc.PidsLimit != 64 || hc.Memory != 128<<20 {
		t.Fatalf("walls: %+v", hc)
	}
	// A leftover of this house nobody claims is reaped; the adopted one is not.
	stray := "mw-stray-" + strconv.Itoa(port)
	exec.Command("docker", "run", "-d", "--name", stray, "--label", HouseLabel+"="+d.House, image, "sleep", "60").Run()
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", stray).Run() })
	removed := d.Reap(context.Background(), map[string]bool{name: true})
	if len(removed) != 1 || removed[0] != stray {
		t.Fatalf("reaped %v", removed)
	}
	if !inst.(*dockerInstance).Alive() {
		t.Fatal("the reaper took the claimed container")
	}
	inst.Stop(context.Background(), time.Second)
	if out, _ := exec.Command("docker", "ps", "-a", "--filter", "name="+name, "-q").Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Fatal("the stopped container was left behind")
	}
}

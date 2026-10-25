package relocate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcimorra/middleware-public/internal/archive"
	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/migrate"
)

// Importing a site from a V1 house into a V2 one is a move whose source is
// not a node of the network: the V2 binary, run on the V1 machine, packs
// the site's folder and sends it to the V2 house over HTTPS with a token
// that house made for this one site and two hours (the import scope). The
// V1 site is stopped the way V1 stops a site, by writing disabled in its
// manifest, and started again the same way if the V2 copy does not come
// up. What V1 keeps of the site under its data/ (conversations, cost) is
// written in V1's own formats and stays there; the folder and its HOME go.

// HTTPTarget is a V2 house reached at its panel's address with a token.
type HTTPTarget struct {
	// Base is the house's address, "https://panel.example.com".
	Base  string
	Token string
	// Label names it in the messages; empty is Base.
	Label  string
	Client *http.Client
	// Install asks the house to install the site's dependencies before
	// putting it in place: they are not sent (node_modules is built for
	// the machine it was installed on).
	Install bool
}

// Name implements Target.
func (t HTTPTarget) Name() string {
	if t.Label != "" {
		return t.Label
	}
	return t.Base
}

func (t HTTPTarget) client() *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return &http.Client{}
}

func (t HTTPTarget) do(ctx context.Context, method, path, ctype string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.Base, "/")+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := t.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		if res.StatusCode == http.StatusRequestEntityTooLarge {
			msg += " (the bundle is larger than what the way in lets through: Cloudflare's tunnel takes 100 MB; reach the house by its ports door or the VPN)"
		}
		return fmt.Errorf("%s answered %d: %s", t.Name(), res.StatusCode, msg)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func jsonBody(v any) io.Reader {
	raw, _ := json.Marshal(v)
	return bytes.NewReader(raw)
}

// Prepare implements Preparer: the house installs the site's dependencies
// on its own and is asked how it goes, so the install is not inside one
// long request (which a Cloudflare tunnel cuts at 100 seconds, and the
// install with it). A house too old to have the route installs at the
// commit, as before.
func (t HTTPTarget) Prepare(ctx context.Context, d string) (string, error) {
	if !t.Install {
		return "", nil
	}
	if err := t.do(ctx, "POST", "/relocate/"+d+"/prepare", "", nil, nil); err != nil {
		if strings.Contains(err.Error(), "answered 404") || strings.Contains(err.Error(), "answered 403") {
			return "", nil
		}
		return "", err
	}
	for {
		var j struct {
			Done   bool   `json:"done"`
			Output string `json:"output"`
			Error  string `json:"error"`
		}
		if err := t.do(ctx, "GET", "/relocate/"+d+"/prepare", "", nil, &j); err == nil && j.Done {
			if j.Error != "" {
				return "", errors.New(j.Error)
			}
			return j.Output, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// Check implements Target.
func (t HTTPTarget) Check(ctx context.Context, d string, need Needs) error {
	return t.do(ctx, "POST", "/relocate/"+d+"/check", "application/json", jsonBody(need), nil)
}

// Send implements Target.
func (t HTTPTarget) Send(ctx context.Context, d string, r io.Reader) error {
	return t.do(ctx, "PUT", "/relocate/"+d, "application/gzip", r, nil)
}

// SendDelta implements Target.
func (t HTTPTarget) SendDelta(ctx context.Context, d string, r io.Reader) error {
	return t.do(ctx, "PUT", "/relocate/"+d+"/delta", "application/gzip", r, nil)
}

// Commit implements Target.
func (t HTTPTarget) Commit(ctx context.Context, d string, m []byte) error {
	return t.do(ctx, "POST", "/relocate/"+d+"/commit", "application/json", jsonBody(map[string]any{"manifest": base64.StdEncoding.EncodeToString(m), "install": t.Install}), nil)
}

// Discard implements Target.
func (t HTTPTarget) Discard(ctx context.Context, d string) error {
	return t.do(ctx, "DELETE", "/relocate/"+d, "", nil, nil)
}

// Remove implements Target.
func (t HTTPTarget) Remove(ctx context.Context, d string) error {
	return t.do(ctx, "DELETE", "/sites/"+d, "", nil, nil)
}

// Ready implements Target: the same rule as between nodes, read from the
// site as the house describes it.
func (t HTTPTarget) Ready(ctx context.Context, d string) (bool, string, error) {
	var s struct {
		Alive   bool   `json:"alive"`
		Error   string `json:"error"`
		Mode    string `json:"mode"`
		Workers map[string]struct {
			Alive bool `json:"alive"`
		} `json:"workers"`
	}
	if err := t.do(ctx, "GET", "/sites/"+d, "", nil, &s); err != nil {
		return false, "", err
	}
	switch {
	case s.Error != "":
		return false, s.Error, nil
	case s.Alive, s.Mode == "static":
		return true, "", nil
	case s.Mode == "project" && len(s.Workers) > 0:
		for name, w := range s.Workers {
			if !w.Alive {
				return false, "worker " + name + " is not up", nil
			}
		}
		return true, "", nil
	}
	return false, "not up yet", nil
}

// Publish implements Target. An import does not move the names: the V1
// house still has them, and taking them is the operator's yes in the V2
// panel (Publish, then "point it here" for each name the zone has on V1).
func (t HTTPTarget) Publish(context.Context, string, string) error {
	return errors.New("the names are moved from the V2 panel")
}

// ImportParts are what an import takes: the folder, without what the
// target installs again, and the site's HOME.
func ImportParts(root, domain string) []archive.Part {
	skip := func(name string) bool { return name == "node_modules" }
	return []archive.Part{
		{Name: "site", Path: filepath.Join(root, "sites", domain), SkipDir: skip},
		{Name: "data/homes/" + domain, Path: filepath.Join(root, "data", "homes", domain), SkipDir: skip},
	}
}

// V1Source is a V1 house's site as the source of an import.
type V1Source struct {
	// Root is the V1 house's root (the folder sites/ is in).
	Root string
	// Wait is how long the V1 engine gets to stop the site.
	Wait time.Duration
	// Running says a process still works in dir; replaced by tests.
	Running func(dir string) bool

	original  []byte
	converted []byte
	docker    bool
}

// Convert reads and converts the V1 manifest, before anything moves: a
// site V2 cannot honour yet is refused here.
func (s *V1Source) Convert(domain string) (notes []string, err error) {
	raw, err := os.ReadFile(filepath.Join(s.Root, "sites", domain, "site.json"))
	if err != nil {
		return nil, err
	}
	res := migrate.Convert(domain, raw)
	if res.Skipped != "" {
		return nil, errors.New(res.Skipped)
	}
	if !res.Ready() {
		return nil, fmt.Errorf("V2 cannot honour this site yet: %s", strings.Join(res.Blockers, "; "))
	}
	res.Manifest.Disabled = false
	out, err := json.MarshalIndent(res.Manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	s.original, s.converted = raw, append(out, '\n')
	s.docker = res.Manifest.Run != nil && res.Manifest.Run.Runtime == "docker"
	return res.Notes, nil
}

// Needs implements Needer.
func (s *V1Source) Needs(string) Needs { return Needs{Docker: s.docker} }

// Stop writes disabled into the V1 manifest, which is how V1 stops a site,
// and waits for its processes to be gone. It returns the converted V2
// manifest: that is what the V2 house commits.
func (s *V1Source) Stop(ctx context.Context, domain string) ([]byte, error) {
	if s.converted == nil {
		if _, err := s.Convert(domain); err != nil {
			return nil, err
		}
	}
	var m map[string]any
	if err := json.Unmarshal(s.original, &m); err != nil {
		return nil, err
	}
	m["disabled"] = true
	raw, _ := json.MarshalIndent(m, "", "  ")
	dir := filepath.Join(s.Root, "sites", domain)
	if err := fsx.WriteAtomic(filepath.Join(dir, "site.json"), append(raw, '\n'), 0o644); err != nil {
		return nil, err
	}
	wait := s.Wait
	if wait == 0 {
		wait = time.Minute
	}
	running := s.Running
	if running == nil {
		running = processIn
	}
	deadline := time.Now().Add(wait)
	for running(dir) {
		if time.Now().After(deadline) {
			_ = s.Start(domain, nil)
			return nil, fmt.Errorf("V1 did not stop %s in %s", domain, wait)
		}
		select {
		case <-ctx.Done():
			_ = s.Start(domain, nil)
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return s.converted, nil
}

// Start puts the V1 manifest back as it was, which starts the site in V1.
func (s *V1Source) Start(domain string, _ []byte) error {
	return fsx.WriteAtomic(filepath.Join(s.Root, "sites", domain, "site.json"), s.original, 0o644)
}

// Published implements Source: the V1 house's names are not this
// importer's to move.
func (s *V1Source) Published(string) (string, bool) { return "", false }

// Names implements Source.
func (s *V1Source) Names(string) []string { return nil }

// Unpublish implements Source.
func (s *V1Source) Unpublish(context.Context, string) {}

// Archive implements Source: the V1 site is left stopped, its folder
// where it was, for the V1 panel to archive once the V2 copy has served a
// while; going back is starting it there.
func (s *V1Source) Archive(context.Context, string, []byte) (string, error) { return "", nil }

// processIn says some process has its working directory in dir: what
// is left of a site V1 is stopping.
func processIn(dir string) bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	for _, e := range ents {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil {
			continue
		}
		if cwd == real || strings.HasPrefix(cwd, real+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

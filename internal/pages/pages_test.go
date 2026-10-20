package pages

import (
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every design draws every part of the page, escapes what came from
// outside, and leaves the house's own HTML (Extra) as HTML. A design that
// dropped the label or the foot would be a worse page, not another one.
func TestEveryDesignDrawsTheWholePage(t *testing.T) {
	n := Notice{Status: 503, Signal: Waiting, Label: "Starting", Title: "Shop <b>", Line: "line & more",
		Foot: "shop.example", Extra: template.HTML(`<form class="password"><input></form>`),
		LinkHref: "/", LinkText: "Front page", Reload: 5}
	for _, th := range Themes() {
		n.Theme = th.Key
		out := string(HTML(n))
		for _, want := range []string{"Starting", "Shop &lt;b&gt;", "line &amp; more", "shop.example", "503", `class="waiting"`, `<form class="password">`, `http-equiv="refresh" content="5"`, `class="exit" href="/"`, `lang="en"`} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q", th.Key, want)
			}
		}
		if strings.Contains(out, "Shop <b>") {
			t.Errorf("%s: the title was not escaped", th.Key)
		}
		for _, cls := range []string{".password", ".waiting", ".paused", ".broken", ".nothing", ".locked"} {
			if !strings.Contains(out, cls) {
				t.Errorf("%s: the design does not style %s", th.Key, cls)
			}
		}
	}
}

// The catalogue and the files cannot drift apart.
func TestEveryThemeHasAFileAndEveryFileATheme(t *testing.T) {
	files, _ := fs.Glob(themeFS, "themes/*.html")
	if len(files) != len(Themes()) {
		t.Fatalf("%d files for %d themes", len(files), len(Themes()))
	}
	if !IsTheme("") || IsTheme("neon") {
		t.Fatal("IsTheme")
	}
}

func TestAnAPIClientGetsJSON(t *testing.T) {
	r := httptest.NewRequest("GET", "http://a.example/api", nil)
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	Render(w, r, ForSite("down", "Shop", "a.example", ""))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 503 || body["error"] != "Not answering" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w.Header().Get("Retry-After") != "30" || w.Header().Get("X-Robots-Tag") == "" {
		t.Fatalf("headers %v", w.Header())
	}
}

func TestTheHouseThemeAppliesToPagesOfNoSite(t *testing.T) {
	defer SetHouseTheme("")
	SetHouseTheme("console")
	if HouseTheme() != "console" {
		t.Fatal(HouseTheme())
	}
	SetHouseTheme("neon")
	if HouseTheme() != DefaultTheme {
		t.Fatal("an unknown theme was kept")
	}
}

// TestWriteSamples writes every design in two states to SHOT_DIR, for a
// person (or a headless browser) to look at. Reading the CSS does not say
// whether a design still looks right after a change; looking does.
func TestWriteSamples(t *testing.T) {
	dir := os.Getenv("SHOT_DIR")
	if dir == "" {
		t.Skip("SHOT_DIR=<dir> writes samples")
	}
	form := template.HTML(`<form class="password"><input type="password" placeholder="Password"><button>Enter</button><p class="wrong">That password is not right.</p></form>`)
	for _, th := range Themes() {
		a := ForSite("starting", "Shop", "shop.example.com", th.Key)
		a.LinkHref, a.LinkText = "/", "Front page"
		os.WriteFile(filepath.Join(dir, th.Key+"-starting.html"), HTML(a), 0o644)
		b := ForSite("password", "Shop", "shop.example.com", th.Key)
		b.Extra = form
		os.WriteFile(filepath.Join(dir, th.Key+"-password.html"), HTML(b), 0o644)
	}
}

package panel

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, p string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
	return rec
}

func TestTheShellAnswersTheRootAndDeepLinks(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/sites/x", "/settings", "/anything/at/all"} {
		rec := get(t, h, p)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s: content type %q", p, ct)
		}
		if !strings.Contains(rec.Body.String(), "<title>") {
			t.Fatalf("%s: body is not the shell", p)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control %q", p, cc)
		}
	}
}

func TestAssetsCarryTheirContentType(t *testing.T) {
	h := Handler()
	cases := map[string]string{
		"/assets/app.js":       "text/javascript",
		"/assets/app.css":      "text/css",
		"/assets/i18n/en.json": "application/json",
	}
	for p, want := range cases {
		rec := get(t, h, p)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Fatalf("%s: content type %q, want %s", p, ct, want)
		}
		if rec.Header().Get("ETag") == "" {
			t.Fatalf("%s: no ETag", p)
		}
	}
}

func TestAMissingAssetIsA404NotTheShell(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/assets/nope.js", "/assets/", "/assets/index.html", "/assets/../index.html"} {
		rec := get(t, h, p)
		if rec.Code != 404 {
			t.Fatalf("%s: status %d, body %q", p, rec.Code, rec.Body.String()[:40])
		}
	}
}

func TestTheETagSavesTheSecondDownload(t *testing.T) {
	h := Handler()
	first := get(t, h, "/assets/app.js")
	tag := first.Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	req.Header.Set("If-None-Match", tag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status %d with matching ETag", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("a 304 carried a body")
	}
}

func TestEveryResponseCarriesTheCSP(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/assets/app.js", "/assets/missing.css"} {
		rec := get(t, h, p)
		if got := rec.Header().Get("Content-Security-Policy"); got != CSP {
			t.Fatalf("%s: CSP %q", p, got)
		}
	}
}

func TestOnlyGetAndHeadAreAnswered(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", rec.Code)
	}
}

func walkJS(t *testing.T, fn func(name, src string)) {
	t.Helper()
	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".js") {
			return nil
		}
		data, err := fs.ReadFile(FS, p)
		if err != nil {
			return err
		}
		fn(p, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The CSP forbids inline script and style, so a stray style="" or onclick=
// in the shell would render as nothing, with an error only the console
// sees. Same for the JavaScript: a style attribute set through setAttribute
// or innerHTML is blocked by style-src and looks like a layout bug.
func TestNothingInlineSlipsPastTheCSP(t *testing.T) {
	data, err := fs.ReadFile(FS, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, bad := range []string{"<style", " style=", "onclick=", "onload=", "javascript:", "<script>"} {
		if strings.Contains(s, bad) {
			t.Fatalf("index.html contains %q", bad)
		}
	}
	walkJS(t, func(name, src string) {
		for _, bad := range []string{`setAttribute("style"`, `setAttribute('style'`, ` style="`, `.innerHTML`, `.outerHTML`, `insertAdjacentHTML`} {
			if strings.Contains(src, bad) {
				t.Errorf("%s uses %s, which the CSP blocks or which bypasses escaping", name, bad)
			}
		}
	})
}

// A literal key is one followed by ) or ,; t("mode." + mode) builds the
// key at runtime and is covered by the prefix check below.
var tCall = regexp.MustCompile(`\bt\(\s*"([^"\\]+)"\s*[,)]`)
var tPrefix = regexp.MustCompile(`\bt\(\s*"([^"\\]+\.)"\s*\+`)

// Every user-visible string goes through the catalog, so a key that is used
// in the JavaScript and missing from en.json would show as the key itself,
// in production, to the operator. Node is not available to check this, so
// the Go test greps the embedded sources for literal keys.
func TestEveryStringKeyUsedInTheJavaScriptExistsInTheCatalog(t *testing.T) {
	data, err := fs.ReadFile(FS, "i18n/en.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]string
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("en.json is not a flat object of strings: %v", err)
	}
	used := map[string]bool{}
	walkJS(t, func(name, src string) {
		for _, m := range tCall.FindAllStringSubmatch(src, -1) {
			used[m[1]] = true
			if _, ok := catalog[m[1]]; !ok {
				t.Errorf("%s uses t(%q) which is not in en.json", name, m[1])
			}
		}
		for _, m := range tPrefix.FindAllStringSubmatch(src, -1) {
			found := false
			for k := range catalog {
				if strings.HasPrefix(k, m[1]) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s builds keys from prefix %q and en.json has none", name, m[1])
			}
		}
	})
	if len(used) < 50 {
		t.Fatalf("only %d keys found; the grep is probably broken", len(used))
	}
	for k, v := range catalog {
		if strings.TrimSpace(v) == "" {
			t.Errorf("en.json: %q is empty", k)
		}
	}
}

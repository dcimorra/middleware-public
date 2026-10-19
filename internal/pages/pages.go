// Package pages renders what a visitor sees when the site itself cannot
// answer: stopped, held, unknown host, upstream down. They are plain HTML
// with inline CSS, no assets, so they work through any door and under any
// failure. Strings come from a small catalog so operators can localise.
package pages

import (
	"html/template"
	"net/http"
	"strings"
)

// Page is what a visitor page needs.
type Page struct {
	Status  int
	Title   string
	Message string
	Lang    string
}

var tpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root{color-scheme:light dark;--fg:#1b1b1f;--bg:#f6f6f8;--mute:#6b6b75}
@media(prefers-color-scheme:dark){:root{--fg:#e8e8ec;--bg:#121216;--mute:#9a9aa6}}
html,body{height:100%;margin:0}
body{display:grid;place-items:center;background:var(--bg);color:var(--fg);
font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:24px;box-sizing:border-box}
main{max-width:34rem;text-align:center}
h1{font-size:1.5rem;font-weight:600;margin:0 0 .5rem}
p{margin:0;color:var(--mute)}
.mark{width:40px;height:40px;border-radius:12px;background:currentColor;opacity:.12;margin:0 auto 1.25rem}
</style></head>
<body><main><div class="mark"></div><h1>{{.Title}}</h1><p>{{.Message}}</p></main></body></html>
`))

// Catalog maps a key to a title and message. Operators may override entries
// from settings; the defaults are English.
var Catalog = map[string][2]string{
	"stopped":     {"This site is stopped", "Its owner has switched it off for now. Please come back later."},
	"hold":        {"Back in a moment", "This site is being updated. Please try again in a minute."},
	"unknown":     {"Nothing here", "No site answers to this name on this server."},
	"unavailable": {"Temporarily unavailable", "The site is starting or recovering. Please try again shortly."},
	"forbidden":   {"Access restricted", "You are not allowed to see this page."},
	"ratelimited": {"Slow down", "Too many requests from your address. Please wait a moment."},
	"underattack": {"Checking your browser", "This site is under heavy load. Please wait a few seconds and retry."},
	"doorclosed":  {"This entrance is closed", "The site is served through another door. If you are its operator, open this one in Inbound."},
	"toolarge":    {"Request too large", "The request body exceeds what this site accepts."},
	"nocert":      {"No certificate yet", "A certificate for this domain is being requested. Please retry shortly."},
}

// Write renders the catalog entry key with the given status.
func Write(w http.ResponseWriter, key string, status int, override ...string) {
	entry := Catalog[key]
	if entry[0] == "" {
		entry = Catalog["unavailable"]
	}
	p := Page{Status: status, Title: entry[0], Message: entry[1], Lang: "en"}
	if len(override) > 0 && strings.TrimSpace(override[0]) != "" {
		p.Title = override[0]
	}
	if len(override) > 1 && strings.TrimSpace(override[1]) != "" {
		p.Message = override[1]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(status)
	_ = tpl.Execute(w, p)
}

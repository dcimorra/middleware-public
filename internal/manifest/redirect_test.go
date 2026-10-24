package manifest

import "testing"

func TestRedirectsSendEachOldAddressWhereItSays(t *testing.T) {
	m := &Manifest{Redirects: []Redirect{
		{From: "/old", To: "/new"},
		{From: "/blog/*", To: "https://news.example.com/articles/*", Code: 308},
		{From: "shop.example.org/*", To: "https://example.com/shop/*", Code: 302},
		{From: "alias.example.net", To: "https://example.com/"},
	}}
	for _, c := range []struct {
		host, path, query, to string
		code                  int
		ok                    bool
	}{
		{"example.com", "/old", "", "/new", 301, true},
		{"example.com", "/old/", "", "/new", 301, true},
		{"example.com", "/older", "", "", 0, false},
		{"example.com", "/blog/2026/hello", "ref=x", "https://news.example.com/articles/2026/hello?ref=x", 308, true},
		{"example.com", "/blog", "", "https://news.example.com/articles/", 308, true},
		{"example.com", "/bloggers", "", "", 0, false},
		{"shop.example.org", "/item/7", "", "https://example.com/shop/item/7", 302, true},
		{"example.com", "/item/7", "", "", 0, false},
		{"alias.example.net", "/anything", "", "https://example.com/", 301, true},
	} {
		to, code, ok := m.RedirectFor(c.host, c.path, c.query)
		if ok != c.ok || to != c.to || code != c.code {
			t.Errorf("%s%s?%s: %q %d %v, want %q %d %v", c.host, c.path, c.query, to, code, ok, c.to, c.code, c.ok)
		}
	}
}

func TestABadRedirectIsSaidBeforeItIsWritten(t *testing.T) {
	for _, r := range []Redirect{
		{From: "/a", To: ""},
		{From: "/a", To: "/b", Code: 200},
		{From: "/a*b", To: "/b"},
		{From: "/a", To: "ftp://x/y"},
		{From: "/a", To: "example.com"},
	} {
		if r.Check() == nil {
			t.Errorf("%+v was accepted", r)
		}
	}
	if (Redirect{From: "old.example.com/*", To: "https://new.example.com/*", Code: 307}).Check() != nil {
		t.Fatal("a good rule was refused")
	}
}

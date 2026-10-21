package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestANameNobodyServesIsRecordedWithItsDoorAndAFewSamples(t *testing.T) {
	u := NewUnrouted()
	for i := 0; i < 7; i++ {
		r := httptest.NewRequest("GET", "http://old.example/wp-login.php", nil)
		r.Header.Set("User-Agent", "scanner\x07"+strings.Repeat("x", 400))
		u.Note("Old.Example", "ports", "203.0.113.9", r)
	}
	list := u.List()
	if len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	k := list[0]
	if k.Host != "old.example" || k.Count != 7 || k.Doors["ports"] != 7 || len(k.Samples) != 5 {
		t.Fatalf("%+v", k)
	}
	if s := k.Samples[0]; strings.Contains(s.Agent, "\x07") || len(s.Agent) > 170 || s.Path != "/wp-login.php" {
		t.Fatalf("sample %+v", s)
	}
	if !u.Forget("old.example") || len(u.List()) != 0 {
		t.Fatal("not forgotten")
	}
	// Full: the oldest goes.
	for i := 0; i < maxKnocks+5; i++ {
		u.Note("n"+strings.Repeat("a", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26))+".example", "", "", httptest.NewRequest("GET", "/", nil))
	}
	if n := len(u.List()); n > maxKnocks {
		t.Fatalf("%d names kept", n)
	}
}

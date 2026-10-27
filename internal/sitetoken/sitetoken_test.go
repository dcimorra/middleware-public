package sitetoken

import (
	"os"
	"path/filepath"
	"testing"
)

func TestATokenNamesItsSiteAndNothingElseOpens(t *testing.T) {
	dir := t.TempDir()
	k := New(dir)
	tok, err := k.Token("a.example")
	if err != nil {
		t.Fatal(err)
	}
	if k.Domain(tok) != "a.example" {
		t.Fatal("its own token does not open")
	}
	other, _ := k.Token("b.example")
	forged := "b.example" + tok[len("a.example"):]
	for _, bad := range []string{"", "a.example", forged, tok + "x", "../x." + other[len("b.example."):]} {
		if d := k.Domain(bad); d != "" {
			t.Errorf("%q opened %q", bad, d)
		}
	}
	// A new process derives the same token from the same key; deleting
	// the key rotates them all.
	if New(dir).Domain(tok) != "a.example" {
		t.Fatal("the key did not survive a restart")
	}
	os.Remove(filepath.Join(dir, "site-door.key"))
	if New(dir).Domain(tok) != "" {
		t.Fatal("deleting the key did not rotate the tokens")
	}
}

func TestThePushTokenAndTheDoorTokenDoNotOpenEachOther(t *testing.T) {
	k := New(t.TempDir())
	door, _ := k.Token("a.example")
	git, err := k.GitToken("a.example")
	if err != nil {
		t.Fatal(err)
	}
	if k.GitDomain(git) != "a.example" {
		t.Fatal("the push token does not name its site")
	}
	if k.GitDomain(door) != "" {
		t.Fatal("the door token pushes: the site's running process could push")
	}
	if k.Domain(git) != "" {
		t.Fatal("the push token opens the door")
	}
}

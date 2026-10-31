package sites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSaysWhatABuildWouldNotFindWithoutWritingAnything(t *testing.T) {
	f := newFixture(t, false)
	dir := filepath.Join(f.root, "sites", "bakery.test")
	os.MkdirAll(filepath.Join(dir, "deploy", "next"), 0o755)
	onDisk := `{"version":2,"static":{}}`
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(onDisk), 0o644)
	os.WriteFile(filepath.Join(dir, "deploy", "next", "Dockerfile"), []byte("FROM node:24-slim\n"), 0o644)

	draft := `{"version":2,"services":{
		"web":{"run":{"command":["node","server.js"]},"domains":["bakery.test"]},
		"next":{"run":{"runtime":"docker","docker":{"build":"deploy/next/Dockerfile"},"command":["npm","start"],"env_files":["deploy/next/.env"]},"domains":["next.bakery.test"]},
		"drupal":{"run":{"runtime":"docker","docker":{"build":"deploy/drupal/Dockerfile"}},"domains":["drupal.bakery.test"]}}}`
	v, err := f.m.Validate("bakery.test", []byte(draft), false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Error != "" || v.OK {
		t.Fatalf("want readable but not OK (drupal's Dockerfile is missing): %+v", v)
	}
	if len(v.Problems) != 1 || !strings.Contains(v.Problems[0], "deploy/drupal/Dockerfile") {
		t.Errorf("problems %q, want the missing Dockerfile only", v.Problems)
	}
	if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], ".env") {
		t.Errorf("warnings %q, want the missing env file", v.Warnings)
	}
	if len(v.Routes) != 4 { // the three names and the apex's www
		t.Errorf("routes %+v, want four", v.Routes)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "site.json")); string(b) != onDisk {
		t.Error("validate wrote the manifest")
	}

	v, _ = f.m.Validate("bakery.test", []byte(`{"version":2,"run":{"command":["x"]},"services":{}}`), false)
	if v.Error != "" {
		t.Errorf("an empty services is no services: %s", v.Error)
	}
	v, _ = f.m.Validate("bakery.test", []byte(`{"version":2,"run":{"comand":["x"]}}`), false)
	if v.Error == "" || v.OK {
		t.Error("a manifest the house would refuse must be reported as refused")
	}
	v, _ = f.m.Validate("bakery.test", nil, false)
	if !v.OK {
		t.Errorf("the manifest on disk is fine: %+v", v)
	}
}

func TestValidateNamesAnotherSitesNameOnlyToWhoeverSeesTheHouse(t *testing.T) {
	f := newFixture(t, false)
	for _, d := range []string{"bakery.test", "neighbour.test"} {
		os.MkdirAll(filepath.Join(f.root, "sites", d), 0o755)
		os.WriteFile(filepath.Join(f.root, "sites", d, "site.json"), []byte(`{"version":2}`), 0o644)
	}
	draft := []byte(`{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["bakery.test","neighbour.test"]}}}`)
	v, err := f.m.Validate("bakery.test", draft, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || !strings.Contains(strings.Join(v.Problems, " "), "served by neighbour.test") {
		t.Errorf("the operator is told whose name it is: %+v", v.Problems)
	}
	v, _ = f.m.Validate("bakery.test", draft, true)
	if v.OK || strings.Contains(strings.Join(v.Problems, " "), "served by") || !strings.Contains(strings.Join(v.Problems, " "), "another site") {
		t.Errorf("a site's own caller is told it is taken, not by whom: %+v", v.Problems)
	}
}

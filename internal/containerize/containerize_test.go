package containerize

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

var probe = Probe{
	NodeVersion:   func(context.Context, string, []string) string { return "v22.11.0" },
	PythonVersion: func(context.Context) string { return "3.12.3" },
	OSRelease:     func() []byte { return []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\n") },
}

// The image follows from what the command needs, at the version the site
// runs on today; what it cannot tell, it says, and does not guess.
func TestTheImageFollowsFromTheCommand(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "node_modules", ".bin"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", ".bin", "next"), []byte("#!/usr/bin/env node\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "serve.sh"), []byte("#!/usr/bin/env -S python3 -u\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/bash\n"), 0o755)
	run := func(cmd ...string) *manifest.Manifest {
		return &manifest.Manifest{Version: manifest.Version, Run: &manifest.Run{Command: cmd}}
	}
	for _, c := range []struct {
		m     *manifest.Manifest
		image string
	}{
		{run("node", "server.js"), "node:22-slim"},
		{run("npm", "run", "start"), "node:22-slim"},
		{run("./node_modules/.bin/next", "start"), "node:22-slim"},
		{run("python3", "app.py"), "python:3.12-slim"},
		{run("./serve.sh"), "python:3.12-slim"},
	} {
		p, err := For(context.Background(), dir, c.m, probe)
		if err != nil || p.Docker.Image != c.image {
			t.Errorf("%v: %+v %v, want %s", c.m.Run.Command, p.Docker, err, c.image)
		}
	}
	if bin, err := os.Readlink("/proc/self/exe"); err == nil {
		// The test binary itself: a binary linked here, or a static one.
		p, err := For(context.Background(), dir, run(bin), probe)
		if err != nil || (p.Docker.Image != "ubuntu:24.04" && p.Docker.Image != "gcr.io/distroless/static-debian12") {
			t.Errorf("a binary: %+v %v", p.Docker, err)
		}
	}
	for _, m := range []*manifest.Manifest{run("./run.sh"), run("ruby", "app.rb"), run("./missing"), {Version: manifest.Version}} {
		if _, err := For(context.Background(), dir, m, probe); !errors.Is(err, ErrCannot) {
			t.Errorf("%+v: %v, want it said it cannot", m.Run, err)
		}
	}
	// A Dockerfile the site has, or a container its manifest kept, wins.
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM node:22-slim\n"), 0o644)
	if p, err := For(context.Background(), dir, run("ruby", "app.rb"), probe); err != nil || p.Docker.Build != "Dockerfile" {
		t.Errorf("with a Dockerfile: %+v %v", p.Docker, err)
	}
	kept := run("ruby", "app.rb")
	kept.Run.Docker = &manifest.Docker{Image: "ruby:3.3-slim"}
	if p, err := For(context.Background(), dir, kept, probe); err != nil || p.Docker.Image != "ruby:3.3-slim" {
		t.Errorf("kept: %+v %v", p.Docker, err)
	}
}

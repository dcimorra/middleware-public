package relocate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dcimorra/middleware-public/internal/archive"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Target is the node a site moves to, as the mover talks to it: over the
// mesh in the house, over plain HTTP between two servers in the tests.
type Target interface {
	Name() string
	// Send and SendDelta stream a bundle into the target's staging.
	Send(ctx context.Context, domain string, body io.Reader) error
	SendDelta(ctx context.Context, domain string, body io.Reader) error
	// Commit puts the staged site in place with its manifest.
	Commit(ctx context.Context, domain string, manifestData []byte) error
	// Discard throws the staging away; Remove deletes a committed site
	// that did not come up (it is a copy: the source still has it).
	Discard(ctx context.Context, domain string) error
	Remove(ctx context.Context, domain string) error
	// Check says, before anything moves, that the target can take the
	// site: the name is free there and it can publish names (all of them).
	Check(ctx context.Context, domain string, need Needs) error
	// Ready says the site answers there, or why not.
	Ready(ctx context.Context, domain string) (bool, string, error)
	// Publish puts its names on door there, over what points here.
	Publish(ctx context.Context, domain, door string) error
}

// Preparer is a target that installs a site's dependencies in its staging,
// for its own machine, before the site is stopped here.
type Preparer interface {
	Prepare(ctx context.Context, domain string) error
}

// Source is this node's side of a move.
type Source interface {
	// Stop stops the site and returns the manifest it had.
	Stop(ctx context.Context, domain string) ([]byte, error)
	// Start puts that manifest back, which starts it again.
	Start(domain string, manifestData []byte) error
	// Published says on which door its names are, if they are.
	Published(domain string) (string, bool)
	// Names are every name the site answers to.
	Names(domain string) []string
	// Unpublish takes this node's routes and records for the site down;
	// records already pointing elsewhere are left alone.
	Unpublish(ctx context.Context, domain string)
	// Archive keeps the stopped site in this node's archive and removes
	// its folder.
	Archive(ctx context.Context, domain string, manifestData []byte) (string, error)
}

// Needs are what a site asks of the node it moves to.
type Needs struct {
	// Names are the ones published where it is: the target must be able
	// to publish them all.
	Names []string `json:"names,omitempty"`
	// Docker: it runs in a container, and the target must have a docker
	// that answers. A container site moved to a machine without docker
	// is a site that does not start there.
	Docker bool `json:"docker,omitempty"`
}

// Steps of a move, in order.
const (
	StepCheck   = "check"
	StepCopy    = "copy"
	StepInstall = "install"
	StepStop    = "stop"
	StepFinal   = "final"
	StepCommit  = "commit"
	StepStart   = "start"
	StepDNS     = "dns"
	StepCleanup = "cleanup"
	StepDone    = "done"
	// StepRestore is a failure's way back: the site started here again.
	StepRestore = "restore"
)

// Mover moves a site off this node.
type Mover struct {
	Root   string
	Source Source
	// ReadyWait is how long the target gets to answer healthy.
	ReadyWait time.Duration
	// Poll is how often it is asked.
	Poll time.Duration
	// Parts, when set, replaces Parts: what goes with the site. An import
	// from another installation takes the folder and its HOME, not the
	// house's records of it, which that installation writes its own way.
	Parts func(root, domain string) []archive.Part
	// Dir, when set, is the site's folder: an app imported from a machine
	// that is not a house lives anywhere, not under Root/sites.
	Dir string
	// Install has a target that is a Preparer install the site's
	// dependencies there after the first copy; Parts must then leave
	// node_modules behind (MoveParts).
	Install bool
}

// Needer is a Source that says itself what its site needs of the target,
// for a site whose manifest this package cannot read (a V1 one).
type Needer interface {
	Needs(domain string) Needs
}

// Report says what a move did.
type Report struct {
	Archive string        `json:"archive,omitempty"`
	Down    time.Duration `json:"down"`
	First   int64         `json:"first_bytes"`
	Last    int64         `json:"last_bytes"`
	DNS     string        `json:"dns,omitempty"`
}

// Move takes domain to t. progress hears each step as it starts.
func (m *Mover) Move(ctx context.Context, domain string, t Target, progress func(step, msg string)) (Report, error) {
	var rep Report
	if progress == nil {
		progress = func(string, string) {}
	}
	if !manifest.ValidName(domain) {
		return rep, ErrBadName
	}
	dir := filepath.Join(m.Root, "sites", domain)
	if m.Dir != "" {
		dir = m.Dir
	}
	if info, err := os.Lstat(dir); err != nil {
		return rep, fmt.Errorf("%s: no such site here", domain)
	} else if !info.IsDir() {
		return rep, ErrNotAFolder
	}
	parts := Parts(m.Root, domain)
	if m.Parts != nil {
		parts = m.Parts(m.Root, domain)
	}

	// Before anything is touched: the target must be able to take it,
	// and, when its names are published here, to publish them there. A
	// target without a DNS provider would leave the names pointing at a
	// node that no longer serves the site.
	need := NeedsOf(m.Root, domain, m.Source)
	if n, ok := m.Source.(Needer); ok {
		need = n.Needs(domain)
	}
	progress(StepCheck, "asking "+t.Name()+" whether it can take "+domain)
	if err := t.Check(ctx, domain, need); err != nil {
		return rep, err
	}

	// The first copy, with the site serving. What is written while it
	// goes is caught by the second, which carries everything changed
	// since this moment.
	progress(StepCopy, "copying "+domain+" to "+t.Name()+" while it serves")
	since := time.Now().Add(-2 * time.Second)
	n, err := m.stream(ctx, parts, time.Time{}, func(r io.Reader) error { return t.Send(ctx, domain, r) })
	if err != nil {
		_ = t.Discard(ctx, domain)
		return rep, fmt.Errorf("first copy: %w", err)
	}
	rep.First = n

	// node_modules built here is built for this machine's Node: a native
	// module (better-sqlite3, sharp) compiled for Node 24 does not load on
	// a target whose site runs on 18, and the first moves carried it and
	// failed at the start with ERR_DLOPEN_FAILED. Installed there instead,
	// with the Node the site gets there, while it still serves here, so
	// the install is not part of the time it is down.
	if pr, ok := t.(Preparer); ok && m.Install {
		progress(StepInstall, "installing its dependencies on "+t.Name()+", for that machine, while it serves here")
		if err := pr.Prepare(ctx, domain); err != nil {
			_ = t.Discard(ctx, domain)
			return rep, fmt.Errorf("installing its dependencies there: %w", err)
		}
	}

	progress(StepStop, "stopping "+domain+" here for the last copy")
	original, err := m.Source.Stop(ctx, domain)
	if err != nil {
		_ = t.Discard(ctx, domain)
		return rep, fmt.Errorf("stop: %w", err)
	}
	stopped := time.Now()
	// From here every failure before the target answers puts the source
	// back as it was.
	restore := func(cause error) (Report, error) {
		progress(StepRestore, "starting it here again, as it was")
		if err := m.Source.Start(domain, original); err != nil {
			return rep, fmt.Errorf("%w; and starting it here again failed too: %v", cause, err)
		}
		return rep, fmt.Errorf("%w; it runs here again, as it was", cause)
	}

	progress(StepFinal, "sending what changed")
	n, err = m.stream(ctx, parts, since, func(r io.Reader) error { return t.SendDelta(ctx, domain, r) })
	if err != nil {
		_ = t.Discard(ctx, domain)
		return restore(fmt.Errorf("last copy: %w", err))
	}
	rep.Last = n

	progress(StepCommit, "putting it in place on "+t.Name())
	if err := t.Commit(ctx, domain, original); err != nil {
		_ = t.Discard(ctx, domain)
		return restore(fmt.Errorf("commit: %w", err))
	}

	progress(StepStart, "waiting for it to answer on "+t.Name())
	if err := m.waitReady(ctx, domain, t); err != nil {
		_ = t.Remove(ctx, domain)
		return restore(err)
	}
	rep.Down = time.Since(stopped).Round(time.Second)

	// The site answers there. Its names move now, and only then does
	// this node take its own route down: the other order would leave the
	// names pointing at nothing.
	if door, ok := m.Source.Published(domain); ok {
		progress(StepDNS, "pointing its names at "+t.Name())
		if err := t.Publish(ctx, domain, door); err != nil {
			// It runs there; the names did not follow. Said, not undone:
			// the target's own loop keeps trying, and this node keeps its
			// records until it does.
			rep.DNS = "the names were not moved: " + err.Error()
		} else {
			m.Source.Unpublish(ctx, domain)
			rep.DNS = "moved"
		}
	}

	progress(StepCleanup, "archiving the copy that stays here")
	path, err := m.Source.Archive(ctx, domain, original)
	if err != nil {
		return rep, fmt.Errorf("moved and running on %s, but the copy here could not be archived: %w", t.Name(), err)
	}
	rep.Archive = path
	progress(StepDone, fmt.Sprintf("%s runs on %s; it was down %s", domain, t.Name(), rep.Down))
	return rep, nil
}

// stream packs parts (since, for the last copy) into a pipe that send
// reads, and returns the bytes that went.
func (m *Mover) stream(ctx context.Context, parts []archive.Part, since time.Time, send func(io.Reader) error) (int64, error) {
	pr, pw := io.Pipe()
	counted := &counter{r: pr}
	done := make(chan error, 1)
	go func() {
		_, err := archive.PackPartsSince(ctx, pw, parts, since)
		pw.CloseWithError(err)
		done <- err
	}()
	err := send(counted)
	// Whatever send left unread, so the packer is not stuck writing.
	_ = pr.CloseWithError(errors.New("the target stopped reading"))
	packErr := <-done
	if packErr != nil && err == nil {
		err = packErr
	}
	return counted.n, err
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (m *Mover) waitReady(ctx context.Context, domain string, t Target) error {
	wait, poll := m.ReadyWait, m.Poll
	if wait == 0 {
		wait = 3 * time.Minute
	}
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	why := ""
	for {
		ok, w, err := t.Ready(ctx, domain)
		if err == nil && ok {
			return nil
		}
		if err != nil {
			why = err.Error()
		} else if w != "" {
			why = w
		}
		if time.Now().After(deadline) {
			if why == "" {
				why = "it did not answer"
			}
			return fmt.Errorf("it did not come up on %s in %s (%s)", t.Name(), wait, why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// NeedsOf is what domain asks of a target: its published names, and docker
// when it runs in a container.
func NeedsOf(root, domain string, src Source) Needs {
	var n Needs
	if _, ok := src.Published(domain); ok {
		n.Names = src.Names(domain)
	}
	if mf, err := manifest.Load(filepath.Join(root, "sites", domain)); err == nil && mf.Run != nil && mf.Run.Runtime == "docker" {
		n.Docker = true
	}
	return n
}

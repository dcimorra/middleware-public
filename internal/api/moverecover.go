package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
	"github.com/dcimorra/the-middleware/internal/relocate"
)

// A move this house's restart cut (an automatic update, the kernel's
// out-of-memory killer, a crash) left the site stopped here with no way
// back: the manifest it had and the tasks it held were in the move's
// memory. The move keeps a journal now (relocate.Journal), and when the
// house starts again each one is put right, as its own step of the move
// it belongs to: before the commit the site starts here again, after it
// the target is asked and the move finished or undone.

// The rhythm of a move put right: how long after the start it begins
// (the mesh comes up with the house), how long the target gets to say the
// site answers there, and how often and for how long a target that does
// not answer at all is asked again. The tests make them short.
var (
	recoverFirst = 5 * time.Second
	recoverReady = 3 * time.Minute
	recoverPoll  = 2 * time.Second
	recoverEvery = 30 * time.Second
	recoverFor   = time.Hour
)

// recoverMoves puts right every move a restart cut, each in the
// background; find is the target by the id its journal names. The jobs
// are listed as running before this returns, so nothing reads them as
// cut, nor starts another move of the same site meanwhile.
func (s *Server) recoverMoves(find func(id string) (relocate.Target, bool)) {
	for _, j := range relocate.Journals(s.opts.Root) {
		job := s.resumeMove(j)
		go s.recoverMove(j, job, find)
	}
}

// resumeMove is the move j belongs to, running again: the one on disk
// when there is one, with the step the restart cut said as failed.
func (s *Server) resumeMove(j relocate.Journal) *moveJob {
	s.moves.mu.Lock()
	defer s.moves.mu.Unlock()
	if s.moves.jobs == nil {
		s.moves.jobs = map[string]*moveJob{}
	}
	var job moveJob
	if j.ID == "" || fsx.ReadJSON(filepath.Join(s.movesDir(), j.ID+".json"), &job) != nil || job.Domain != j.Domain {
		id := j.ID
		if id == "" {
			id = time.Now().UTC().Format("20060102T150405.000") + "-" + j.Domain
		}
		job = moveJob{ID: id, Domain: j.Domain, Node: j.Target, Started: j.Started, Steps: []moveStep{}}
	}
	now := time.Now()
	if k := len(job.Steps); k > 0 && job.Steps[k-1].State == stepRunning {
		job.Steps[k-1].State, job.Steps[k-1].Error, job.Steps[k-1].Ended = stepFailed, "cut by a restart of the house", &now
	}
	job.Done, job.Ended, job.Error, job.Report = false, nil, "", nil
	p := &job
	s.moves.jobs[j.Domain] = p
	s.saveMoveLocked(p)
	return p
}

func (s *Server) recoverMove(j relocate.Journal, job *moveJob, find func(id string) (relocate.Target, bool)) {
	progress := func(step, msg string) {
		s.advance(job, step, msg)
		s.emitMove("relocate."+step, j.Domain, "", msg, map[string]any{"node": job.Node, "id": job.ID})
	}
	time.Sleep(recoverFirst)
	progress(relocate.StepRecover, "the house restarted during this move ("+j.Step+"): putting it right")
	mgr, err := s.sitesManager()
	if err != nil {
		s.finish(job, relocate.Report{}, err)
		return
	}
	mover := &relocate.Mover{Root: s.opts.Root, Source: siteSource{s: s, mgr: mgr}, ReadyWait: recoverReady, Poll: recoverPoll}
	began := time.Now()
	for {
		t, ok := find(j.Target)
		if !ok {
			t = absentTarget{name: job.Node}
		}
		ctx, cancel := context.WithTimeout(context.Background(), recoverReady+5*time.Minute)
		rep, err := mover.Recover(ctx, j, t, progress)
		cancel()
		if !errors.Is(err, relocate.ErrUnreachable) {
			s.finish(job, rep, err)
			if err != nil {
				s.emitMove("relocate.failed", j.Domain, "error", err.Error(), map[string]any{"node": job.Node, "id": job.ID})
			}
			return
		}
		if time.Since(began) > recoverFor {
			// Kept: the next start of the house asks again. Starting the
			// site here without the target's word could run it on both.
			err = fmt.Errorf("%w (the journal stays in %s: the house asks again when it next starts)", err, relocate.JournalPath(s.opts.Root, j.Domain))
			s.finish(job, rep, err)
			s.emitMove("relocate.failed", j.Domain, "error", err.Error(), map[string]any{"node": job.Node, "id": job.ID})
			return
		}
		progress(relocate.StepRecover, err.Error()+"; asked again in "+recoverEvery.String())
		time.Sleep(recoverEvery)
	}
}

// absentTarget is a node a journal names that is no longer one of the
// network's: nothing can be asked of it.
type absentTarget struct{ name string }

func (t absentTarget) err() error {
	return fmt.Errorf("%s is no longer a node of this network", t.name)
}
func (t absentTarget) Name() string                                       { return t.name }
func (t absentTarget) Send(context.Context, string, io.Reader) error      { return t.err() }
func (t absentTarget) SendDelta(context.Context, string, io.Reader) error { return t.err() }
func (t absentTarget) Commit(context.Context, string, []byte, map[string][]byte) error {
	return t.err()
}
func (t absentTarget) Discard(context.Context, string) error               { return t.err() }
func (t absentTarget) Remove(context.Context, string) error                { return t.err() }
func (t absentTarget) Check(context.Context, string, relocate.Needs) error { return t.err() }
func (t absentTarget) Ready(context.Context, string) (bool, string, error) { return false, "", t.err() }
func (t absentTarget) Publish(context.Context, string, string) error       { return t.err() }

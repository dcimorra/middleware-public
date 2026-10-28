package api

// MovesRunning says a site is moving through this node, on either side:
// a move from here (or one a restart cut, being put right), or, for one
// coming in from another node or from an import, a copy arriving, a
// commit, or an install, a data import or a repair running here. A restart
// in the middle of any of them breaks it: the jobs live in memory and a
// copy is cut. The updater asks it before it restarts the house.
func (s *Server) MovesRunning() bool {
	s.moves.mu.Lock()
	busy := s.moves.receiving > 0
	for _, j := range s.moves.jobs {
		busy = busy || !j.Done
	}
	s.moves.mu.Unlock()
	for _, p := range []*prepareJobs{&s.prepares, &s.migrates} {
		p.mu.Lock()
		for _, j := range p.jobs {
			busy = busy || !j.Done
		}
		p.mu.Unlock()
	}
	s.repairs.mu.Lock()
	for _, j := range s.repairs.jobs {
		busy = busy || !j.Done
	}
	s.repairs.mu.Unlock()
	return busy
}

// receiving counts a request of a move into this node while it runs;
// the returned func ends it.
func (s *Server) receiving() func() {
	s.moves.mu.Lock()
	s.moves.receiving++
	s.moves.mu.Unlock()
	return func() {
		s.moves.mu.Lock()
		s.moves.receiving--
		s.moves.mu.Unlock()
	}
}

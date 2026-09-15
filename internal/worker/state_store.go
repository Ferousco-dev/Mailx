package worker

import (
	"sync"

	"github.com/Ferousco-dev/mailx/internal/retry"
)

// stateStore is where retry.State lives across a job's Claim -> Release ->
// Claim cycle: an in-memory, Pool-owned map keyed by job ID, not durable
// across process restart.
type stateStore struct {
	mu     sync.Mutex
	states map[string]*retry.State
}

func newStateStore() *stateStore {
	return &stateStore{states: make(map[string]*retry.State)}
}

func (s *stateStore) get(jobID string) *retry.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[jobID]
	if !ok {
		st = &retry.State{}
		s.states[jobID] = st
	}
	return st
}

func (s *stateStore) delete(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, jobID)
}

func (s *stateStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.states)
}

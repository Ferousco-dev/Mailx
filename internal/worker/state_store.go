package worker

import (
	"sync"

	"github.com/Ferousco-dev/mailx/internal/retry"
)

// stateStore caches retry.State across a job's Claim -> Release -> Claim cycle.
// It is process-local, but OutcomeStore reconstructs it from durable attempts
// after restart or cross-process reclaim.
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

func (s *stateStore) getOrSet(jobID string, initial *retry.State) *retry.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state, ok := s.states[jobID]; ok {
		return state
	}
	if initial == nil {
		initial = &retry.State{}
	}
	s.states[jobID] = initial
	return initial
}

func (s *stateStore) lookup(jobID string) (*retry.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[jobID]
	return state, ok
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

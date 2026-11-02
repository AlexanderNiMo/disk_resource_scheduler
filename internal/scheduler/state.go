package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// StateStore reads and writes the daemon's state file.
//
// The plan lives here as well as in memory, so a restart of the daemon does
// not throw away a plan that is waiting for an approval.
type StateStore struct {
	path string
	mu   sync.Mutex
}

// NewStateStore returns a store for the state file in dir.
func NewStateStore(dir string) *StateStore {
	return &StateStore{path: filepath.Join(dir, "state.json")}
}

// Path returns the file the state is kept in.
func (s *StateStore) Path() string {
	return s.path
}

type persisted struct {
	State string `json:"state"`
	Plan  *Plan  `json:"plan,omitempty"`
}

// Save writes the state and the plan.
func (s *StateStore) Save(st State, plan *Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create the state directory: %w", err)
	}
	data, err := json.MarshalIndent(persisted{State: string(st), Plan: plan}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the state: %w", err)
	}

	// A crash in the middle of the write must not leave a file that cannot
	// be read back.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write the state file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace the state file: %w", err)
	}
	return nil
}

// Load reads the state and the plan.
//
// A missing file is not an error: the daemon has simply not planned anything
// yet.
func (s *StateStore) Load() (State, *Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return StateIdle, nil, nil
	}
	if err != nil {
		return StateIdle, nil, fmt.Errorf("read the state file: %w", err)
	}

	var stored persisted
	if err := json.Unmarshal(data, &stored); err != nil {
		return StateIdle, nil, fmt.Errorf("parse the state file: %w", err)
	}
	return State(stored.State), stored.Plan, nil
}

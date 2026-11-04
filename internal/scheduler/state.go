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
	State    string         `json:"state"`
	Plan     *Plan          `json:"plan,omitempty"`
	Rollback *rollbackState `json:"rollback,omitempty"`
}

// Save writes the state and the plan.
func (s *StateStore) Save(st State, plan *Plan, rollback ...*rollbackState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("create the state directory: %w", err)
	}
	stored := persisted{State: string(st), Plan: plan}
	if len(rollback) > 0 {
		stored.Rollback = rollback[0]
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the state: %w", err)
	}

	// A crash in the middle of the write must not leave a file that cannot
	// be read back.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write the state file: %w", err)
	}
	// The data must reach the disk before the name is changed: a power cut
	// in between would leave a state file that is there but empty, and the
	// plan waiting for an approval would be lost.
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace the state file: %w", err)
	}
	return syncDir(filepath.Dir(s.path))
}

// syncDir persists the state file's directory entry.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// Load reads the state and the plan.
//
// A missing file is not an error: the daemon has simply not planned anything
// yet.
func (s *StateStore) Load() (State, *Plan, error) {
	stored, err := s.load()
	return State(stored.State), stored.Plan, err
}

func (s *StateStore) load() (persisted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return persisted{State: string(StateIdle)}, nil
	}
	if err != nil {
		return persisted{}, fmt.Errorf("read the state file: %w", err)
	}

	var stored persisted
	if err := json.Unmarshal(data, &stored); err != nil {
		return persisted{}, fmt.Errorf("parse the state file: %w", err)
	}
	if op := stored.Rollback; op != nil {
		if stored.State != string(StateRollingBack) || op.Plan == nil || len(op.Targets) == 0 {
			return persisted{}, fmt.Errorf("invalid persisted rollback")
		}
		if op.Current && (stored.Plan == nil || stored.Plan.ID != op.Plan.ID) {
			return persisted{}, fmt.Errorf("rollback does not match the current plan")
		}
		for _, index := range op.Targets {
			if index < 0 || index >= len(op.Plan.Moves) {
				return persisted{}, fmt.Errorf("invalid rollback target %d", index)
			}
		}
	}
	return stored, nil
}

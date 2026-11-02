package config

import "sync/atomic"

// Store holds the configuration the daemon is currently running with.
//
// The config watcher swaps it under the running server, so every reader has
// to take a snapshot instead of holding a pointer.
type Store struct {
	current atomic.Pointer[Config]
}

// NewStore returns a store holding the given configuration.
func NewStore(cfg *Config) *Store {
	store := &Store{}
	store.current.Store(cfg)
	return store
}

// Load returns the configuration in force right now.
func (s *Store) Load() *Config {
	return s.current.Load()
}

// Store replaces the configuration in force.
func (s *Store) Store(cfg *Config) {
	s.current.Store(cfg)
}

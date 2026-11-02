package scheduler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// History keeps the finished plans in an append-only file.
//
// The file is a plain JSON lines file on purpose: the history is read once
// and written once, and a database would be a dependency for nothing.
type History struct {
	path string
	max  int
	mu   sync.Mutex
}

// NewHistory returns a history in dir.
func NewHistory(dir string) *History {
	return &History{path: filepath.Join(dir, "history.jsonl"), max: 50}
}

// HistoryEntry is one plan in the history.
type HistoryEntry struct {
	ID         int       `json:"id"`
	Kind       string    `json:"kind"`
	CreatedAt  time.Time `json:"created_at"`
	State      string    `json:"state"`
	MovesCount int       `json:"moves_count"`
	Bytes      int64     `json:"bytes"`
	Plan       *Plan     `json:"plan,omitempty"`
}

// Append adds a plan to the history.
func (h *History) Append(entry HistoryEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(h.path), 0o750); err != nil {
		return fmt.Errorf("create the state directory: %w", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode the history entry: %w", err)
	}

	file, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open the history file: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("write the history file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close the history file: %w", err)
	}
	return h.prune()
}

// List returns the plans in the history, oldest first.
func (h *History) List() ([]HistoryEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	lines, err := h.lines()
	if err != nil {
		return nil, err
	}
	entries := make([]HistoryEntry, 0, len(lines))
	for _, line := range lines {
		var entry HistoryEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			// A line that cannot be read is skipped: the rest of the
			// history is still worth having.
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (h *History) lines() ([][]byte, error) {
	data, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the history file: %w", err)
	}

	var lines [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// prune keeps the history from growing without end.
func (h *History) prune() error {
	lines, err := h.lines()
	if err != nil {
		return err
	}
	if len(lines) <= h.max {
		return nil
	}

	keep := lines[len(lines)-h.max:]
	var buf bytes.Buffer
	for _, line := range keep {
		buf.Write(line)
		buf.WriteByte('\n')
	}

	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write the history file: %w", err)
	}
	if err := os.Rename(tmp, h.path); err != nil {
		return fmt.Errorf("replace the history file: %w", err)
	}
	return nil
}

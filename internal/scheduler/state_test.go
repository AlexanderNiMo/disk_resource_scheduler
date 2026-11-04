package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
)

func TestThePlanSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("hello"), 0o644)

	cfg := &config.Config{
		Folders: map[string]config.Folder{
			src: folder(src, config.DirTypeSrc, 2, 50*gb),
			dst: folder(dst, config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(0)},
	}
	probe := fakeProbe{volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 1 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}

	first := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := first.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	_, plan := first.Snapshot()
	first.Stop(context.Background())

	second := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer second.Stop(context.Background())

	state, restored := second.Snapshot()
	if restored == nil {
		t.Fatal("the plan was not restored")
	}
	if restored.ID != plan.ID {
		t.Fatalf("the plan is #%d, want #%d", restored.ID, plan.ID)
	}
	if state != StatePending {
		t.Fatalf("the state is %s, want %s", state, StatePending)
	}
}

func TestTheInterruptedExecutionIsResumed(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("hello"), 0o644)
	// The copy was cut in the middle: the part of the file that the daemon
	// owns is under its own name, and the destination is still free.
	os.WriteFile(filepath.Join(dst, "movie.mkv.drs-partial"), []byte("hel"), 0o644)

	cfg := &config.Config{
		Folders: map[string]config.Folder{
			src: folder(src, config.DirTypeSrc, 2, 50*gb),
			dst: folder(dst, config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(0)},
	}
	probe := fakeProbe{volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 1 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}

	store := NewStateStore(dir)
	plan := &Plan{
		ID:        3,
		CreatedAt: time.Now(),
		Moves: []Move{{
			ID: 1, Src: src, Name: "movie.mkv", Path: filepath.Join(src, "movie.mkv"),
			Dst: dst, DstPath: filepath.Join(dst, "movie.mkv"), Size: 5,
			Type: 2, Status: StatusMoving,
		}},
	}
	if err := store.Save(StateRunning, plan); err != nil {
		t.Fatalf("save: %v", err)
	}

	sch := New(config.NewStore(cfg), testLogger(), probe, store, NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())

	waitFor(t, "the execution to be resumed", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone
	})

	body, err := os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if err != nil || string(body) != "hello" {
		t.Fatalf("the file at the destination is %q %v, want the whole file", body, err)
	}
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err == nil {
		t.Fatal("the source is still there")
	}
	// The part of the copy that was left behind is gone.
	if _, err := os.Stat(filepath.Join(dst, "movie.mkv.drs-partial")); err == nil {
		t.Fatal("the interrupted copy was left behind")
	}
}

// The copy was published under its final name and the source was not
// removed: the source is a leftover that takes up the space. The daemon
// notices it on the next start and removes it.
func TestTheLeftoverOfACopyIsRemovedOnTheNextStart(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	// The copy is in place and whole; the source is still there.
	os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dst, "movie.mkv"), []byte("hello"), 0o644)

	cfg := &config.Config{
		Folders: map[string]config.Folder{
			src: folder(src, config.DirTypeSrc, 2, 50*gb),
			dst: folder(dst, config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(0)},
	}
	probe := fakeProbe{volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 1 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}

	store := NewStateStore(dir)
	plan := &Plan{
		ID:        3,
		CreatedAt: time.Now(),
		Moves: []Move{{
			ID: 1, Src: src, Name: "movie.mkv", Path: filepath.Join(src, "movie.mkv"),
			Dst: dst, DstPath: filepath.Join(dst, "movie.mkv"), Size: 5,
			Type: 2, Status: StatusMoving,
		}},
	}
	if err := store.Save(StateRunning, plan); err != nil {
		t.Fatalf("save: %v", err)
	}

	sch := New(config.NewStore(cfg), testLogger(), probe, store, NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())

	waitFor(t, "the execution to be resumed", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone || state == StateFailed
	})

	state, p := sch.Snapshot()
	if state != StateDone {
		t.Fatalf("the state is %s, want done: %q", state, p.Moves[0].Error)
	}
	// The leftover is gone, so the space is free.
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err == nil {
		t.Fatal("the leftover is still there")
	}
	// The copy is whole.
	body, err := os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if err != nil || string(body) != "hello" {
		t.Fatalf("the file at the destination is %q %v, want the whole file", body, err)
	}
}

func TestAFinishedMoveIsNotDoneAgain(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	// The copy finished and the source is gone; only the status was not
	// saved before the crash.
	os.WriteFile(filepath.Join(dst, "movie.mkv"), []byte("hello"), 0o644)

	cfg := &config.Config{
		Folders: map[string]config.Folder{
			src: folder(src, config.DirTypeSrc, 2, 50*gb),
			dst: folder(dst, config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(0)},
	}
	probe := fakeProbe{volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 1 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}

	store := NewStateStore(dir)
	plan := &Plan{
		ID:        4,
		CreatedAt: time.Now(),
		Moves: []Move{{
			ID: 1, Src: src, Name: "movie.mkv", Path: filepath.Join(src, "movie.mkv"),
			Dst: dst, DstPath: filepath.Join(dst, "movie.mkv"), Size: 5,
			Type: 2, Status: StatusMoving,
		}},
	}
	if err := store.Save(StateRunning, plan); err != nil {
		t.Fatalf("save: %v", err)
	}

	sch := New(config.NewStore(cfg), testLogger(), probe, store, NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())

	waitFor(t, "the execution to be resumed", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone
	})

	body, err := os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if err != nil || string(body) != "hello" {
		t.Fatalf("the file at the destination is %q %v", body, err)
	}
}

func TestABrokenStateFileDoesNotStopTheDaemon(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	sch := New(config.NewStore(&config.Config{}), testLogger(), fakeProbe{},
		NewStateStore(dir), NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())

	// The broken file is not read, so nothing is resumed; the daemon scans
	// on its own and carries on.
	waitFor(t, "the first scan to finish", func() bool {
		status := sch.ScanStatus()
		return status.Scanned && !status.Running
	})
	state, _ := sch.Snapshot()
	if state == StateRunning {
		t.Fatal("a broken state file left the daemon running something")
	}
}

func TestTheHistoryKeepsTheLastPlans(t *testing.T) {
	dir := t.TempDir()
	history := NewHistory(dir)

	for i := 0; i < 60; i++ {
		entry := HistoryEntry{ID: i, Kind: "plan", CreatedAt: time.Now(), State: "done"}
		if err := history.Append(entry); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	entries, err := history.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 50 {
		t.Fatalf("the history has %d entries, want 50", len(entries))
	}
	if entries[len(entries)-1].ID != 59 {
		t.Fatalf("the last entry is #%d, want #59", entries[len(entries)-1].ID)
	}
	if entries[0].ID != 10 {
		t.Fatalf("the first entry is #%d, want #10", entries[0].ID)
	}

	body, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if lines := strings.Count(string(body), "\n"); lines != 50 {
		t.Fatalf("the file has %d lines, want 50", lines)
	}
}

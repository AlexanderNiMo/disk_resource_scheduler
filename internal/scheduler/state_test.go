package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
)

// The plan numbers continue after the history: a rollback looks the plan up
// by its number, and two plans with one number would be the wrong plan.
func TestThePlanNumbersContinueAfterTheHistory(t *testing.T) {
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
	history := NewHistory(dir)
	// An old plan sits in the history, and the state file is gone.
	if err := history.Append(HistoryEntry{ID: 7, Kind: "plan", CreatedAt: time.Now(), State: "done"}); err != nil {
		t.Fatal(err)
	}

	sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), history)
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())
	waitFor(t, "the first scan", func() bool { return sch.ScanStatus().Scanned })
	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	_, plan := sch.Snapshot()
	if plan.ID <= 7 {
		t.Fatalf("the plan is #%d, want a number after #7", plan.ID)
	}
}

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

// Equal sizes do not prove that the destination belongs to this move.
func TestRecoveryPreservesAnAmbiguousSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	// The copy is in place and whole; the source is still there.
	os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dst, "movie.mkv"), []byte("WRONG"), 0o644)

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
	if state != StateFailed {
		t.Fatalf("the state is %s, want failed: %q", state, p.Moves[0].Error)
	}
	// The original must survive even when an unrelated destination has the same size.
	body, err := os.ReadFile(filepath.Join(src, "movie.mkv"))
	if err != nil || string(body) != "hello" {
		t.Fatalf("source changed: %q, %v", body, err)
	}
	// The copy is whole.
	body, err = os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if err != nil || string(body) != "WRONG" {
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

func TestInterruptedRollbackResumesOnlySelectedMoves(t *testing.T) {
	for _, historical := range []bool{false, true} {
		for _, published := range []bool{false, true} {
			t.Run(fmt.Sprintf("history=%v/published=%v", historical, published), func(t *testing.T) {
				dir := t.TempDir()
				src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
				os.Mkdir(src, 0700)
				os.Mkdir(dst, 0700)
				cfg := &config.Config{Folders: map[string]config.Folder{
					src: folder(src, config.DirTypeSrc, 2, 1), dst: folder(dst, config.DirTypeDst, 2, 0),
				}}
				probe := fakeProbe{volumes: map[string]disk.Volume{src: {Dev: 1, Free: 100}, dst: {Dev: 2, Free: 100}}}
				store, history := NewStateStore(dir), NewHistory(dir)
				plan := &Plan{ID: 7}
				for i := 1; i <= 3; i++ {
					name := fmt.Sprintf("movie%d", i)
					p, q := filepath.Join(src, name), filepath.Join(dst, name)
					os.WriteFile(q, []byte(name), 0600)
					plan.Moves = append(plan.Moves, Move{ID: i, Src: src, Dst: dst, Path: p, DstPath: q, Name: name, Type: 2, Size: int64(len(name)), Status: StatusDone})
				}
				s := New(config.NewStore(cfg), testLogger(), probe, store, history)
				s.plan, s.st, s.scan.Scanned = plan, StateDone, true
				if historical {
					if err := history.Append(HistoryEntry{ID: 7, Kind: "plan", State: string(StateDone), Plan: plan}); err != nil {
						t.Fatal(err)
					}
					s.plan = &Plan{ID: 8, Moves: []Move{{ID: 1, Status: StatusPending}}}
					s.st = StatePending
				}
				// Stop before the worker runs, leaving the actual API's durable rollback record.
				s.cancel()
				if err := s.Rollback(7, 2); err != nil {
					t.Fatal(err)
				}
				if err := s.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
				stored, err := store.load()
				if err != nil || stored.Rollback == nil || len(stored.Rollback.Targets) != 1 || stored.Rollback.Targets[0] != 1 {
					t.Fatalf("rollback was not persisted: %+v %v", stored, err)
				}
				if published {
					if err := os.Rename(plan.Moves[1].DstPath, plan.Moves[1].Path); err != nil {
						t.Fatal(err)
					}
				}
				resumed := New(config.NewStore(cfg), testLogger(), probe, store, history)
				if err := resumed.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				defer resumed.Stop(context.Background())
				waitFor(t, "rollback history", func() bool {
					entries, _ := history.List()
					for _, entry := range entries {
						if entry.Kind == "rollback" && entry.ID == 7 {
							return true
						}
					}
					return false
				})
				if err := resumed.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
				for i, move := range plan.Moves {
					path := move.DstPath
					if i == 1 {
						path = move.Path
					}
					data, err := os.ReadFile(path)
					if err != nil || string(data) != move.Name {
						t.Fatalf("wrong restored file %s: %q %v", path, data, err)
					}
				}
				state, current := resumed.Snapshot()
				if historical && (state != StatePending || current.ID != 8) {
					t.Fatalf("current plan changed: %s %+v", state, current)
				}
				stored, err = store.load()
				if err != nil || stored.Rollback != nil {
					t.Fatalf("rollback record was not cleared: %+v %v", stored, err)
				}
			})
		}
	}
}

func TestLoadRejectsInvalidRollbackRecords(t *testing.T) {
	for _, record := range []string{
		`{"state":"rolling_back","rollback":{"targets":[0]}}`,
		`{"state":"rolling_back","rollback":{"plan":{"id":1,"moves":[]},"targets":[2]}}`,
		`{"state":"rolling_back","plan":{"id":2},"rollback":{"plan":{"id":1,"moves":[{}]},"targets":[0],"current":true}}`,
	} {
		t.Run(record, func(t *testing.T) {
			store := NewStateStore(t.TempDir())
			if err := os.WriteFile(store.Path(), []byte(record), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.load(); err == nil {
				t.Fatal("invalid recovery record was accepted")
			}
		})
	}
}

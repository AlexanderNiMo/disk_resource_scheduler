package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
)

const gb = 1 << 30

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeProbe reports the volumes a test wants.
type fakeProbe struct {
	volumes map[string]disk.Volume
}

func (p fakeProbe) VolumeOf(path string) (disk.Volume, error) {
	volume, ok := p.volumes[path]
	if !ok {
		return disk.Volume{}, os.ErrNotExist
	}
	return volume, nil
}

// folder builds a configured folder.
func folder(path string, dirType config.DirType, typ int, minFree int64) config.Folder {
	return config.Folder{Path: path, DirType: dirType, Type: typ, MinFree: config.Size(minFree)}
}

// fixture builds the configuration and the measurements a scan would work on.
func fixture(folders []config.Folder, volumes map[string]disk.Volume, entries map[string][]disk.Entry) (*config.Config, measured) {
	cfg := &config.Config{
		Folders: map[string]config.Folder{},
		Scan:    config.Scan{MinAge: config.Duration(0)},
	}
	for _, f := range folders {
		cfg.Folders[f.Path] = f
	}
	m := measured{
		volumes: map[string]disk.Volume{},
		entries: map[string][]disk.Entry{},
		errors:  map[string]string{},
	}
	for path, volume := range volumes {
		m.volumes[path] = volume
	}
	for path, list := range entries {
		m.entries[path] = list
	}
	return cfg, m
}

func TestBuildPlanPicksTheFreestDestination(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src", config.DirTypeSrc, 2, 50*gb),
			folder("/dst1", config.DirTypeDst, 2, 0),
			folder("/dst2", config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src":  {Dev: 1, Free: 10 * gb},
			"/dst1": {Dev: 2, Free: 100 * gb},
			"/dst2": {Dev: 3, Free: 200 * gb},
		},
		map[string][]disk.Entry{
			"/src": {{Name: "Show.S01", Path: "/src/Show.S01", Size: 5 * gb}},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 1 {
		t.Fatalf("got %d moves, want 1: %+v", len(plan.Moves), plan.Moves)
	}
	if plan.Moves[0].Dst != "/dst2" {
		t.Fatalf("the move goes to %q, want the freest destination /dst2", plan.Moves[0].Dst)
	}
}

func TestBuildPlanSkipsUnmovableEntries(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src", config.DirTypeSrc, 2, 50*gb),
			folder("/dst", config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src": {Dev: 1, Free: 10 * gb},
			"/dst": {Dev: 2, Free: 1 * gb},
		},
		map[string][]disk.Entry{
			"/src": {{Name: "Show.S01", Path: "/src/Show.S01", Size: 5 * gb}},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 0 {
		t.Fatalf("got %d moves, want none: %+v", len(plan.Moves), plan.Moves)
	}
	if len(plan.Unmovable) != 1 {
		t.Fatalf("got %d unmovable entries, want 1", len(plan.Unmovable))
	}
}

func TestBuildPlanStopsWhenTheDeficitIsMet(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src", config.DirTypeSrc, 2, 50*gb),
			folder("/dst", config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src": {Dev: 1, Free: 40 * gb},
			"/dst": {Dev: 2, Free: 500 * gb},
		},
		map[string][]disk.Entry{
			"/src": {
				{Name: "big", Path: "/src/big", Size: 30 * gb},
				{Name: "small", Path: "/src/small", Size: 5 * gb},
			},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 1 || plan.Moves[0].Name != "big" {
		t.Fatalf("got %+v, want only the big entry", plan.Moves)
	}
}

func TestBuildPlanOrdersLargestFirst(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src", config.DirTypeSrc, 2, 50*gb),
			folder("/dst", config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src": {Dev: 1, Free: 10 * gb},
			"/dst": {Dev: 2, Free: 500 * gb},
		},
		map[string][]disk.Entry{
			"/src": {
				{Name: "small", Path: "/src/small", Size: 10 * gb},
				{Name: "big", Path: "/src/big", Size: 30 * gb},
				{Name: "medium", Path: "/src/medium", Size: 20 * gb},
			},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 2 {
		t.Fatalf("got %d moves, want 2: %+v", len(plan.Moves), plan.Moves)
	}
	if plan.Moves[0].Name != "big" || plan.Moves[1].Name != "medium" {
		t.Fatalf("the order is %s, %s, want big, medium", plan.Moves[0].Name, plan.Moves[1].Name)
	}
}

func TestBuildPlanIgnoresASameVolumeDestination(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src", config.DirTypeSrc, 2, 50*gb),
			folder("/dst", config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src": {Dev: 1, Free: 10 * gb},
			"/dst": {Dev: 1, Free: 500 * gb},
		},
		map[string][]disk.Entry{
			"/src": {{Name: "Show.S01", Path: "/src/Show.S01", Size: 5 * gb}},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 0 {
		t.Fatalf("got %d moves, want none: a move inside one volume frees nothing", len(plan.Moves))
	}
}

func TestBuildPlanRequiresTheSameType(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src", config.DirTypeSrc, 2, 50*gb),
			folder("/films", config.DirTypeDst, 1, 0),
			folder("/serials", config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src":     {Dev: 1, Free: 10 * gb},
			"/films":   {Dev: 2, Free: 100 * gb},
			"/serials": {Dev: 3, Free: 500 * gb},
		},
		map[string][]disk.Entry{
			"/src": {{Name: "Show.S01", Path: "/src/Show.S01", Size: 5 * gb}},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 1 || plan.Moves[0].Dst != "/serials" {
		t.Fatalf("got %+v, want the destination of the same type", plan.Moves)
	}
}

func TestBuildPlanSharesFreeSpaceBetweenSources(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src/serials", config.DirTypeSrc, 2, 50*gb),
			folder("/src/films", config.DirTypeSrc, 1, 50*gb),
			folder("/dst/serials", config.DirTypeDst, 2, 0),
			folder("/dst/films", config.DirTypeDst, 1, 0),
		},
		map[string]disk.Volume{
			"/src/serials": {Dev: 1, Free: 10 * gb},
			"/src/films":   {Dev: 1, Free: 10 * gb},
			"/dst/serials": {Dev: 2, Free: 500 * gb},
			"/dst/films":   {Dev: 3, Free: 500 * gb},
		},
		map[string][]disk.Entry{
			"/src/serials": {{Name: "Show.S01", Path: "/src/serials/Show.S01", Size: 20 * gb}},
			"/src/films":   {{Name: "Film.mkv", Path: "/src/films/Film.mkv", Size: 20 * gb}},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	// The first source frees 20 GB on the shared volume, which leaves the
	// second one 10 GB short again, so it moves its entry as well.
	if len(plan.Moves) != 2 {
		t.Fatalf("got %d moves, want 2: %+v", len(plan.Moves), plan.Moves)
	}
}

func TestBuildPlanSkipsRecentlyChangedEntries(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src", config.DirTypeSrc, 2, 50*gb),
			folder("/dst", config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src": {Dev: 1, Free: 10 * gb},
			"/dst": {Dev: 2, Free: 500 * gb},
		},
		map[string][]disk.Entry{
			"/src": {
				{Name: "fresh", Path: "/src/fresh", Size: 20 * gb, ModTime: time.Now()},
				{Name: "old", Path: "/src/old", Size: 20 * gb, ModTime: time.Now().Add(-48 * time.Hour)},
			},
		},
	)
	cfg.Scan.MinAge = config.Duration(24 * time.Hour)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 1 || plan.Moves[0].Name != "old" {
		t.Fatalf("got %+v, want only the entry that has not changed for a day", plan.Moves)
	}
	if len(plan.Unmovable) != 1 || plan.Unmovable[0].Name != "fresh" {
		t.Fatalf("got %+v, want the fresh entry to be reported as unmovable", plan.Unmovable)
	}
}

// newTestScheduler builds a scheduler over a temporary state directory.
func newTestScheduler(t *testing.T, cfg *config.Config, probe disk.Probe) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { sch.Stop(context.Background()) })
	return sch
}

// waitFor polls until the condition holds or the time runs out.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAnEmptyPlanIsNotSaved(t *testing.T) {
	dir := t.TempDir()
	store := NewStateStore(dir)
	cfg := &config.Config{
		Folders: map[string]config.Folder{
			"/src": folder("/src", config.DirTypeSrc, 2, 50*gb),
			"/dst": folder("/dst", config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(0)},
	}
	// The source has enough free space, so there is nothing to move.
	probe := fakeProbe{volumes: map[string]disk.Volume{
		"/src": {Dev: 1, Free: 500 * gb},
		"/dst": {Dev: 2, Free: 500 * gb},
	}}

	sch := New(config.NewStore(cfg), testLogger(), probe, store, NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())

	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the empty plan was saved: %v", err)
	}
	// The plan is still there to be shown in the interface.
	state, plan := sch.Snapshot()
	if plan == nil || len(plan.Moves) != 0 {
		t.Fatalf("the plan is %+v, want an empty one", plan)
	}
	if state != StatePending {
		t.Fatalf("the state is %s, want %s", state, StatePending)
	}
}

func TestApproveExecutesTheMoves(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

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
	sch := newTestScheduler(t, cfg, probe)

	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	state, plan := sch.Snapshot()
	if state != StatePending || plan == nil || len(plan.Moves) != 1 {
		t.Fatalf("the plan is not ready: %s %+v", state, plan)
	}

	if err := sch.Approve(plan.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, "the move to finish", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone
	})

	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err == nil {
		t.Fatal("the file is still at the source")
	}
	body, err := os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if err != nil || string(body) != "hello" {
		t.Fatalf("the file is not at the destination: %q %v", body, err)
	}
}

func TestApproveRefusesAStalePlan(t *testing.T) {
	sch := newTestScheduler(t, &config.Config{}, fakeProbe{})
	if err := sch.Approve(7); err != ErrStalePlan && err != ErrNoPlan {
		t.Fatalf("approve returned %v, want a stale plan or no plan", err)
	}
}

func TestRejectDiscardsThePlan(t *testing.T) {
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
	sch := newTestScheduler(t, cfg, probe)

	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	_, plan := sch.Snapshot()
	if err := sch.Reject(plan.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	state, plan := sch.Snapshot()
	if state != StateIdle || plan != nil {
		t.Fatalf("the plan is still there: %s %+v", state, plan)
	}
}

func TestRollbackPutsOneMoveBack(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(src, "one.mkv"), []byte("one"), 0o644)
	os.WriteFile(filepath.Join(src, "two.mkv"), []byte("two"), 0o644)

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
	sch := newTestScheduler(t, cfg, probe)

	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	_, plan := sch.Snapshot()
	if err := sch.Approve(plan.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, "the moves to finish", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone
	})

	_, plan = sch.Snapshot()
	if err := sch.Rollback(plan.ID, plan.Moves[0].ID); err != nil {
		t.Fatalf("roll back one move: %v", err)
	}
	waitFor(t, "the rollback to finish", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone
	})

	// The file that was rolled back is at the source again, the other one is
	// still at the destination.
	_, plan = sch.Snapshot()
	rolledBack := plan.Moves[0]
	if _, err := os.Stat(rolledBack.Path); err != nil {
		t.Fatalf("%q was not put back: %v", rolledBack.Path, err)
	}
	other := plan.Moves[1]
	if _, err := os.Stat(other.DstPath); err != nil {
		t.Fatalf("%q was moved as well: %v", other.DstPath, err)
	}
	if _, err := os.Stat(other.Path); err == nil {
		t.Fatalf("%q was put back as well", other.Path)
	}
}

func TestMoveIsRetriedAfterAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	pause := retryPause
	retryPause = time.Millisecond
	defer func() { retryPause = pause }()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("hello"), 0o644)
	// The destination cannot be written into, so every attempt fails.
	if err := os.Chmod(dst, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dst, 0o700)

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
	sch := newTestScheduler(t, cfg, probe)

	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	_, plan := sch.Snapshot()
	if err := sch.Approve(plan.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, "the move to fail", func() bool {
		state, _ := sch.Snapshot()
		return state == StateFailed
	})

	_, plan = sch.Snapshot()
	if plan.Moves[0].Attempt != moveAttempts {
		t.Fatalf("the move was tried %d times, want %d", plan.Moves[0].Attempt, moveAttempts)
	}
	if plan.Moves[0].Error == "" {
		t.Fatal("the failure was not recorded")
	}
	// The source is still there: a failed move changes nothing.
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
		t.Fatalf("the source is gone: %v", err)
	}
}

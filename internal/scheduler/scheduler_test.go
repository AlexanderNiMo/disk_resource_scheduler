package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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

func TestBuildPlanSkipsTheEntriesThatCannotBeMoved(t *testing.T) {
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
}

// A symlink is not media: moving it frees no space, and its target would
// break on another disk.
func TestBuildPlanSkipsSymlinks(t *testing.T) {
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
				{Name: "link.mkv", Path: "/src/link.mkv", Size: 12, IsSymlink: true},
				{Name: "movie.mkv", Path: "/src/movie.mkv", Size: 20 * gb},
			},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 1 || plan.Moves[0].Name != "movie.mkv" {
		t.Fatalf("got %+v, want only the file", plan.Moves)
	}
	// The size of the symlink is not counted as freed space either.
	if plan.Moves[0].Size != 20*gb {
		t.Fatalf("the move is %d bytes, want %d", plan.Moves[0].Size, 20*gb)
	}
}

// Two entries of the same name from two sources cannot both go to the same
// destination: the second one would find the place taken.
func TestBuildPlanGivesEachMoveItsOwnPlace(t *testing.T) {
	cfg, m := fixture(
		[]config.Folder{
			folder("/src-a", config.DirTypeSrc, 2, 50*gb),
			folder("/src-b", config.DirTypeSrc, 2, 50*gb),
			folder("/dst", config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src-a": {Dev: 1, Free: 10 * gb},
			"/src-b": {Dev: 2, Free: 10 * gb},
			"/dst":   {Dev: 3, Free: 500 * gb},
		},
		map[string][]disk.Entry{
			"/src-a": {{Name: "Show.S01", Path: "/src-a/Show.S01", Size: 20 * gb}},
			"/src-b": {{Name: "Show.S01", Path: "/src-b/Show.S01", Size: 20 * gb}},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	places := map[string]bool{}
	for _, move := range plan.Moves {
		if places[move.DstPath] {
			t.Fatalf("two moves share the place %q: %+v", move.DstPath, plan.Moves)
		}
		places[move.DstPath] = true
	}
}

// A name that is taken at the freest destination does not stop the entry:
// another destination of the same type can take it.
func TestBuildPlanTriesAnotherDestinationWhenTheNameIsTaken(t *testing.T) {
	root := t.TempDir()
	freest := filepath.Join(root, "dst-freest")
	other := filepath.Join(root, "dst-other")
	if err := os.MkdirAll(freest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(freest, "Show.S01"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, m := fixture(
		[]config.Folder{
			folder("/src", config.DirTypeSrc, 2, 50*gb),
			folder(freest, config.DirTypeDst, 2, 0),
			folder(other, config.DirTypeDst, 2, 0),
		},
		map[string]disk.Volume{
			"/src": {Dev: 1, Free: 10 * gb},
			freest: {Dev: 2, Free: 900 * gb},
			other:  {Dev: 3, Free: 500 * gb},
		},
		map[string][]disk.Entry{
			"/src": {{Name: "Show.S01", Path: "/src/Show.S01", Size: 20 * gb}},
		},
	)

	plan := buildPlan(cfg, m, time.Now(), 1)
	if len(plan.Moves) != 1 {
		t.Fatalf("got %+v, want the move to the other destination", plan.Moves)
	}
	if plan.Moves[0].Dst != other {
		t.Fatalf("the move goes to %q, want the one without the file: %q", plan.Moves[0].Dst, other)
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
	// The daemon scans as soon as it starts; the tests should not race it.
	waitFor(t, "the first scan to finish", func() bool {
		status := sch.ScanStatus()
		return status.Scanned && !status.Running
	})
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
	// The state is idle, not pending: a plan that waits for an approval
	// stops the next scan, and the daemon would never measure the disks
	// again.
	if state != StateIdle {
		t.Fatalf("the state is %s, want %s", state, StateIdle)
	}
}

func TestTheDaemonKeepsScanningAfterAnEmptyPlan(t *testing.T) {
	sch := newTestScheduler(t, &config.Config{
		Folders: map[string]config.Folder{
			"/src": folder("/src", config.DirTypeSrc, 2, 50*gb),
			"/dst": folder("/dst", config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(0)},
	}, fakeProbe{volumes: map[string]disk.Volume{
		"/src": {Dev: 1, Free: 500 * gb},
		"/dst": {Dev: 2, Free: 500 * gb},
	}})

	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("the first scan: %v", err)
	}
	_, first := sch.Snapshot()

	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("the second scan: %v", err)
	}
	_, second := sch.Snapshot()

	if second.ID == first.ID {
		t.Fatalf("the second scan did nothing: the plan id is still %d", second.ID)
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

// The state file holds the whole plan, so writing it after every move would
// rewrite hundreds of kilobytes for every move.
func TestTheStateFileIsNotWrittenAfterEveryMove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	const moves = 40
	for i := 0; i < moves; i++ {
		name := fmt.Sprintf("m%02d.mkv", i)
		os.WriteFile(filepath.Join(src, name), []byte("hello"), 0o644)
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
	store := NewStateStore(dir)
	sch := New(config.NewStore(cfg), testLogger(), probe, store, NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())
	waitFor(t, "the first scan", func() bool { return sch.ScanStatus().Scanned })
	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	_, plan := sch.Snapshot()
	if len(plan.Moves) != moves {
		t.Fatalf("the plan has %d moves, want %d", len(plan.Moves), moves)
	}

	// Count the writes: the file grows and shrinks, and every write is a new
	// copy of the whole plan.
	writes := 0
	var lastSize int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if info, err := os.Stat(store.Path()); err == nil {
				if info.Size() != lastSize {
					lastSize = info.Size()
					writes++
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	if err := sch.Approve(plan.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, "the moves to finish", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone || state == StateFailed
	})
	close(stop)
	<-done

	// The whole plan is written every hundred moves, so a plan of forty is
	// written once and not forty times.
	if writes > 3 {
		t.Fatalf("the state file was written %d times for %d moves", writes, moves)
	}
}

func TestApproveRefusesAStalePlan(t *testing.T) {
	sch := newTestScheduler(t, &config.Config{}, fakeProbe{})
	if err := sch.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := sch.Approve(7); err != ErrStalePlan && err != ErrNoPlan {
		t.Fatalf("approve returned %v, want a stale plan or no plan", err)
	}
}

// A plan that is already done is not approved again: the history would get
// a second entry for the same plan.
func TestApprovingAFinishedPlanIsRefused(t *testing.T) {
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
	if err := sch.Approve(plan.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, "the moves to finish", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone
	})

	// The plan is done. Approving it again must not write a second entry
	// into the history.
	if err := sch.Approve(plan.ID); !errors.Is(err, ErrStalePlan) {
		t.Fatalf("the second approval returned %v, want %v", err, ErrStalePlan)
	}
	entries, err := history.List()
	if err != nil {
		t.Fatalf("the history: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the history has %d entries, want 1", len(entries))
	}
}

func TestThereIsNoPlanBeforeTheFirstScan(t *testing.T) {
	// The scheduler is not started, so no scan has happened yet.
	sch := New(config.NewStore(&config.Config{}), testLogger(), fakeProbe{},
		NewStateStore(t.TempDir()), NewHistory(t.TempDir()))
	if err := sch.Approve(1); err != ErrNotScanned {
		t.Fatalf("approve returned %v, want %v", err, ErrNotScanned)
	}
	if err := sch.Reject(1); err != ErrNotScanned {
		t.Fatalf("reject returned %v, want %v", err, ErrNotScanned)
	}
	if err := sch.Rollback(1, 0); err != ErrNotScanned {
		t.Fatalf("rollback returned %v, want %v", err, ErrNotScanned)
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

// A rejected plan moved nothing, and the history says so.
func TestARejectedPlanReportsNoBytes(t *testing.T) {
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
	if err := sch.Reject(plan.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}

	entries, err := history.List()
	if err != nil {
		t.Fatalf("the history: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the history has %d entries, want 1", len(entries))
	}
	if entries[0].Bytes != 0 || entries[0].MovesCount != 0 {
		t.Fatalf("the rejected plan reports %d bytes and %d moves, want none",
			entries[0].Bytes, entries[0].MovesCount)
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

// A file that was written again after the plan was made is not moved: it
// would be moved half-written and the original would be deleted.
func TestAMoveThatWasWrittenAgainIsNotMoved(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	path := filepath.Join(src, "movie.mkv")
	os.WriteFile(path, []byte("hello"), 0o644)
	// The file was old enough to be planned.
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(path, old, old)

	cfg := &config.Config{
		Folders: map[string]config.Folder{
			src: folder(src, config.DirTypeSrc, 2, 50*gb),
			dst: folder(dst, config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(24 * time.Hour)},
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
	if len(plan.Moves) != 1 {
		t.Fatalf("the plan has %+v, want the old file", plan.Moves)
	}

	// The download starts over: the file is written again.
	os.WriteFile(path, []byte("HALF"), 0o644)

	if err := sch.Approve(plan.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, "the move to finish", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone || state == StateFailed
	})

	_, plan = sch.Snapshot()
	if plan.Moves[0].Status != StatusFailed {
		t.Fatalf("the move is %q, want it to be refused", plan.Moves[0].Status)
	}
	// The source is still there, and the destination is empty.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the source is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "movie.mkv")); err == nil {
		t.Fatal("the file was moved anyway")
	}
}

func TestAMoveNeverTouchesAFileItDidNotCreate(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("ours"), 0o644)

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

	// Somebody puts a file of the same name in the destination after the
	// plan was made.
	os.WriteFile(filepath.Join(dst, "movie.mkv"), []byte("not ours"), 0o644)

	if err := sch.Approve(plan.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, "the move to fail", func() bool {
		state, _ := sch.Snapshot()
		return state == StateFailed
	})

	body, err := os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if err != nil || string(body) != "not ours" {
		t.Fatalf("the file in the destination is %q %v, want it untouched", body, err)
	}
	// And the source is still there: a failed move changes nothing.
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
		t.Fatalf("the source is gone: %v", err)
	}
}

// slowProbe measures the volumes slowly, so that a test can change the plan
// while a scan is running.
type slowProbe struct {
	mu      sync.Mutex
	delay   time.Duration
	volumes map[string]disk.Volume
}

func (p *slowProbe) setDelay(d time.Duration) {
	p.mu.Lock()
	p.delay = d
	p.mu.Unlock()
}

func (p *slowProbe) VolumeOf(path string) (disk.Volume, error) {
	p.mu.Lock()
	delay := p.delay
	p.mu.Unlock()
	time.Sleep(delay)

	volume, ok := p.volumes[path]
	if !ok {
		return disk.Volume{}, os.ErrNotExist
	}
	return volume, nil
}

func TestAScanIsThrownAwayWhenThePlanChangesUnderIt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(src, "movie.mkv"), []byte("hello"), 0o644)

	probe := &slowProbe{volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 1 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}
	cfg := &config.Config{
		Folders: map[string]config.Folder{
			src: folder(src, config.DirTypeSrc, 2, 50*gb),
			dst: folder(dst, config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(0)},
	}
	sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())

	waitFor(t, "the first scan", func() bool { return sch.ScanStatus().Scanned })
	_, plan := sch.Snapshot()

	if err := sch.Approve(plan.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, "the execution", func() bool {
		state, _ := sch.Snapshot()
		return state == StateDone
	})

	// A slow scan starts, and the plan is rolled back while it runs: the
	// measurement it took is stale, and it must not be published.
	probe.setDelay(200 * time.Millisecond)
	go sch.Scan(context.Background())
	time.Sleep(50 * time.Millisecond)
	if err := sch.Rollback(plan.ID, 0); err != nil {
		t.Fatalf("the rollback: %v", err)
	}

	waitFor(t, "the scan to finish", func() bool { return !sch.ScanStatus().Running })
	_, after := sch.Snapshot()
	if after == nil || after.ID != plan.ID {
		t.Fatalf("the stale scan published a plan: %+v", after)
	}
}

func TestTheLeftoversOfACopyAreRemovedAtTheStart(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	// The leftover of a copy that was cut in the middle, and a file that
	// has nothing to do with the daemon.
	os.WriteFile(filepath.Join(dst, "movie.mkv.drs-partial"), []byte("half"), 0o644)
	os.WriteFile(filepath.Join(src, "somebody-elses.mkv"), []byte("not ours"), 0o644)

	cfg := &config.Config{
		Folders: map[string]config.Folder{
			src: folder(src, config.DirTypeSrc, 2, 50*gb),
			dst: folder(dst, config.DirTypeDst, 2, 0),
		},
		Scan: config.Scan{MinAge: config.Duration(0)},
	}
	probe := fakeProbe{volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 500 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}

	sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(t.TempDir()), NewHistory(t.TempDir()))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())

	if _, err := os.Stat(filepath.Join(dst, "movie.mkv.drs-partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover of the copy is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "somebody-elses.mkv")); err != nil {
		t.Fatalf("a file that is not the daemon's was removed: %v", err)
	}
}

// slowProbe measures the volumes slowly, so that a test can stop the daemon
// while a scan is walking.
type slowVolumeProbe struct {
	delay   time.Duration
	volumes map[string]disk.Volume
}

func (p slowVolumeProbe) VolumeOf(path string) (disk.Volume, error) {
	time.Sleep(p.delay)
	volume, ok := p.volumes[path]
	if !ok {
		return disk.Volume{}, os.ErrNotExist
	}
	return volume, nil
}

// A scan that is cancelled publishes nothing: the folders it did not reach
// would look like full disks.
func TestACancelledScanPublishesNoPlan(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a-src")
	dst := filepath.Join(dir, "b-dst")
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
	probe := slowVolumeProbe{delay: 300 * time.Millisecond, volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 1 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}

	sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	if err := sch.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the scan returned %v, want %v", err, context.Canceled)
	}
	state, plan := sch.Snapshot()
	if state == StatePending && plan != nil && len(plan.Moves) > 0 {
		t.Fatalf("the cancelled scan published a plan: %+v", plan.Moves)
	}
}

// The daemon waits for the scan it started, and the scan is cancelled.
func TestTheStopWaitsForTheScan(t *testing.T) {
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
	probe := slowVolumeProbe{delay: 300 * time.Millisecond, volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 1 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}

	sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "the scan to start", func() bool { return sch.ScanStatus().Running })

	if err := sch.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if sch.ScanStatus().Running {
		t.Fatal("the scan was still running after the stop")
	}
}

// A second scan is refused while one is running: the library is walked once,
// not once per click.
func TestASecondScanIsRefused(t *testing.T) {
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
	probe := slowVolumeProbe{delay: 300 * time.Millisecond, volumes: map[string]disk.Volume{
		src: {Dev: 1, Free: 1 * gb},
		dst: {Dev: 2, Free: 500 * gb},
	}}

	sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
	if err := sch.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop(context.Background())
	waitFor(t, "the scan to start", func() bool { return sch.ScanStatus().Running })

	if err := sch.StartScan(); !errors.Is(err, ErrScanRunning) {
		t.Fatalf("the second scan returned %v, want %v", err, ErrScanRunning)
	}
}

// A stop and an approval at the same time must not upset the runtime.
func TestTheStopAndTheApprovalDoNotCollide(t *testing.T) {
	for i := 0; i < 50; i++ {
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
		sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
		if err := sch.Start(context.Background()); err != nil {
			t.Fatalf("start: %v", err)
		}
		waitFor(t, "the scan", func() bool { return sch.ScanStatus().Scanned })
		_, plan := sch.Snapshot()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); sch.Stop(context.Background()) }()
		go func() { defer wg.Done(); sch.Approve(plan.ID) }()
		wg.Wait()
	}
}

// A rollback and an approval at the same time must not move the same file
// twice and must not upset the runtime.
//
// The rollback reads the statuses of the moves of the plan while the
// executor writes them: the race detector finds it when both run at once.
func TestTheRollbackAndTheApprovalDoNotCollide(t *testing.T) {
	for i := 0; i < 50; i++ {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		dst := filepath.Join(dir, "dst")
		os.MkdirAll(src, 0o755)
		os.MkdirAll(dst, 0o755)
		for j := 0; j < 20; j++ {
			name := fmt.Sprintf("m%02d.mkv", j)
			if err := os.WriteFile(filepath.Join(src, name), []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
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
		sch := New(config.NewStore(cfg), testLogger(), probe, NewStateStore(dir), NewHistory(dir))
		if err := sch.Start(context.Background()); err != nil {
			t.Fatalf("start: %v", err)
		}
		waitFor(t, "the scan", func() bool { return sch.ScanStatus().Scanned })
		_, plan := sch.Snapshot()

		// The plan waits for an approval. The rollback and the approval
		// arrive at the same time: the rollback reads the statuses of the
		// moves while the executor writes them.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); sch.Rollback(plan.ID, 0) }()
		go func() { defer wg.Done(); sch.Approve(plan.ID) }()
		wg.Wait()
		time.Sleep(20 * time.Millisecond)
		sch.Stop(context.Background())
	}
}

func TestRemoveExcludesOnlyTheSelectedMoveAndPersistsThePlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStateStore(t.TempDir())
	s := &Scheduler{
		st: StatePending, scan: ScanStatus{Scanned: true}, store: store,
		plan: &Plan{ID: 7, Moves: []Move{
			{ID: 1, Path: path, Status: StatusPending},
			{ID: 2, Status: StatusPending},
		}},
	}
	if err := s.Remove(7, 1); err != nil {
		t.Fatal(err)
	}
	state, plan, err := store.Load()
	if err != nil || state != StatePending || len(plan.Moves) != 1 || plan.Moves[0].ID != 2 {
		t.Fatalf("persisted plan: state=%s plan=%+v err=%v", state, plan, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
		t.Fatalf("excluded file changed: %q, %v", data, err)
	}
	if err := s.Remove(7, 2); err != nil {
		t.Fatal(err)
	}
	state, plan, err = store.Load()
	if err != nil || state != StateIdle || len(plan.Moves) != 0 {
		t.Fatalf("empty plan: state=%s plan=%+v err=%v", state, plan, err)
	}
}

func TestRemoveRefusesAnInvalidOrActivePlan(t *testing.T) {
	for _, tc := range []struct {
		name           string
		state          State
		scanned        bool
		planID, moveID int
		want           error
	}{
		{"not scanned", StatePending, false, 7, 1, ErrNotScanned},
		{"stale", StatePending, true, 6, 1, ErrStalePlan},
		{"running", StateRunning, true, 7, 1, ErrBusy},
		{"rolling back", StateRollingBack, true, 7, 1, ErrBusy},
		{"done", StateDone, true, 7, 1, ErrStalePlan},
		{"unknown move", StatePending, true, 7, 9, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{st: tc.state, scan: ScanStatus{Scanned: tc.scanned}, plan: &Plan{ID: 7, Moves: []Move{{ID: 1}}}}
			err := s.Remove(tc.planID, tc.moveID)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if len(s.plan.Moves) != 1 || s.st != tc.state {
				t.Fatal("refused removal changed the plan")
			}
		})
	}
}

func TestRejectRefusesAnActiveOrSettledPlan(t *testing.T) {
	for _, state := range []State{StateRunning, StateRollingBack, StateDone, StateFailed, StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			s := &Scheduler{st: state, scan: ScanStatus{Scanned: true}, plan: &Plan{ID: 1}}
			if err := s.Reject(1); err == nil {
				t.Fatal("rejected a plan that was not pending")
			}
			if s.st != state || s.plan == nil {
				t.Fatal("rejection changed the plan")
			}
		})
	}
}

func TestOperationsDoNotStartWhenStateCannotBeSaved(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprint(rollback), func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
			os.WriteFile(src, []byte("source"), 0o600)
			os.WriteFile(dst, []byte("destination"), 0o600)
			blocker := filepath.Join(dir, "not-a-directory")
			os.WriteFile(blocker, nil, 0o600)
			state, status := StatePending, StatusPending
			if rollback {
				state, status = StateDone, StatusDone
			}
			s := New(config.NewStore(&config.Config{}), testLogger(), nil, NewStateStore(blocker), NewHistory(dir))
			s.st, s.scan.Scanned = state, true
			s.plan = &Plan{ID: 1, Moves: []Move{{ID: 1, Path: src, DstPath: dst, Status: status}}}
			var err error
			if rollback {
				err = s.Rollback(1, 1)
			} else {
				err = s.Approve(1)
			}
			if err == nil {
				t.Fatal("operation started without durable state")
			}
			if s.st != state || s.activeRollback != nil {
				t.Fatal("failed operation changed state")
			}
			if err := s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]string{src: "source", dst: "destination"} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Fatalf("file changed: %q %v", data, err)
				}
			}
		})
	}
}

func TestChangedRecentlyChecksFilesInsideADirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie")
	os.WriteFile(path, []byte("old"), 0o600)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(dir, old, old)
	os.Chtimes(path, old, old)
	s := &Scheduler{cfg: config.NewStore(&config.Config{Scan: config.Scan{MinAge: config.Duration(24 * time.Hour)}})}
	if s.changedRecently(dir) {
		t.Fatal("old tree was refused")
	}
	os.WriteFile(path, []byte("new download"), 0o600)
	if !s.changedRecently(dir) {
		t.Fatal("active file inside old directory was missed")
	}
}

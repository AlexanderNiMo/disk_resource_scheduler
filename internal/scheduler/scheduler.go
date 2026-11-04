// Package scheduler decides what moves where and performs the moves.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
	"github.com/alex/disk_resource_scheduler/internal/format"
)

// State is what the scheduler is doing right now.
type State string

const (
	StateIdle        State = "idle"
	StatePending     State = "pending"
	StateRunning     State = "running"
	StateRollingBack State = "rolling_back"
	StateDone        State = "done"
	StateFailed      State = "failed"
)

// The status of a single move.
const (
	StatusPending    = "pending"
	StatusMoving     = "moving"
	StatusDone       = "done"
	StatusFailed     = "failed"
	StatusRolledBack = "rolled_back"
)

const (
	moveAttempts = 3

	// progressStep is how much has to be copied before the interface is told
	// about it: every chunk would be a lock and a redraw.
	progressStep = 4 << 20

	// saveEveryMoves is how many changes of a move go into the state file
	// before it is written again. The file holds the whole plan, so writing
	// it after every move would rewrite hundreds of kilobytes per move.
	saveEveryMoves = 100
)

// retryPause is how long to wait between two attempts at the same move.
var retryPause = 2 * time.Second

var (
	// ErrNoPlan means there is nothing to approve.
	ErrNoPlan = errors.New("there is no plan")
	// ErrStalePlan means the plan in the interface is not the current one.
	ErrStalePlan = errors.New("the plan has changed")
	// ErrBusy means an execution is already running.
	ErrBusy = errors.New("something is already running")
	// ErrNotScanned means the folders have not been measured yet: there is
	// nothing to make a plan of.
	ErrNotScanned = errors.New("the folders have not been scanned yet")
	// ErrNothingToApprove means the plan has no moves in it.
	ErrNothingToApprove = errors.New("there is nothing to approve")
	// ErrScanRunning means a scan is already in flight.
	ErrScanRunning = errors.New("a scan is already running")
	// ErrStopping means the daemon is going away.
	ErrStopping = errors.New("the daemon is stopping")
)

// Plan is what the daemon proposes to do.
type Plan struct {
	ID        int            `json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	Folders   []FolderStatus `json:"folders"`
	Moves     []Move         `json:"moves"`
}

// FolderStatus is what the scan found on one configured folder.
type FolderStatus struct {
	Path    string `json:"path"`
	DirType string `json:"dir_type"`
	Type    int    `json:"type"`
	Free    int64  `json:"free"`
	Total   int64  `json:"total"`
	MinFree int64  `json:"min_free"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

// Move is one entry that changes its place.
type Move struct {
	ID      int    `json:"id"`
	Src     string `json:"src"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Dst     string `json:"dst"`
	DstPath string `json:"dst_path"`
	Size    int64  `json:"size"`
	Type    int    `json:"type"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
	Copied  int64  `json:"copied"`
	Error   string `json:"error,omitempty"`
}

// ScanStatus is what the scan is doing right now.
//
// The interface shows it: the walk of a big library takes a while, and a
// page that says nothing about it looks broken.
type ScanStatus struct {
	Running bool
	// Scanned tells whether the folders have been measured at least once.
	// There is no plan before that.
	Scanned bool
	Started time.Time
	// Folder is the folder being walked right now.
	Folder string
	// Entries is the number of entries counted in that folder so far.
	Entries int
	// Measured is the number of folders that have been measured.
	Measured int
	// Total is the number of folders in the configuration.
	Total int
}

type rollbackState struct {
	Plan     *Plan `json:"plan"`
	Targets  []int `json:"targets"`
	Previous State `json:"previous"`
	Current  bool  `json:"current"`
}

// Scheduler plans the moves and performs them.
type Scheduler struct {
	cfg     *config.Store
	log     *slog.Logger
	probe   disk.Probe
	store   *StateStore
	history *History

	mu   sync.RWMutex
	st   State
	plan *Plan
	next int
	// dirty counts the changes of the moves that are not in the state file
	// yet.
	dirty          int
	activeRollback *rollbackState
	// epoch counts the changes that make a measurement stale: an approval,
	// a rejection, a rollback. A scan that started before such a change
	// throws its result away.
	epoch uint64
	// scan is what the scan is doing right now.
	scan ScanStatus

	scanMu sync.Mutex
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	// stopping tells whether the daemon is shutting down. A task that is
	// started after that is not started at all: Stop waits for the
	// WaitGroup, and adding to it after that wait has begun is a misuse
	// the runtime panics about.
	stopping bool
	// scanActive tells whether a scan is in flight. A second request
	// while one is running is refused instead of queued: twenty clicks on
	// "Scan" must not mean twenty walks of the library.
	scanActive atomic.Bool
}

// New builds a scheduler.
func New(cfg *config.Store, log *slog.Logger, probe disk.Probe, store *StateStore, history *History) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cfg:     cfg,
		log:     log,
		probe:   probe,
		store:   store,
		history: history,
		st:      StateIdle,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Start restores the plan from the state file and resumes an interrupted
// execution.
//
// The context it is given is the daemon's lifetime: everything the scheduler
// starts in the background runs on it, so a stop cancels it.
func (s *Scheduler) Start(ctx context.Context) error {
	base, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	old := s.cancel
	s.ctx, s.cancel = base, cancel
	s.mu.Unlock()
	if old != nil {
		old()
	}

	// The leftovers of the copies that were cut in the middle are thrown
	// away first: they are the daemon's own files, they are in the way and
	// nothing else would free the space they take.
	s.cleanPartials(s.cfg.Load())

	stored, err := s.store.load()
	st, plan := State(stored.State), stored.Plan
	if err != nil {
		s.log.Warn("could not read the state file, starting empty", "error", err)
	} else if plan != nil || stored.Rollback != nil {
		s.mu.Lock()
		s.st, s.plan = st, plan
		if plan != nil {
			s.next = plan.ID
		}
		if stored.Rollback != nil && stored.Rollback.Plan.ID > s.next {
			s.next = stored.Rollback.Plan.ID
		}
		s.activeRollback = stored.Rollback
		if st == StateRollingBack && stored.Rollback == nil {
			s.st = StateFailed
			s.log.Warn("the interrupted rollback has no recovery record; automatic recovery is unavailable")
		}
		s.scan.Scanned = true
		s.mu.Unlock()
		s.log.Info("restored the plan", "state", st)

		if st == StateRollingBack && stored.Rollback != nil {
			op := stored.Rollback
			if op.Current {
				op.Plan = plan
			}
			s.run(func() { s.rollback(op.Plan, op.Targets, op.Current, op.Previous, true) })
		}
		if st == StateRunning && plan != nil {
			s.log.Info("resuming the interrupted execution", "plan_id", plan.ID)
			s.run(func() { s.execute(plan, true) })
		}
	}

	// The plan numbers must not collide with the ones in the history: the
	// history keeps the plans the state file has already forgotten, and a
	// rollback looks the plan up by its number.
	if last := s.lastHistoryID(); last > s.next {
		s.mu.Lock()
		s.next = last
		s.mu.Unlock()
		s.log.Info("the plan numbers continue after the history", "last_plan_id", last)
	}

	// The folders are measured right away: the daemon should not sit idle
	// until the first tick of the cron, and there is no plan to make before
	// the folders are known. A restored plan is not replaced: the scan skips
	// itself while a plan waits for an approval.
	//
	// The scan runs on the daemon's own context, so a stop cancels it and
	// the daemon waits for it.
	if err := s.StartScan(); err != nil {
		s.log.Error("the first scan could not be started", "error", err)
	}
	return nil
}

// lastHistoryID returns the number of the last plan in the history.
//
// It is zero when there is no history or it cannot be read.
func (s *Scheduler) lastHistoryID() int {
	entries, err := s.history.List()
	if err != nil {
		s.log.Warn("could not read the history", "error", err)
		return 0
	}
	last := 0
	for _, entry := range entries {
		if entry.ID > last {
			last = entry.ID
		}
	}
	return last
}

// cleanPartials removes the leftovers of the copies that were cut in the
// middle.
//
// A copy that was cut off leaves a file with the daemon's own suffix behind.
// Nothing else would ever take it away, and it takes the space of a whole
// film. The plan that was interrupted copies the entry again from the source,
// so throwing the leftover away is safe.
func (s *Scheduler) cleanPartials(cfg *config.Config) {
	for _, folder := range cfg.Sorted() {
		entries, err := os.ReadDir(folder.Path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), disk.PartialSuffix) {
				continue
			}
			path := filepath.Join(folder.Path, entry.Name())
			s.log.Info("removing the leftover of a copy", "path", path)
			if err := os.RemoveAll(path); err != nil {
				s.log.Warn("could not remove the leftover", "path", path, "error", err)
			}
		}
	}
}

// Stop cancels a running execution and waits for it.
//
// Nothing new is started after this: the tasks that are already running
// are given the time to finish, and the daemon goes away when they are
// done.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Scan measures the folders and publishes a new plan.
//
// A scan that is cancelled publishes nothing: the folders that were not
// measured would look like full disks, and a plan built from half the data
// is worse than no plan at all.
func (s *Scheduler) Scan(ctx context.Context) error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	s.mu.RLock()
	st := s.st
	s.mu.RUnlock()
	if st == StatePending || st == StateRunning || st == StateRollingBack {
		s.log.Info("skipping the scan: the plan is not settled", "state", st)
		return nil
	}

	cfg := s.cfg.Load()
	started := time.Now()
	s.mu.Lock()
	s.scan.Running = true
	s.scan.Started = started
	s.scan.Total = len(cfg.Folders)
	s.scan.Folder = ""
	s.scan.Entries = 0
	s.scan.Measured = 0
	epoch := s.epoch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.scan.Running = false
		s.scan.Folder = ""
		s.scan.Entries = 0
		s.mu.Unlock()
	}()

	s.log.Info("the scan is started", "folders", len(cfg.Folders))
	measured := s.gather(ctx, cfg)

	if ctx.Err() != nil {
		// The scan was cancelled: half the folders are measured and half
		// are not. A plan made of that would move files the daemon did not
		// mean to move, so nothing is published.
		s.log.Info("the scan was cancelled, the measurement is thrown away")
		return ctx.Err()
	}

	s.mu.Lock()
	if s.epoch != epoch {
		// The disks changed while the scan was running: the measurement is
		// stale and the plan it would make is wrong. Throw it away.
		s.mu.Unlock()
		s.log.Info("the scan is stale, the plan changed while it was running")
		return nil
	}
	s.next++
	plan := buildPlan(cfg, measured, time.Now(), s.next)
	s.plan = plan
	s.scan.Scanned = true
	if len(plan.Moves) == 0 {
		// Nothing to approve. The state stays idle, because a plan that
		// waits for an approval stops the next scan: the daemon would
		// never measure the disks again.
		s.st = StateIdle
	} else {
		s.st = StatePending
	}
	s.mu.Unlock()

	for _, move := range plan.Moves {
		s.log.Debug("the entry is planned to move",
			"name", move.Name, "from", move.Src, "to", move.Dst, "size", format.Bytes(move.Size))
	}
	if len(plan.Moves) == 0 {
		// There is nothing to approve, so the plan is not worth keeping:
		// the state file holds the last plan that had something in it.
		s.log.Info("there is nothing to move",
			"plan_id", plan.ID, "duration", time.Since(started).Round(time.Millisecond).String())
		return nil
	}
	s.log.Info("the plan is ready",
		"plan_id", plan.ID, "moves", len(plan.Moves),
		"duration", time.Since(started).Round(time.Millisecond).String())
	return s.save()
}

// StartScan starts a scan in the background and returns at once.
//
// The scan runs on the daemon's own context: it is cancelled when the
// daemon stops, and the daemon waits for it before it goes away.
//
// Only one scan runs at a time. A second request while a scan is in
// flight is refused, so that twenty clicks on the button do not mean
// twenty walks of the library.
func (s *Scheduler) StartScan() error {
	if !s.scanActive.CompareAndSwap(false, true) {
		return ErrScanRunning
	}

	s.mu.RLock()
	ctx := s.ctx
	s.mu.RUnlock()

	if !s.run(func() {
		defer s.scanActive.Store(false)
		if err := s.Scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("the scan failed", "error", err)
		}
	}) {
		s.scanActive.Store(false)
		return ErrStopping
	}
	return nil
}

// Approve starts the execution of the current plan.
func (s *Scheduler) Approve(planID int) error {
	s.mu.Lock()
	if !s.scan.Scanned {
		s.mu.Unlock()
		return ErrNotScanned
	}
	if s.plan == nil {
		s.mu.Unlock()
		return ErrNoPlan
	}
	if s.plan.ID != planID {
		s.mu.Unlock()
		return ErrStalePlan
	}
	if len(s.plan.Moves) == 0 {
		s.mu.Unlock()
		return ErrNothingToApprove
	}
	if s.st == StateRunning || s.st == StateRollingBack {
		s.mu.Unlock()
		return ErrBusy
	}
	if s.st != StatePending {
		// The plan is not waiting for an approval: it was already done,
		// or it was rejected. Approving it again would write a second
		// entry into the history for the same plan.
		s.mu.Unlock()
		return ErrStalePlan
	}
	plan := s.plan
	if !s.reserveLocked() {
		s.mu.Unlock()
		return ErrStopping
	}
	if err := s.store.Save(StateRunning, copyPlan(plan)); err != nil {
		s.wg.Done()
		s.mu.Unlock()
		return err
	}
	s.st = StateRunning
	s.epoch++
	s.mu.Unlock()
	s.start(func() { s.execute(plan, false) })
	return nil
}

// Remove excludes one move from a plan that is waiting for approval.
func (s *Scheduler) Remove(planID, moveID int) error {
	s.mu.Lock()
	if !s.scan.Scanned {
		s.mu.Unlock()
		return ErrNotScanned
	}
	if s.plan == nil {
		s.mu.Unlock()
		return ErrNoPlan
	}
	if s.plan.ID != planID {
		s.mu.Unlock()
		return ErrStalePlan
	}
	if s.st == StateRunning || s.st == StateRollingBack {
		s.mu.Unlock()
		return ErrBusy
	}
	if s.st != StatePending {
		s.mu.Unlock()
		return ErrStalePlan
	}
	for i, move := range s.plan.Moves {
		if move.ID != moveID {
			continue
		}
		s.plan.Moves = append(s.plan.Moves[:i], s.plan.Moves[i+1:]...)
		if len(s.plan.Moves) == 0 {
			s.st = StateIdle
		}
		s.epoch++
		s.mu.Unlock()
		return s.save()
	}
	s.mu.Unlock()
	return fmt.Errorf("there is no move #%d", moveID)
}

// Reject discards the current plan.
func (s *Scheduler) Reject(planID int) error {
	s.mu.Lock()
	if !s.scan.Scanned {
		s.mu.Unlock()
		return ErrNotScanned
	}
	if s.plan == nil {
		s.mu.Unlock()
		return ErrNoPlan
	}
	if s.plan.ID != planID {
		s.mu.Unlock()
		return ErrStalePlan
	}
	if s.st == StateRunning || s.st == StateRollingBack {
		s.mu.Unlock()
		return ErrBusy
	}
	if s.st != StatePending {
		s.mu.Unlock()
		return ErrStalePlan
	}
	plan := s.plan
	s.plan = nil
	s.st = StateIdle
	s.epoch++
	s.mu.Unlock()

	s.appendHistory(plan, "plan", StateIdle, time.Now())
	return s.save()
}

// Rollback puts one move back where it came from, or all of them when moveID
// is zero.
//
// The plan may be the current one or an older one from the history: a scan
// that runs after the execution replaces the current plan, and the files
// that were moved are still worth putting back.
func (s *Scheduler) Rollback(planID, moveID int) error {
	s.mu.RLock()
	if !s.scan.Scanned {
		s.mu.RUnlock()
		return ErrNotScanned
	}
	if s.st == StateRunning || s.st == StateRollingBack {
		s.mu.RUnlock()
		return ErrBusy
	}
	current := s.plan
	s.mu.RUnlock()

	plan := current
	if plan == nil || plan.ID != planID {
		plan = s.planFromHistory(planID)
	}
	if plan == nil {
		return fmt.Errorf("there is no plan #%d", planID)
	}

	// The statuses are read under the lock: the executor writes them
	// while it moves the files, and it writes them into this very plan
	// when it is the current one.
	s.mu.RLock()
	var targets []int
	for i := range plan.Moves {
		move := plan.Moves[i]
		if move.Status != StatusDone {
			continue
		}
		if moveID > 0 && move.ID != moveID {
			continue
		}
		targets = append(targets, i)
	}
	s.mu.RUnlock()
	if len(targets) == 0 {
		return fmt.Errorf("there is nothing to roll back")
	}
	// The last thing moved goes back first.
	sort.Slice(targets, func(i, j int) bool { return targets[i] > targets[j] })

	// The state is checked and set in one go. An approval that came in
	// between the two would start the execution, and the rollback would
	// then move the same files at the same time.
	s.mu.Lock()
	if s.st == StateRunning || s.st == StateRollingBack {
		s.mu.Unlock()
		return ErrBusy
	}
	if !s.reserveLocked() {
		s.mu.Unlock()
		return ErrStopping
	}
	isCurrent := s.plan != nil && s.plan.ID == planID
	previous := s.st
	op := &rollbackState{Plan: copyPlan(plan), Targets: targets, Current: isCurrent, Previous: previous}
	if err := s.store.Save(StateRollingBack, copyPlan(s.plan), op); err != nil {
		s.wg.Done()
		s.mu.Unlock()
		return err
	}
	op.Plan = plan
	s.activeRollback = op
	s.st = StateRollingBack
	s.epoch++
	s.mu.Unlock()
	s.start(func() { s.rollback(plan, targets, isCurrent, previous, false) })
	return nil
}

// planFromHistory looks a plan up in the history.
func (s *Scheduler) planFromHistory(planID int) *Plan {
	entries, err := s.history.List()
	if err != nil {
		s.log.Warn("could not read the history", "error", err)
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].ID == planID && entries[i].Plan != nil {
			return entries[i].Plan
		}
	}
	return nil
}

// Snapshot returns the current state and a copy of the plan.
func (s *Scheduler) Snapshot() (State, *Plan) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st, copyPlan(s.plan)
}

// copyPlan returns a copy of the plan.
//
// The executor writes into the plan while it moves the files, so everything
// that reads the plan outside the lock gets a copy of it.
func copyPlan(plan *Plan) *Plan {
	if plan == nil {
		return nil
	}
	copied := *plan
	copied.Folders = append([]FolderStatus(nil), plan.Folders...)
	copied.Moves = append([]Move(nil), plan.Moves...)
	return &copied
}

// gather measures the folders.
//
// The walk of a src folder is the expensive part, so it happens only when that
// volume is already below its threshold.
func (s *Scheduler) gather(ctx context.Context, cfg *config.Config) measured {
	result := measured{
		volumes: map[string]disk.Volume{},
		entries: map[string][]disk.Entry{},
		errors:  map[string]string{},
	}

	for _, folder := range cfg.Sorted() {
		if ctx.Err() != nil {
			return result
		}
		volume, err := s.probe.VolumeOf(folder.Path)
		if err != nil {
			result.errors[folder.Path] = err.Error()
			s.log.Error("could not measure the volume", "folder", folder.Path, "error", err)
			continue
		}
		result.volumes[folder.Path] = volume
		s.setScanMeasured()
		s.log.Debug("the volume of the folder",
			"folder", folder.Path, "role", folder.DirType, "type", folder.Type,
			"free", format.Bytes(volume.Free), "total", format.Bytes(volume.Total),
			"threshold", format.Bytes(int64(folder.MinFree)))

		if folder.DirType != config.DirTypeSrc {
			continue
		}
		if volume.Free >= int64(folder.MinFree) {
			s.log.Debug("there is enough free space on the volume",
				"folder", folder.Path, "free", format.Bytes(volume.Free),
				"threshold", format.Bytes(int64(folder.MinFree)))
			continue
		}

		walked := time.Now()
		s.setScanFolder(folder.Path)
		entries, err := disk.ListEntries(folder.Path, func(count int) { s.setScanEntries(count) })
		if err != nil {
			result.errors[folder.Path] = err.Error()
			s.log.Error("could not read the folder", "folder", folder.Path, "error", err)
			continue
		}
		var total int64
		for _, entry := range entries {
			total += entry.Size
		}
		s.log.Info("the folder is below the threshold, its entries are counted",
			"folder", folder.Path, "type", folder.Type,
			"free", format.Bytes(volume.Free), "threshold", format.Bytes(int64(folder.MinFree)),
			"entries", len(entries), "size", format.Bytes(total),
			"duration", time.Since(walked).Round(time.Millisecond).String())
		result.entries[folder.Path] = entries
	}
	return result
}

// setScanFolder says which folder the scan is walking right now.
func (s *Scheduler) setScanFolder(path string) {
	s.mu.Lock()
	s.scan.Folder = path
	s.scan.Entries = 0
	s.mu.Unlock()
}

// setScanEntries says how many entries of that folder have been counted.
func (s *Scheduler) setScanEntries(count int) {
	s.mu.Lock()
	s.scan.Entries = count
	s.mu.Unlock()
}

// setScanMeasured counts a folder whose volume has been measured.
func (s *Scheduler) setScanMeasured() {
	s.mu.Lock()
	s.scan.Measured++
	s.mu.Unlock()
}

// ScanStatus returns what the scan is doing right now.
func (s *Scheduler) ScanStatus() ScanStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scan
}

// execute performs the moves of a plan.
func (s *Scheduler) execute(plan *Plan, resume bool) {
	for i := range plan.Moves {
		if s.stopped() {
			// The state stays "running": the next start picks the plan up
			// again. Calling it done would drop the rest of the plan.
			s.log.Warn("the execution was stopped, the rest of the plan stays pending")
			s.mu.Lock()
			s.st = StateRunning
			s.mu.Unlock()
			s.save()
			return
		}
		s.mu.RLock()
		move := plan.Moves[i]
		s.mu.RUnlock()

		if move.Status == StatusDone || move.Status == StatusRolledBack {
			continue
		}
		if err := s.stillConfigured(move); err != nil {
			s.setMove(plan, i, StatusFailed, err.Error())
			continue
		}

		if resume {
			// The files say what happened, not the saved status: it can be
			// up to a hundred moves behind.
			_, sourceErr := os.Lstat(move.Path)
			_, destErr := os.Lstat(move.DstPath)
			switch {
			case errors.Is(sourceErr, os.ErrNotExist) && destErr == nil:
				// The copy finished before the crash; only the status was
				// not saved.
				s.setMove(plan, i, StatusDone, "")
				continue
			case sourceErr == nil && destErr == nil:
				s.setMove(plan, i, StatusFailed, "и источник, и приёмник на месте")
				continue
			case sourceErr != nil:
				s.setMove(plan, i, StatusFailed, "источника нет на месте")
				continue
			}
		}

		// The plan may have waited for an approval for hours, and the file
		// may have been written again since: a download that started over.
		// Moving it now would move half a file and delete the original.
		if s.changedRecently(move.Path) {
			s.setMove(plan, i, StatusFailed, "файл изменился после составления плана")
			continue
		}

		s.transfer(plan, i, move.Path, move.DstPath, StatusDone)
		if s.stopped() {
			s.save()
			return
		}
	}
	s.finish(plan, "plan")
}

// rollback puts the given moves back where they came from.
func (s *Scheduler) rollback(plan *Plan, targets []int, isCurrent bool, previous State, resume bool) {
	for _, i := range targets {
		if s.stopped() {
			s.log.Warn("the rollback was stopped, the rest stays as it is")
			s.mu.Lock()
			s.st = StateRollingBack
			s.mu.Unlock()
			s.save()
			return
		}
		s.mu.RLock()
		move := plan.Moves[i]
		s.mu.RUnlock()

		if move.Status == StatusRolledBack {
			continue
		}
		// A rename may have finished before its status reached the state file.
		_, fromErr := os.Lstat(move.DstPath)
		_, toErr := os.Lstat(move.Path)
		if resume && errors.Is(fromErr, os.ErrNotExist) && toErr == nil {
			s.setMove(plan, i, StatusRolledBack, "")
			continue
		}

		if err := s.stillConfigured(move); err != nil {
			s.setMove(plan, i, StatusFailed, err.Error())
			continue
		}
		if _, err := os.Lstat(move.DstPath); err != nil {
			s.setMove(plan, i, StatusFailed, "файла на месте назначения нет")
			continue
		}
		if _, err := os.Lstat(move.Path); err == nil {
			s.setMove(plan, i, StatusFailed, "на месте источника уже есть файл")
			continue
		}
		s.transfer(plan, i, move.DstPath, move.Path, StatusRolledBack)
		if s.stopped() {
			s.save()
			return
		}
	}

	s.mu.RLock()
	failed := false
	for _, move := range plan.Moves {
		if move.Status == StatusFailed {
			failed = true
		}
	}
	s.mu.RUnlock()

	st := StateDone
	if failed {
		st = StateFailed
	}

	s.appendHistory(plan, "rollback", st, time.Now())
	s.mu.Lock()
	s.activeRollback = nil
	if isCurrent {
		s.st = st
	} else {
		// The plan that was rolled back is not the current one: the current
		// plan keeps the state it had.
		s.st = previous
	}
	s.mu.Unlock()

	s.save()
}

// transfer moves one entry and records the outcome.
//
// The destination is never touched: the copy goes to a name of its own and is
// put in place at the end, so that a file the daemon did not create is never
// overwritten and never deleted.
func (s *Scheduler) transfer(plan *Plan, index int, from, to string, okStatus string) {
	s.setMove(plan, index, StatusMoving, "")

	var lastErr error
	for attempt := 1; attempt <= moveAttempts; attempt++ {
		if attempt == 1 {
			if _, err := os.Lstat(to); err == nil {
				s.setMove(plan, index, StatusFailed, "на месте назначения уже есть файл")
				return
			}
		}
		s.setAttempt(plan, index, attempt)

		started := time.Now()
		var published int64
		err := disk.Move(from, to, func(copied int64) {
			// The copy calls this for every chunk; only every few
			// megabytes is worth a lock.
			if copied-published < progressStep {
				return
			}
			published = copied
			s.setProgress(plan, index, copied)
		})
		if err == nil {
			s.log.Info("the entry is moved",
				"from", from, "to", to, "attempt", attempt,
				"duration", time.Since(started).Round(time.Millisecond).String())
			s.setMove(plan, index, okStatus, "")
			return
		}
		if errors.Is(err, disk.ErrSourceNotRemoved) {
			// The copy is in place and whole; only the source is left
			// behind. The move succeeded, and a retry would find the
			// destination taken.
			s.log.Warn("the entry is moved, but the source is left behind",
				"from", from, "to", to, "attempt", attempt, "error", err)
			s.setMove(plan, index, okStatus, "источник не удалился")
			return
		}
		// The first error is the one that says what is wrong: the later
		// attempts fail because of it, and their messages point at the
		// wrong thing.
		if lastErr == nil {
			lastErr = err
		}
		s.log.Warn("the move failed", "from", from, "to", to, "attempt", attempt, "error", err)
		if attempt < moveAttempts {
			// The pause ends right away when the daemon is stopping: there
			// is no point in trying again when nobody is waiting.
			select {
			case <-time.After(retryPause):
			case <-s.daemonCtx().Done():
				return
			}
		}
	}
	s.setMove(plan, index, StatusFailed, lastErr.Error())
}

// stillConfigured checks the move against the configuration in force.
//
// The config can change between the scan and the execution, and a folder that
// was taken out of the config is not to be touched any more.
func (s *Scheduler) stillConfigured(move Move) error {
	cfg := s.cfg.Load()

	src, ok := cfg.Folders[move.Src]
	if !ok || src.DirType != config.DirTypeSrc {
		return errors.New("папка-источник убрана из конфига")
	}
	dst, ok := cfg.Folders[move.Dst]
	if !ok || dst.DirType != config.DirTypeDst || dst.Type != move.Type {
		return errors.New("папка-приёмник убрана из конфига или сменила тип")
	}
	return nil
}

// changedRecently tells whether the file was changed within min_age.
//
// A file that is written right now is not media yet: it is being downloaded
// or unpacked. Moving it would move half a file and delete the original.
func (s *Scheduler) changedRecently(path string) bool {
	minAge := time.Duration(s.cfg.Load().Scan.MinAge)
	if minAge <= 0 {
		return false
	}
	newest, err := disk.ModTime(path)
	return err != nil || newest.After(time.Now().Add(-minAge))
}

// finish closes a plan and puts it into the history.
func (s *Scheduler) finish(plan *Plan, kind string) {
	s.mu.RLock()
	failed := false
	for _, move := range plan.Moves {
		if move.Status == StatusFailed {
			failed = true
		}
	}
	s.mu.RUnlock()

	st := StateDone
	if failed {
		st = StateFailed
	}

	// The history is written before the state says the plan is done: a
	// reader that sees "done" must see the plan in the history too.
	s.appendHistory(plan, kind, st, time.Now())

	s.mu.Lock()
	s.st = st
	s.mu.Unlock()

	s.save()
}

// run starts a task in the background and waits for it when the daemon
// stops.
//
// A task that is started after the shutdown has begun is not started at
// all: the WaitGroup is waited for by Stop, and adding to it after that
// wait has begun is a misuse the runtime panics about.
func (s *Scheduler) run(fn func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reserveLocked() {
		return false
	}
	s.start(fn)
	return true
}

// reserveLocked takes a slot in the WaitGroup for a task that is about to
// start. It returns false when the daemon is stopping.
//
// The slot is taken while the lock is held, so that it can never be taken
// after Stop has begun to wait: the runtime panics about that.
//
// The caller must hold s.mu.
func (s *Scheduler) reserveLocked() bool {
	if s.stopping {
		return false
	}
	s.wg.Add(1)
	return true
}

// start runs a task in a slot that was reserved with reserveLocked.
func (s *Scheduler) start(fn func()) {
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// stopped reports whether the daemon is shutting down.
func (s *Scheduler) stopped() bool {
	return s.daemonCtx().Err() != nil
}

// daemonCtx returns the context of the daemon's lifetime.
func (s *Scheduler) daemonCtx() context.Context {
	s.mu.RLock()
	ctx := s.ctx
	s.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (s *Scheduler) setMove(plan *Plan, index int, status, message string) {
	s.mu.Lock()
	plan.Moves[index].Status = status
	plan.Moves[index].Error = message
	s.dirty++
	// The state file holds the whole plan, so writing it after every move
	// would rewrite hundreds of kilobytes for every move. It is written
	// every hundred changes instead, and once more when the plan is done.
	write := s.dirty >= saveEveryMoves
	if write {
		s.dirty = 0
	}
	s.mu.Unlock()

	if write {
		s.save()
	}
}

func (s *Scheduler) setAttempt(plan *Plan, index, attempt int) {
	s.mu.Lock()
	plan.Moves[index].Attempt = attempt
	s.mu.Unlock()
}

func (s *Scheduler) setProgress(plan *Plan, index int, copied int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan.Moves[index].Copied = copied
}

func (s *Scheduler) appendHistory(plan *Plan, kind string, st State, at time.Time) {
	// The copy is taken under the lock: the executor writes into the plan
	// while it moves the files.
	s.mu.RLock()
	copied := copyPlan(plan)
	s.mu.RUnlock()

	entry := HistoryEntry{
		ID:        copied.ID,
		Kind:      kind,
		CreatedAt: at,
		State:     string(st),
		Plan:      copied,
	}
	// Only the moves that were really made are counted: a plan that was
	// rejected moved nothing, and a plan that failed halfway moved a part
	// of what it said.
	for _, move := range copied.Moves {
		if move.Status != StatusDone && move.Status != StatusRolledBack {
			continue
		}
		entry.MovesCount++
		entry.Bytes += move.Size
	}
	if err := s.history.Append(entry); err != nil {
		s.log.Warn("could not write the history", "error", err)
	}
}

func (s *Scheduler) save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.st
	plan := copyPlan(s.plan)
	var op *rollbackState
	if s.activeRollback != nil {
		copied := *s.activeRollback
		copied.Plan = copyPlan(copied.Plan)
		op = &copied
	}

	if err := s.store.Save(st, plan, op); err != nil {
		s.log.Warn("could not write the state file", "error", err)
		return err
	}
	return nil
}

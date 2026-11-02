// Package scheduler decides what moves where and performs the moves.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
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
)

// Plan is what the daemon proposes to do.
type Plan struct {
	ID        int            `json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	Folders   []FolderStatus `json:"folders"`
	Moves     []Move         `json:"moves"`
	Unmovable []Unmovable    `json:"unmovable"`
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

// Unmovable is an entry that stays where it is, and why.
type Unmovable struct {
	Src    string `json:"src"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Reason string `json:"reason"`
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

	scanMu sync.Mutex
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// New builds a scheduler.
func New(cfg *config.Store, log *slog.Logger, probe disk.Probe, store *StateStore, history *History) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		log:     log,
		probe:   probe,
		store:   store,
		history: history,
		st:      StateIdle,
	}
}

// Start restores the plan from the state file and resumes an interrupted
// execution.
func (s *Scheduler) Start(ctx context.Context) error {
	base, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.ctx, s.cancel = base, cancel
	s.mu.Unlock()

	st, plan, err := s.store.Load()
	if err != nil {
		s.log.Warn("could not read the state file, starting empty", "error", err)
		return nil
	}
	if plan == nil {
		return nil
	}

	s.mu.Lock()
	s.st, s.plan, s.next = st, plan, plan.ID
	s.mu.Unlock()
	s.log.Info("restored the plan", "state", st, "plan_id", plan.ID)

	if st == StateRunning {
		s.log.Info("resuming the interrupted execution", "plan_id", plan.ID)
		s.run(func() { s.execute(plan, true) })
	}
	return nil
}

// Stop cancels a running execution and waits for it.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.RLock()
	cancel := s.cancel
	s.mu.RUnlock()
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
	measured := s.gather(ctx, cfg)

	s.mu.Lock()
	s.next++
	plan := buildPlan(cfg, measured, time.Now(), s.next)
	s.plan = plan
	s.st = StatePending
	s.mu.Unlock()

	s.log.Info("the plan is ready",
		"plan_id", plan.ID, "moves", len(plan.Moves), "unmovable", len(plan.Unmovable))
	return s.save()
}

// Approve starts the execution of the current plan.
func (s *Scheduler) Approve(planID int) error {
	s.mu.Lock()
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
	plan := s.plan
	s.st = StateRunning
	s.mu.Unlock()

	s.save()
	s.run(func() { s.execute(plan, false) })
	return nil
}

// Reject discards the current plan.
func (s *Scheduler) Reject(planID int) error {
	s.mu.Lock()
	if s.plan == nil {
		s.mu.Unlock()
		return ErrNoPlan
	}
	if s.plan.ID != planID {
		s.mu.Unlock()
		return ErrStalePlan
	}
	plan := s.plan
	s.plan = nil
	s.st = StateIdle
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
	s.mu.Lock()
	if s.st == StateRunning || s.st == StateRollingBack {
		s.mu.Unlock()
		return ErrBusy
	}
	current := s.plan
	previous := s.st
	s.mu.Unlock()

	plan := current
	if plan == nil || plan.ID != planID {
		plan = s.planFromHistory(planID)
	}
	if plan == nil {
		return fmt.Errorf("there is no plan #%d", planID)
	}

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
	if len(targets) == 0 {
		return fmt.Errorf("there is nothing to roll back")
	}
	// The last thing moved goes back first.
	sort.Slice(targets, func(i, j int) bool { return targets[i] > targets[j] })

	isCurrent := current != nil && current.ID == planID
	s.mu.Lock()
	s.st = StateRollingBack
	s.mu.Unlock()

	s.save()
	s.run(func() { s.rollback(plan, targets, isCurrent, previous) })
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

	if s.plan == nil {
		return s.st, nil
	}
	copied := *s.plan
	copied.Folders = append([]FolderStatus(nil), s.plan.Folders...)
	copied.Moves = append([]Move(nil), s.plan.Moves...)
	copied.Unmovable = append([]Unmovable(nil), s.plan.Unmovable...)
	return s.st, &copied
}

// measured is what a scan found on the disks.
type measured struct {
	volumes map[string]disk.Volume
	entries map[string][]disk.Entry
	errors  map[string]string
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
			continue
		}
		result.volumes[folder.Path] = volume

		if folder.DirType != config.DirTypeSrc {
			continue
		}
		if volume.Free >= int64(folder.MinFree) {
			continue
		}
		entries, err := disk.ListEntries(folder.Path)
		if err != nil {
			result.errors[folder.Path] = err.Error()
			continue
		}
		result.entries[folder.Path] = entries
	}
	return result
}

// buildPlan turns what was measured into a plan.
//
// Pure: everything it needs is in the arguments and it touches no disk. That
// is what makes the interesting part — which entry goes where — testable
// without a disk.
func buildPlan(cfg *config.Config, m measured, now time.Time, planID int) *Plan {
	plan := &Plan{ID: planID, CreatedAt: now}
	folders := cfg.Sorted()

	// The simulated free space of every volume. Folders on one volume share
	// the number, so two sources on one disk do not each plan for the same
	// deficit.
	free := map[uint64]int64{}
	for _, folder := range folders {
		volume := m.volumes[folder.Path]
		free[volume.Dev] = volume.Free
	}

	for _, folder := range folders {
		volume := m.volumes[folder.Path]
		status := FolderStatus{
			Path:    folder.Path,
			DirType: string(folder.DirType),
			Type:    folder.Type,
			Free:    volume.Free,
			Total:   volume.Total,
			MinFree: int64(folder.MinFree),
		}
		switch {
		case m.errors[folder.Path] != "":
			status.State, status.Error = "error", m.errors[folder.Path]
		case folder.DirType == config.DirTypeSrc && volume.Free < int64(folder.MinFree):
			status.State = "low"
		default:
			status.State = "ok"
		}
		plan.Folders = append(plan.Folders, status)
	}

	minAge := time.Duration(cfg.Scan.MinAge)
	for _, folder := range folders {
		if folder.DirType != config.DirTypeSrc {
			continue
		}
		volume := m.volumes[folder.Path]
		deficit := int64(folder.MinFree) - free[volume.Dev]
		if deficit <= 0 {
			continue
		}

		entries := m.entries[folder.Path]
		// The biggest first: it frees the most space per move, so the fewest
		// entries have to move. The name breaks the tie, so the plan is the
		// same on every run.
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Size != entries[j].Size {
				return entries[i].Size > entries[j].Size
			}
			return entries[i].Name < entries[j].Name
		})

		freed := int64(0)
		for _, entry := range entries {
			if freed >= deficit {
				break
			}
			if entry.ModTime.After(now.Add(-minAge)) {
				plan.Unmovable = append(plan.Unmovable, Unmovable{
					Src: folder.Path, Name: entry.Name, Path: entry.Path, Size: entry.Size,
					Reason: "изменён меньше суток назад",
				})
				continue
			}

			dst, dstVolume, ok := pickDestination(folders, m, free, entry, folder.Type, volume.Dev)
			if !ok {
				plan.Unmovable = append(plan.Unmovable, Unmovable{
					Src: folder.Path, Name: entry.Name, Path: entry.Path, Size: entry.Size,
					Reason: "нет папки-приёмника с таким type и достаточным местом",
				})
				continue
			}
			if _, err := os.Lstat(dst.Path + "/" + entry.Name); err == nil {
				plan.Unmovable = append(plan.Unmovable, Unmovable{
					Src: folder.Path, Name: entry.Name, Path: entry.Path, Size: entry.Size,
					Reason: "в приёмнике уже есть такой файл",
				})
				continue
			}

			plan.Moves = append(plan.Moves, Move{
				ID:      len(plan.Moves) + 1,
				Src:     folder.Path,
				Name:    entry.Name,
				Path:    entry.Path,
				Dst:     dst.Path,
				DstPath: dst.Path + "/" + entry.Name,
				Size:    entry.Size,
				Type:    folder.Type,
				Status:  StatusPending,
			})
			free[dstVolume.Dev] -= entry.Size
			free[volume.Dev] += entry.Size
			freed += entry.Size
		}
	}
	return plan
}

// pickDestination chooses where an entry goes: the dst folder of the same
// type on another volume with the most free space, that can still take it.
func pickDestination(folders []config.Folder, m measured, free map[uint64]int64, entry disk.Entry, wantType int, srcDev uint64) (config.Folder, disk.Volume, bool) {
	var best config.Folder
	var bestVolume disk.Volume
	bestFree := int64(-1)

	for _, candidate := range folders {
		if candidate.DirType != config.DirTypeDst || candidate.Type != wantType {
			continue
		}
		if m.errors[candidate.Path] != "" {
			continue
		}
		volume := m.volumes[candidate.Path]
		if volume.Dev == srcDev {
			// A move inside one volume frees nothing, so it would never
			// close the deficit.
			continue
		}
		if free[volume.Dev]-int64(candidate.MinFree) < entry.Size {
			continue
		}
		if free[volume.Dev] > bestFree {
			best, bestVolume, bestFree = candidate, volume, free[volume.Dev]
		}
	}
	return best, bestVolume, bestFree >= 0
}

// execute performs the moves of a plan.
func (s *Scheduler) execute(plan *Plan, resume bool) {
	for i := range plan.Moves {
		if s.stopped() {
			s.log.Warn("the execution was stopped, the rest of the plan stays pending")
			break
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

		clean := false
		if resume && move.Status == StatusMoving {
			// The copy may have finished before the crash: the source is
			// gone only then, and deleting the destination now would throw
			// away a good copy.
			if _, err := os.Stat(move.Path); errors.Is(err, os.ErrNotExist) {
				s.setMove(plan, i, StatusDone, "")
				continue
			}
			clean = true
		}
		s.transfer(plan, i, move.Path, move.DstPath, clean, StatusDone)
	}
	s.finish(plan, "plan")
}

// rollback puts the given moves back where they came from.
func (s *Scheduler) rollback(plan *Plan, targets []int, isCurrent bool, previous State) {
	for _, i := range targets {
		if s.stopped() {
			s.log.Warn("the rollback was stopped, the rest stays as it is")
			break
		}
		s.mu.RLock()
		move := plan.Moves[i]
		s.mu.RUnlock()

		if _, err := os.Lstat(move.DstPath); err != nil {
			s.setMove(plan, i, StatusFailed, "файла на месте назначения нет")
			continue
		}
		if _, err := os.Lstat(move.Path); err == nil {
			s.setMove(plan, i, StatusFailed, "на месте источника уже есть файл")
			continue
		}
		s.transfer(plan, i, move.DstPath, move.Path, false, StatusRolledBack)
	}

	failed := false
	for _, move := range plan.Moves {
		if move.Status == StatusFailed {
			failed = true
		}
	}
	st := StateDone
	if failed {
		st = StateFailed
	}

	s.mu.Lock()
	if isCurrent {
		s.st = st
	} else {
		// The plan that was rolled back is not the current one: the current
		// plan keeps the state it had.
		s.st = previous
	}
	s.mu.Unlock()

	s.save()
	s.appendHistory(plan, "rollback", st, time.Now())
}

// transfer moves one entry and records the outcome.
//
// clean says the destination is ours to remove: a partial copy from an
// earlier attempt. It is never set for a file we did not create.
func (s *Scheduler) transfer(plan *Plan, index int, from, to string, clean bool, okStatus string) {
	s.setMove(plan, index, StatusMoving, "")

	var lastErr error
	for attempt := 1; attempt <= moveAttempts; attempt++ {
		if attempt == 1 && !clean {
			if _, err := os.Lstat(to); err == nil {
				s.setMove(plan, index, StatusFailed, "на месте назначения уже есть файл")
				return
			}
		} else {
			// The destination is ours: a partial copy from an earlier attempt.
			// The retry starts from scratch — finishing a partial copy
			// would leave a broken file behind.
			os.RemoveAll(to)
		}
		s.setAttempt(plan, index, attempt)

		err := disk.Move(from, to, func(copied int64) { s.setProgress(plan, index, copied) })
		if err == nil {
			s.setMove(plan, index, okStatus, "")
			return
		}
		lastErr = err
		s.log.Warn("the move failed", "from", from, "to", to, "attempt", attempt, "error", err)
		if attempt < moveAttempts {
			time.Sleep(retryPause)
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
	s.mu.Lock()
	s.st = st
	s.mu.Unlock()

	s.save()
	s.appendHistory(plan, kind, st, time.Now())
}

func (s *Scheduler) run(fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// stopped reports whether the daemon is shutting down.
func (s *Scheduler) stopped() bool {
	s.mu.RLock()
	ctx := s.ctx
	s.mu.RUnlock()
	return ctx != nil && ctx.Err() != nil
}

func (s *Scheduler) setMove(plan *Plan, index int, status, message string) {
	s.mu.Lock()
	plan.Moves[index].Status = status
	plan.Moves[index].Error = message
	s.mu.Unlock()
	s.save()
}

func (s *Scheduler) setAttempt(plan *Plan, index, attempt int) {
	s.mu.Lock()
	plan.Moves[index].Attempt = attempt
	s.mu.Unlock()
}

func (s *Scheduler) setProgress(plan *Plan, index int, copied int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if copied-plan.Moves[index].Copied < progressStep {
		return
	}
	plan.Moves[index].Copied = copied
}

func (s *Scheduler) appendHistory(plan *Plan, kind string, st State, at time.Time) {
	entry := HistoryEntry{
		ID:         plan.ID,
		Kind:       kind,
		CreatedAt:  at,
		State:      string(st),
		MovesCount: len(plan.Moves),
		Plan:       plan,
	}
	for _, move := range plan.Moves {
		entry.Bytes += move.Size
	}
	if err := s.history.Append(entry); err != nil {
		s.log.Warn("could not write the history", "error", err)
	}
}

func (s *Scheduler) save() error {
	s.mu.RLock()
	st, plan := s.st, s.plan
	s.mu.RUnlock()

	if err := s.store.Save(st, plan); err != nil {
		s.log.Warn("could not write the state file", "error", err)
		return err
	}
	return nil
}

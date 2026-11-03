// The planning: what moves where. It is pure — it reads no disk and no
// clock — so that the interesting part is testable without a disk.
package scheduler

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
)

// measured is what a scan found on the disks.
type measured struct {
	volumes map[string]disk.Volume
	entries map[string][]disk.Entry
	errors  map[string]string
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
			if entry.IsSymlink {
				// A symlink frees nothing: it is the target that takes the
				// space, and the target stays where it is. Moving the link
				// itself would also break a relative target.
				continue
			}
			if entry.ModTime.After(now.Add(-minAge)) {
				// It was changed too recently: it is left alone.
				continue
			}

			dst, dstVolume, ok := pickDestination(folders, m, free, entry, folder.Type, volume.Dev)
			if !ok {
				// There is nowhere for it to go: it stays where it is.
				continue
			}
			if _, err := os.Lstat(filepath.Join(dst.Path, entry.Name)); err == nil {
				// The destination has a file of that name already.
				continue
			}

			plan.Moves = append(plan.Moves, Move{
				ID:      len(plan.Moves) + 1,
				Src:     folder.Path,
				Name:    entry.Name,
				Path:    entry.Path,
				Dst:     dst.Path,
				DstPath: filepath.Join(dst.Path, entry.Name),
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

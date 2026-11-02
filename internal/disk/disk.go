// Package disk knows about the folders: how much space is left on them, what
// is in them, and how to move an entry from one to another.
package disk

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Volume is what is known about one filesystem.
type Volume struct {
	Dev   uint64 // two paths with the same Dev share their free space
	Free  int64  // bytes available to an unprivileged user
	Total int64
}

// Probe reports what is known about the volume a folder lives on.
//
// It is an interface so the end-to-end test can treat each temporary folder
// as its own volume; production asks the kernel.
type Probe interface {
	VolumeOf(path string) (Volume, error)
}

// StatfsProbe asks the kernel.
type StatfsProbe struct{}

// VolumeOf measures the filesystem a path lives on.
//
// Free is Bavail, not Bfree: Bfree counts the blocks the filesystem reserves
// for root, which an unprivileged daemon can never use.
func (StatfsProbe) VolumeOf(path string) (Volume, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return Volume{}, err
	}
	var fsStat syscall.Statfs_t
	if err := syscall.Statfs(path, &fsStat); err != nil {
		return Volume{}, err
	}
	blockSize := int64(fsStat.Bsize)
	return Volume{
		Dev:   uint64(stat.Dev),
		Free:  int64(fsStat.Bavail) * blockSize,
		Total: int64(fsStat.Blocks) * blockSize,
	}, nil
}

// Entry is one immediate child of a folder: the unit of movement.
type Entry struct {
	Name      string
	Path      string
	Size      int64
	ModTime   time.Time
	IsDir     bool
	IsSymlink bool
}

// ListEntries returns the immediate children of dir, sorted by name.
//
// Hidden names and anything that is not a file, a directory or a symlink are
// skipped: they are not media and cannot be copied.
func ListEntries(dir string) ([]Entry, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var entries []Entry
	for _, item := range items {
		name := item.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			entries = append(entries, Entry{
				Name:      name,
				Path:      path,
				Size:      info.Size(),
				ModTime:   info.ModTime(),
				IsSymlink: true,
			})
		case info.IsDir():
			size, modTime, err := treeStat(path)
			if err != nil {
				return nil, err
			}
			entries = append(entries, Entry{
				Name:    name,
				Path:    path,
				Size:    size,
				ModTime: modTime,
				IsDir:   true,
			})
		case info.Mode().IsRegular():
			entries = append(entries, Entry{
				Name:    name,
				Path:    path,
				Size:    info.Size(),
				ModTime: info.ModTime(),
			})
		}
	}
	return entries, nil
}

// treeStat returns the size of the tree under path and the newest mtime in it.
//
// The newest mtime matters more than the directory's own: changing a file
// inside a directory does not change the directory's mtime.
func treeStat(path string) (int64, time.Time, error) {
	var size int64
	var newest time.Time

	err := filepath.Walk(path, func(_ string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	return size, newest, nil
}

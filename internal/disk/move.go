package disk

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Progress is called with the number of bytes copied so far.
type Progress func(copied int64)

// PartialSuffix is the name the copy gets before it is put in place.
//
// The copy never writes into the destination itself: a destination that is
// somebody else's stays untouched, and a copy that was cut in the middle is
// picked up by the next attempt under a name the daemon owns.
const PartialSuffix = ".drs-partial"

// ErrSourceNotRemoved means the copy is in place and the source could not
// be removed.
//
// It is not a failed move: the entry is at the destination and is whole.
// The source is a leftover that takes up space until somebody removes it.
var ErrSourceNotRemoved = errors.New("the source could not be removed")

// Move moves the entry at src to dst.
//
// The same filesystem is a rename; a different one is a copy followed by the
// removal of the source. The source is removed only after the copy has
// finished, so a failure anywhere in the copy leaves the source where it was
// and the entry is still where it belongs.
func Move(src, dst string, progress Progress) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("move %q: %w", dst, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("move %q: %w", dst, err)
	}

	// A copy that was cut in the middle is under this name. It is ours, and
	// it is in the way.
	os.RemoveAll(dst + PartialSuffix)

	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("move %q to %q: %w", src, dst, err)
	}
	return copyAndPublish(src, dst, progress)
}

// copyAndPublish copies the entry to another volume and puts it in place.
//
// The copy goes to a name of its own and is put in place at the end, so
// that nothing of somebody else is ever written into or deleted. The source
// is removed only after the copy is in place.
func copyAndPublish(src, dst string, progress Progress) error {
	partial := dst + PartialSuffix
	if err := CopyTree(src, partial, progress); err != nil {
		os.RemoveAll(partial)
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		// Somebody put a file there while the copy was running.
		os.RemoveAll(partial)
		return fmt.Errorf("move %q: %w", dst, os.ErrExist)
	}
	if err := os.Rename(partial, dst); err != nil {
		os.RemoveAll(partial)
		return fmt.Errorf("move %q to %q: %w", src, dst, err)
	}
	// The name must reach the disk too, or a power cut would leave the file
	// without a name.
	syncDir(filepath.Dir(dst))
	if err := os.RemoveAll(src); err != nil {
		// The copy is whole and in place. The source is a leftover: it is
		// said apart from a failed copy, because the move did succeed.
		return fmt.Errorf("%w: %q: %w", ErrSourceNotRemoved, src, err)
	}
	return nil
}

// Size returns the size of the entry: the size of the file, or the sum of
// the sizes of the files in the tree.
func Size(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		size, _, err := treeStat(path)
		return size, err
	}
	return info.Size(), nil
}

// syncDir makes the changes in a directory reach the disk.
//
// Best effort: some filesystems refuse it, and a missing flush is not worth
// failing a move over.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// CopyTree copies src to dst.
//
// Symlinks are recreated as symlinks and never followed. File modes are
// preserved and modification times are restored best effort.
func CopyTree(src, dst string, progress Progress) error {
	copier := &copier{progress: progress}
	return copier.copy(src, dst)
}

type copier struct {
	progress Progress
	copied   int64
}

func (c *copier) add(n int64) {
	c.copied += n
	if c.progress != nil {
		c.progress(c.copied)
	}
}

func (c *copier) copy(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)

	case info.IsDir():
		// Mkdir, not MkdirAll: a directory that is already there means the
		// copy would mix our files with somebody else's.
		//
		// The mode of the source is set at the end: a directory that is
		// read-only would not take the files of the copy.
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		items, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := c.copy(filepath.Join(src, item.Name()), filepath.Join(dst, item.Name())); err != nil {
				return err
			}
		}
		if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
			return err
		}
		restoreTimes(src, dst)
		return nil

	case info.Mode().IsRegular():
		return c.copyFile(src, dst, info)

	default:
		return fmt.Errorf("copy %q: unsupported file type %s", src, info.Mode().Type())
	}
}

func (c *copier) copyFile(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// O_EXCL: the file must not be there yet, or the copy would overwrite
	// what is already there.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// io.Copy returns how many bytes it copied.
	written, err := io.Copy(out, in)
	c.add(written)
	// The data must reach the disk before the file is closed: the source is
	// removed after the copy, and a power cut in between must not leave a
	// file that is there but empty.
	if syncErr := out.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// The mode of the source is set here: the umask would have eaten it
	// when the file was created.
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	restoreTimes(src, dst)
	return nil
}

// restoreTimes copies the modification time of src onto dst.
//
// Best effort: some filesystems (an SMB mount, say) refuse it, and a missing
// timestamp is not worth failing a move over.
func restoreTimes(src, dst string) {
	info, err := os.Lstat(src)
	if err != nil {
		return
	}
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
}

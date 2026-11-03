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

// partialSuffix is the name the copy gets before it is put in place.
//
// The copy never writes into the destination itself: a destination that is
// somebody else's stays untouched, and a copy that was cut in the middle is
// picked up by the next attempt under a name the daemon owns.
const partialSuffix = ".drs-partial"

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
	os.RemoveAll(dst + partialSuffix)

	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("move %q to %q: %w", src, dst, err)
	}

	// The copy goes to a name of its own and is put in place at the end, so
	// that nothing of somebody else is ever written into or deleted.
	partial := dst + partialSuffix
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
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("remove the source %q: %w", src, err)
	}
	return nil
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
		if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
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
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, &countingReader{reader: in, counter: c})
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	restoreTimes(src, dst)
	return nil
}

// countingReader counts the bytes that go through it.
type countingReader struct {
	reader  io.Reader
	counter *copier
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.counter.add(int64(n))
	return n, err
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

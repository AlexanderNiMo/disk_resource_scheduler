package disk

import "golang.org/x/sys/unix"

// renameNoReplace leaves an existing destination untouched, even if it appears concurrently.
func renameNoReplace(src, dst string) error {
	return unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
}

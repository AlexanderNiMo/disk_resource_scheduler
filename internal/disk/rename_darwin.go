package disk

import "golang.org/x/sys/unix"

// renameNoReplace leaves an existing destination untouched, even if it appears concurrently.
func renameNoReplace(src, dst string) error { return unix.RenamexNp(src, dst, unix.RENAME_EXCL) }

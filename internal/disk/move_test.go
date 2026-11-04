package disk

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyTreeRecreatesSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")

	// The target of the link is outside the tree, so a copy that follows
	// the link would bring it along.
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	writeFile(t, outside, "xyz")

	writeFile(t, filepath.Join(src, "movie.mkv"), "abc")
	writeFile(t, filepath.Join(src, "Show.S01", "ep01.mkv"), "0123456789")
	if err := os.Symlink(outside, filepath.Join(src, "Show.S01", "link")); err != nil {
		t.Fatalf("make a symlink: %v", err)
	}

	if err := CopyTree(src, dst, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}

	link := filepath.Join(dst, "Show.S01", "link")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the link is missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is not a symlink: %s", info.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil || target != outside {
		t.Fatalf("the link points at %q, want %q", target, outside)
	}
}

func TestCopyTreeKeepsModeAndTime(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")

	writeFile(t, filepath.Join(src, "movie.mkv"), "abc")
	if err := os.Chmod(filepath.Join(src, "movie.mkv"), 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	when := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(filepath.Join(src, "movie.mkv"), when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := CopyTree(src, dst, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}

	info, err := os.Stat(filepath.Join(dst, "movie.mkv"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("the mode is %o, want 640", info.Mode().Perm())
	}
	if !info.ModTime().Truncate(time.Second).Equal(when) {
		t.Fatalf("the time is %s, want %s", info.ModTime(), when)
	}
}

// The umask must not eat the mode: a file that the group can write stays
// that way after the copy.
func TestCopyTreeKeepsTheModeOfAGroupWritableFile(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")

	writeFile(t, filepath.Join(src, "movie.mkv"), "abc")
	if err := os.Chmod(filepath.Join(src, "movie.mkv"), 0o664); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := CopyTree(src, dst, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}

	info, err := os.Stat(filepath.Join(dst, "movie.mkv"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o664 {
		t.Fatalf("the mode is %o, want 664", info.Mode().Perm())
	}
}

// A directory that is read-only still holds files: the copy puts them in
// and sets the mode at the end.
func TestCopyTreeOfAReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions")
	}
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")

	writeFile(t, filepath.Join(src, "Show.S01", "ep01.mkv"), "abc")
	if err := os.Chmod(filepath.Join(src, "Show.S01"), 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(filepath.Join(src, "Show.S01"), 0o755)

	if err := CopyTree(src, dst, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}
	// The copy is read-only, like the source: the test needs to be able to
	// take it away again.
	defer os.Chmod(filepath.Join(dst, "Show.S01"), 0o755)

	info, err := os.Stat(filepath.Join(dst, "Show.S01"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("the mode is %o, want 555", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dst, "Show.S01", "ep01.mkv")); err != nil {
		t.Fatalf("the file is not in the copy: %v", err)
	}
}

func TestCopyTreeFailsAndKeepsTheSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "movie.mkv"), "abc")

	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.MkdirAll(dst, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.Chmod(dst, 0o700)

	if err := CopyTree(src, dst, nil); err == nil {
		t.Fatal("want an error when the copy cannot be written")
	}
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
		t.Fatalf("the source is gone: %v", err)
	}
}

// The copy is in place and whole, but the source cannot be removed. The
// move says so apart from a failed copy: the entry IS at the destination.
func TestMoveSaysWhenTheSourceCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)
	writeFile(t, filepath.Join(src, "movie.mkv"), "hello")
	// The source cannot be removed: its directory is read-only.
	if err := os.Chmod(src, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(src, 0o755)

	// The copy of another volume: the copy goes to a name of its own and is
	// put in place at the end.
	err := copyAndPublish(filepath.Join(src, "movie.mkv"), filepath.Join(dst, "movie.mkv"), nil)
	if err == nil {
		t.Fatal("want an error when the source cannot be removed")
	}
	if !errors.Is(err, ErrSourceNotRemoved) {
		t.Fatalf("the move returned %v, want %v", err, ErrSourceNotRemoved)
	}
	// The copy is in place and whole.
	body, readErr := os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if readErr != nil || string(body) != "hello" {
		t.Fatalf("the file at the destination is %q %v, want the whole file", body, readErr)
	}
}

func TestMoveRefusesAnExistingTarget(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeFile(t, filepath.Join(src, "movie.mkv"), "abc")
	writeFile(t, filepath.Join(dst, "movie.mkv"), "other")

	if err := Move(filepath.Join(src, "movie.mkv"), filepath.Join(dst, "movie.mkv"), nil); err == nil {
		t.Fatal("want an error when the target exists")
	}
	body, err := os.ReadFile(filepath.Join(dst, "movie.mkv"))
	if err != nil || string(body) != "other" {
		t.Fatalf("the target was touched: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(src, "movie.mkv")); err != nil {
		t.Fatalf("the source is gone: %v", err)
	}
}

func TestMoveWithinOneFilesystem(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Show.S01", "ep01.mkv"), "0123456789")

	var copied int64
	err := Move(filepath.Join(dir, "Show.S01"), filepath.Join(dir, "moved"), func(n int64) { copied = n })
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Show.S01")); err == nil {
		t.Fatal("the source is still there")
	}
	body, err := os.ReadFile(filepath.Join(dir, "moved", "ep01.mkv"))
	if err != nil || string(body) != "0123456789" {
		t.Fatalf("the content is %q %v", body, err)
	}
	// A rename has nothing to report.
	if copied != 0 {
		t.Fatalf("a rename reported %d bytes copied", copied)
	}
}

func TestCopyTreeRefusesAnExistingDestination(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "movie.mkv"), "abc")

	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dst, "somebody-elses.mkv"), "not ours")

	if err := CopyTree(src, dst, nil); err == nil {
		t.Fatal("want an error when the destination is already there")
	}
	body, err := os.ReadFile(filepath.Join(dst, "somebody-elses.mkv"))
	if err != nil || string(body) != "not ours" {
		t.Fatalf("the file in the destination was touched: %q %v", body, err)
	}
}

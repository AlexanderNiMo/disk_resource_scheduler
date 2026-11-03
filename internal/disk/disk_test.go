package disk

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func TestListEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "movie.mkv"), "abc")
	writeFile(t, filepath.Join(dir, ".hidden"), "x")
	writeFile(t, filepath.Join(dir, "Show.S01", "ep01.mkv"), "0123456789")
	if err := os.Symlink("movie.mkv", filepath.Join(dir, "link")); err != nil {
		t.Fatalf("make a symlink: %v", err)
	}

	entries, err := ListEntries(dir, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}

	// os.ReadDir sorts by name: Show.S01, link, movie.mkv.
	if entries[0].Name != "Show.S01" || !entries[0].IsDir {
		t.Fatalf("the first entry is %+v, want the directory", entries[0])
	}
	if entries[0].Size != 10 {
		t.Fatalf("the directory is %d bytes, want 10", entries[0].Size)
	}
	if entries[1].Name != "link" || !entries[1].IsSymlink {
		t.Fatalf("the second entry is %+v, want the symlink", entries[1])
	}
	if entries[2].Name != "movie.mkv" || entries[2].Size != 3 {
		t.Fatalf("the third entry is %+v, want the file", entries[2])
	}
}

func TestListEntriesTakesTheNewestTimeInTheTree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Show.S01", "ep01.mkv"), "0123456789")

	// The directory's own mtime says nothing about the file inside it.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "Show.S01"), old, old); err != nil {
		t.Fatalf("set the directory time: %v", err)
	}

	entries, err := ListEntries(dir, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !entries[0].ModTime.After(old) {
		t.Fatalf("the time is %s, want something newer than %s", entries[0].ModTime, old)
	}
}

func TestVolumeOf(t *testing.T) {
	volume, err := StatfsProbe{}.VolumeOf(t.TempDir())
	if err != nil {
		t.Fatalf("volume: %v", err)
	}
	if volume.Free <= 0 || volume.Total <= 0 {
		t.Fatalf("the volume looks empty: %+v", volume)
	}
	if volume.Dev == 0 {
		t.Fatalf("the volume has no device: %+v", volume)
	}
}

func TestListEntriesSkipsTheLeftoversOfACopy(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "movie.mkv"), "abc")
	writeFile(t, filepath.Join(dir, "movie.mkv"+partialSuffix), "half a file")

	entries, err := ListEntries(dir, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "movie.mkv" {
		t.Fatalf("got %+v, want only the file", entries)
	}
}

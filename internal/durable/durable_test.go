package durable

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func restoreSeams(t *testing.T) {
	t.Helper()
	w, f, r, d := writeData, syncFile, renameFile, syncDir
	t.Cleanup(func() { writeData, syncFile, renameFile, syncDir = w, f, r, d })
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteFileReplacesContentsWithMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-a.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readFile(t, path); got != `{"new":true}` {
		t.Fatalf("contents = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
	assertNoTempFiles(t, dir)
}

// An open handle on the old file must still read the old bytes: the new
// contents land in a new inode through rename, never by truncating in place.
func TestWriteFileDoesNotTruncateInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename over an open file fails on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	if err := os.WriteFile(path, []byte("old-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "hardlink")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readFile(t, link); got != "old-contents" {
		t.Fatalf("old inode was rewritten in place: %q", got)
	}
	if got := readFile(t, path); got != "new" {
		t.Fatalf("contents = %q", got)
	}
}

func TestWriteFileInterruptedWriteKeepsOldFileWhole(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	old := `{"refresh_token":"old-but-valid"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	writeData = func(f *os.File, data []byte) error {
		if _, err := f.Write(data[:len(data)/2]); err != nil {
			return err
		}
		return errors.New("disk vanished mid-write")
	}
	err := WriteFile(path, []byte(`{"refresh_token":"new-rotated-token"}`), 0o600)
	if err == nil {
		t.Fatal("expected error from interrupted write")
	}
	if got := readFile(t, path); got != old {
		t.Fatalf("old file damaged: %q", got)
	}
	assertNoTempFiles(t, dir)
}

func TestWriteFileFailedFsyncKeepsOldFileWhole(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncFile = func(*os.File) error { return errors.New("EIO") }
	if err := WriteFile(path, []byte("new"), 0o600); err == nil {
		t.Fatal("expected fsync error")
	}
	if got := readFile(t, path); got != "old" {
		t.Fatalf("old file damaged: %q", got)
	}
	assertNoTempFiles(t, dir)
}

func TestWriteFileFsyncsFileBeforeRenameAndDirAfter(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	var calls []string
	writeData = func(f *os.File, data []byte) error {
		calls = append(calls, "write")
		_, err := f.Write(data)
		return err
	}
	syncFile = func(f *os.File) error {
		calls = append(calls, "fsync-file")
		return f.Sync()
	}
	renameFile = func(from, to string) error {
		calls = append(calls, "rename")
		return os.Rename(from, to)
	}
	syncDir = func(d string) error {
		calls = append(calls, "fsync-dir:"+d)
		return syncDirectory(d)
	}
	if err := WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want := []string{"write", "fsync-file", "rename", "fsync-dir:" + dir}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestWriteFileReportsDirFsyncFailure(t *testing.T) {
	restoreSeams(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	syncDir = func(string) error { return errors.New("EIO on dir") }
	err := WriteFile(path, []byte("x"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "fsync dir") {
		t.Fatalf("err = %v, want fsync dir error", err)
	}
}

func TestSyncDirectoryOnRealDir(t *testing.T) {
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatalf("syncDirectory: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := syncDirectory(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("expected error for a missing dir")
		}
	}
}

func TestWriteFileKeepsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if got := readFile(t, real); got != "new" {
		t.Fatalf("target contents = %q", got)
	}
}

// Package durabletest checks, from any package's tests, that an auth-file
// writer replaces the file through a rename instead of rewriting it in place.
package durabletest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// AssertReplacesFile seeds path with old, keeps a hard link to that inode,
// runs write, and returns the new contents. It fails t when the old inode
// saw the new bytes (an in-place rewrite that a crash can leave half done),
// when the new file is not mode 0600, or when a temp file is left behind.
func AssertReplacesFile(t testing.TB, path string, old []byte, write func() error) []byte {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hard link check needs a POSIX file system")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "old-inode")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}

	if err := write(); err != nil {
		t.Fatalf("write: %v", err)
	}

	if raw, _ := os.ReadFile(link); string(raw) != string(old) {
		t.Fatalf("old file was rewritten in place: %q", raw)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == string(old) {
		t.Fatalf("file still holds the old bytes")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	return raw
}

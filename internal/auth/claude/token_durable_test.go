package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// SaveTokenToFile must replace the auth file through a rename, not truncate
// it in place. A hard link to the old file keeps the old bytes only when the
// write lands in a new inode, which is what keeps the old refresh token whole
// if the process dies mid-write.
func TestSaveTokenToFileReplacesAtomically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard link check needs a POSIX file system")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-test.json")
	old := `{"refresh_token":"old-token"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "old-inode")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}

	ts := &ClaudeTokenStorage{RefreshToken: "new-token"}
	if err := ts.SaveTokenToFile(path); err != nil {
		t.Fatalf("SaveTokenToFile: %v", err)
	}

	if raw, _ := os.ReadFile(link); string(raw) != old {
		t.Fatalf("old file was rewritten in place: %q", raw)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("new file is not JSON: %v", err)
	}
	if got["refresh_token"] != "new-token" || got["type"] != "claude" {
		t.Fatalf("new file = %s", raw)
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
}

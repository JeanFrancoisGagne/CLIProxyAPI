package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Save on an existing metadata-only auth file must replace it through a
// rename. The old build opened it with O_TRUNC and wrote in place, so a crash
// between the truncate and the write left an empty file and lost the only
// copy of the refresh token. A hard link keeps the old bytes only when the
// new contents land in a new inode.
func TestFileTokenStoreSaveReplacesExistingFileAtomically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard link check needs a POSIX file system")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-x.json")
	old := `{"type":"claude","refresh_token":"old-token"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "old-inode")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auth := &cliproxyauth.Auth{
		ID:       "claude-x.json",
		Provider: "claude",
		FileName: "claude-x.json",
		Metadata: map[string]any{"type": "claude", "refresh_token": "new-token"},
	}
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatalf("Save: %v", err)
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
	if got["refresh_token"] != "new-token" {
		t.Fatalf("new file = %s", raw)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

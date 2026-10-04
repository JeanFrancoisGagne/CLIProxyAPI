package xai

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/durable/durabletest"
)

func TestSaveTokenToFileReplacesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xai-test.json")
	ts := &TokenStorage{RefreshToken: "new-token"}
	raw := durabletest.AssertReplacesFile(t, path, []byte(`{"refresh_token":"old-token"}`), func() error {
		return ts.SaveTokenToFile(path)
	})
	if !strings.Contains(string(raw), "new-token") {
		t.Fatalf("new file = %s", raw)
	}
}

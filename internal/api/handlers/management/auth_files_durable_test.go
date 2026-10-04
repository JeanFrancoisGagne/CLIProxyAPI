package management

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/durable/durabletest"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The upload path replaces an existing auth file through a rename.
func TestWriteAuthFileReplacesAtomically(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	path := filepath.Join(authDir, "codex-durable.json")
	raw := durabletest.AssertReplacesFile(t, path, []byte(`{"type":"codex","email":"old@example.com"}`), func() error {
		return h.writeAuthFile(context.Background(), "codex-durable.json", []byte(`{"type":"codex","email":"new@example.com"}`))
	})
	if !strings.Contains(string(raw), "new@example.com") {
		t.Fatalf("new file = %s", raw)
	}
}

// Turning a source auth file off or on replaces it through a rename.
func TestSetSourceAuthFileDisabledReplacesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-durable.json")
	raw := durabletest.AssertReplacesFile(t, path, []byte(`{"type":"claude","email":"a@example.com"}`), func() error {
		return setSourceAuthFileDisabled(path, true)
	})
	if !strings.Contains(string(raw), `"disabled":true`) {
		t.Fatalf("new file = %s", raw)
	}
}

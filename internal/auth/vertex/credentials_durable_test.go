package vertex

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/durable/durabletest"
)

func TestSaveTokenToFileReplacesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vertex-test.json")
	s := &VertexCredentialStorage{ServiceAccount: map[string]any{"client_email": "new@example.com"}}
	raw := durabletest.AssertReplacesFile(t, path, []byte(`{"client_email":"old@example.com"}`), func() error {
		return s.SaveTokenToFile(path)
	})
	if !strings.Contains(string(raw), "new@example.com") {
		t.Fatalf("new file = %s", raw)
	}
}

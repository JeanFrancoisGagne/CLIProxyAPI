package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// Each accepted refresh logs one info line naming the provider and auth, so
// operators can count refreshes without reading the file watcher's events.
func TestRefreshSuccessLogsOneLine(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(&unauthorizedRefreshExecutor{id: "claude"})
	registerReadinessAuth(t, m, readinessAuth("claude-a", "claude", "rt", time.Now().Add(3*time.Hour)))

	if _, err := m.ForceRefreshAuth(context.Background(), "claude-a"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	var lines []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.InfoLevel && strings.HasPrefix(entry.Message, "refreshed provider=") {
			lines = append(lines, entry.Message)
		}
	}
	if len(lines) != 1 || lines[0] != "refreshed provider=claude auth=claude-a" {
		t.Fatalf("refresh log lines = %q, want one for claude-a", lines)
	}
}

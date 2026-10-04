package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func readinessAuth(id, provider, refreshToken string, expires time.Time) *Auth {
	return &Auth{
		ID:       id,
		Provider: provider,
		Metadata: map[string]any{
			"access_token":  "access-" + id,
			"refresh_token": refreshToken,
			"expired":       expires.UTC().Format(time.RFC3339),
		},
	}
}

func registerReadinessAuth(t *testing.T, m *Manager, a *Auth) {
	t.Helper()
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatalf("register %s: %v", a.ID, err)
	}
}

func TestGrantReadinessCountsEachState(t *testing.T) {
	now := time.Now()
	m := NewManager(nil, nil, nil)
	registerReadinessAuth(t, m, readinessAuth("claude-live", "claude", "rt-live", now.Add(2*time.Hour)))
	registerReadinessAuth(t, m, readinessAuth("claude-soon", "claude", "rt-soon", now.Add(5*time.Minute)))
	registerReadinessAuth(t, m, readinessAuth("claude-gone", "claude", "rt-gone", now.Add(-time.Minute)))
	disabled := readinessAuth("claude-off", "claude", "rt-off", now.Add(2*time.Hour))
	disabled.Disabled = true
	registerReadinessAuth(t, m, disabled)
	registerReadinessAuth(t, m, readinessAuth("codex-live", "codex", "rt-codex", now.Add(240*time.Hour)))
	registerReadinessAuth(t, m, &Auth{ID: "gemini-key", Provider: "gemini", Attributes: map[string]string{"api_key": "k"}})

	got := m.GrantReadiness(now, 10*time.Minute)

	want := map[string]*GrantCounts{
		"claude": {Total: 4, Usable: 1, Expiring: 2, Disabled: 1, InvalidGrantIDs: []string{}, BlockedIDs: []string{}},
		"codex":  {Total: 1, Usable: 1, InvalidGrantIDs: []string{}, BlockedIDs: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GrantReadiness = %+v / %+v, want %+v / %+v", got["claude"], got["codex"], want["claude"], want["codex"])
	}
}

// A refresh answered with invalid_grant marks the grant dead even while its
// access token is still valid. Saves that keep the same refresh token (a
// priority patch) do not clear the mark; a new refresh token does.
func TestGrantReadinessInvalidGrantSticksUntilTokenChanges(t *testing.T) {
	now := time.Now()
	executor := &unauthorizedRefreshExecutor{
		id:         "claude",
		refreshErr: errors.New(`token refresh failed with status 400: {"error": "invalid_grant", "error_description": "Refresh token not found or invalid"}`),
	}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)
	registerReadinessAuth(t, m, readinessAuth("claude-a", "claude", "rt-spent", now.Add(3*time.Hour)))
	registerReadinessAuth(t, m, readinessAuth("claude-b", "claude", "rt-good", now.Add(3*time.Hour)))

	if _, err := m.ForceRefreshAuth(context.Background(), "claude-a"); err == nil {
		t.Fatal("expected the refresh to fail")
	}

	counts := m.GrantReadiness(now, 10*time.Minute)["claude"]
	if counts.InvalidGrant != 1 || counts.Usable != 1 || !reflect.DeepEqual(counts.InvalidGrantIDs, []string{"claude-a"}) {
		t.Fatalf("after invalid_grant: %+v", counts)
	}

	patched, _ := m.GetByID("claude-a")
	patched.Metadata["priority"] = 5
	patched.LastError = nil
	if _, err := m.Update(context.Background(), patched); err != nil {
		t.Fatalf("update: %v", err)
	}
	if counts = m.GrantReadiness(now, 10*time.Minute)["claude"]; counts.InvalidGrant != 1 {
		t.Fatalf("a save with the same refresh token cleared the mark: %+v", counts)
	}

	relogged, _ := m.GetByID("claude-a")
	relogged.Metadata["refresh_token"] = "rt-new-login"
	if _, err := m.Update(context.Background(), relogged); err != nil {
		t.Fatalf("update: %v", err)
	}
	if counts = m.GrantReadiness(now, 10*time.Minute)["claude"]; counts.InvalidGrant != 0 || counts.Usable != 2 {
		t.Fatalf("a new refresh token did not clear the mark: %+v", counts)
	}
}

func TestGrantReadinessTransportErrorIsNotInvalidGrant(t *testing.T) {
	now := time.Now()
	executor := &unauthorizedRefreshExecutor{id: "claude", refreshErr: errors.New("connection reset by peer")}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)
	registerReadinessAuth(t, m, readinessAuth("claude-a", "claude", "rt", now.Add(3*time.Hour)))

	_, _ = m.ForceRefreshAuth(context.Background(), "claude-a")

	if counts := m.GrantReadiness(now, 10*time.Minute)["claude"]; counts.InvalidGrant != 0 || counts.Usable != 1 {
		t.Fatalf("transport error counted as a dead grant: %+v", counts)
	}
}

package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// weeklyLimitExecutor answers every request with a 429 that asks for a long wait.
type weeklyLimitExecutor struct {
	*unauthorizedRefreshExecutor
	wait time.Duration
}

func (e weeklyLimitExecutor) Execute(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, a.ID)
	e.mu.Unlock()
	return cliproxyexecutor.Response{}, weeklyLimitError{wait: e.wait}
}

type weeklyLimitError struct{ wait time.Duration }

func (weeklyLimitError) Error() string   { return "status 429: rate_limit_error: weekly limit reached" }
func (weeklyLimitError) StatusCode() int { return http.StatusTooManyRequests }
func (e weeklyLimitError) RetryAfter() *time.Duration {
	d := e.wait
	return &d
}

func registerModelClient(t *testing.T, id, provider, model string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(id) })
}

// A grant cooling down after a 429 for longer than the lead time cannot
// serve, though its token is fine: it counts as Blocked, not Usable.
func TestGrantReadinessCooledGrantIsBlocked(t *testing.T) {
	model := "claude-readiness-cooldown"
	exec := weeklyLimitExecutor{unauthorizedRefreshExecutor: &unauthorizedRefreshExecutor{id: "claude"}, wait: 72 * time.Hour}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(exec)
	a := readinessAuth("claude-only", "claude", "rt-good", time.Now().Add(5*time.Hour))
	registerModelClient(t, a.ID, "claude", model)
	registerReadinessAuth(t, m, a)

	_, _ = m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if _, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("second request succeeded; the grant should be cooling down")
	}
	if calls := exec.ExecuteCalls(); len(calls) != 1 {
		t.Fatalf("executor calls = %v, want the grant skipped on the second request", calls)
	}

	got := m.GrantReadiness(time.Now(), 10*time.Minute)["claude"]
	want := &GrantCounts{Total: 1, Blocked: 1, InvalidGrantIDs: []string{}, BlockedIDs: []string{"b2c887d8587d"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GrantReadiness = %+v, want %+v", got, want)
	}
}

// A cooldown that ends inside the lead time leaves the grant usable.
func TestGrantReadinessShortCooldownStaysUsable(t *testing.T) {
	model := "claude-readiness-short-cooldown"
	exec := weeklyLimitExecutor{unauthorizedRefreshExecutor: &unauthorizedRefreshExecutor{id: "claude"}, wait: 2 * time.Minute}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(exec)
	a := readinessAuth("claude-only", "claude", "rt-good", time.Now().Add(5*time.Hour))
	registerModelClient(t, a.ID, "claude", model)
	registerReadinessAuth(t, m, a)

	_, _ = m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	got, _ := m.GetByID(a.ID)
	if !got.Unavailable || got.NextRetryAfter.IsZero() {
		t.Fatalf("auth not cooling down: unavailable=%v next_retry=%v", got.Unavailable, got.NextRetryAfter)
	}

	if counts := m.GrantReadiness(time.Now(), 10*time.Minute)["claude"]; counts.Usable != 1 || counts.Blocked != 0 {
		t.Fatalf("2 min cooldown: %+v, want usable", counts)
	}
}

// A 401 whose refresh also answers 401 (no invalid_grant) puts the grant in a
// 30 min cooldown though its access token runs for hours. No invalid_grant
// mark exists, so only the Blocked bucket keeps it out of Usable.
func TestGrantReadinessUnauthorizedCooldownIsBlocked(t *testing.T) {
	m, _, primary, backup, model := newUnauthorizedRefreshFixture(t, true)
	updated, _ := m.GetByID(primary.ID)
	updated.Metadata["expired"] = time.Now().Add(6 * time.Hour).Format(time.RFC3339)
	if _, err := m.Update(context.Background(), updated); err != nil {
		t.Fatalf("update primary: %v", err)
	}
	if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	final, _ := m.GetByID(primary.ID)
	if !final.Unavailable || !final.NextRetryAfter.After(time.Now().Add(10*time.Minute)) {
		t.Fatalf("primary not cooling down past the lead: unavailable=%v next_retry=%v", final.Unavailable, final.NextRetryAfter)
	}

	counts := m.GrantReadiness(time.Now(), 10*time.Minute)["codex"]
	want := &GrantCounts{Total: 2, Usable: 1, Blocked: 1, InvalidGrantIDs: []string{}, BlockedIDs: []string{"898f602edbd8"}}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("after a 401 cooldown: %+v, want %+v (%s usable)", counts, want, backup.ID)
	}
}

// A grant in the terminal 401 state (no retry or refresh scheduled) is never
// selected, whatever its access token expiry says.
func TestGrantReadinessTerminalUnauthorizedIsBlocked(t *testing.T) {
	m := NewManager(nil, nil, nil)
	held := readinessAuth("claude-held", "claude", "rt-held", time.Now().Add(5*time.Hour))
	held.Unavailable = true
	held.Status = StatusError
	held.LastError = &Error{Code: "unauthorized", Message: "token revoked", HTTPStatus: http.StatusUnauthorized}
	registerReadinessAuth(t, m, held)
	got, _ := m.GetByID(held.ID)
	if !hasUnauthorizedAuthFailure(got) {
		t.Fatalf("registered auth is not in the terminal 401 state: %+v", got)
	}

	counts := m.GrantReadiness(time.Now(), 10*time.Minute)["claude"]
	if counts.Usable != 0 || counts.Blocked != 1 || !reflect.DeepEqual(counts.BlockedIDs, []string{"1580079c7a62"}) {
		t.Fatalf("terminal 401: %+v, want blocked", counts)
	}
}

// Codex answers a spent single-use refresh token with refresh_token_reused.
// That grant is as dead as one that drew invalid_grant.
func TestGrantReadinessCodexRefreshTokenReusedIsInvalidGrant(t *testing.T) {
	now := time.Now()
	executor := &unauthorizedRefreshExecutor{
		id:         "codex",
		refreshErr: errors.New(`token refresh failed with status 401: {"error":{"code":"refresh_token_reused"}}`),
	}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)
	registerReadinessAuth(t, m, readinessAuth("codex-a", "codex", "rt-spent", now.Add(3*time.Hour)))

	_, _ = m.ForceRefreshAuth(context.Background(), "codex-a")

	counts := m.GrantReadiness(now, 10*time.Minute)["codex"]
	if counts.InvalidGrant != 1 || counts.Usable != 0 || !reflect.DeepEqual(counts.InvalidGrantIDs, []string{"3bcf6776aae8"}) {
		t.Fatalf("after refresh_token_reused: %+v", counts)
	}
}

// A refresh the provider accepts clears the mark even when the response keeps
// the same refresh token, as Claude's does when it omits refresh_token.
func TestGrantReadinessSuccessfulRefreshClearsMark(t *testing.T) {
	now := time.Now()
	executor := &unauthorizedRefreshExecutor{
		id:         "claude",
		refreshErr: errors.New(`token refresh failed with status 400: {"error": "invalid_grant"}`),
	}
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(executor)
	registerReadinessAuth(t, m, readinessAuth("claude-a", "claude", "rt-same", now.Add(3*time.Hour)))

	if _, err := m.ForceRefreshAuth(context.Background(), "claude-a"); err == nil {
		t.Fatal("expected the first refresh to fail")
	}
	if counts := m.GrantReadiness(now, 10*time.Minute)["claude"]; counts.InvalidGrant != 1 {
		t.Fatalf("after invalid_grant: %+v", counts)
	}
	executor.mu.Lock()
	executor.refreshErr = nil
	executor.mu.Unlock()
	if _, err := m.ForceRefreshAuth(context.Background(), "claude-a"); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	got, _ := m.GetByID("claude-a")
	if authRefreshToken(got) != "rt-same" {
		t.Fatalf("refresh token changed to %q; the test needs it kept", authRefreshToken(got))
	}
	if counts := m.GrantReadiness(now, 10*time.Minute)["claude"]; counts.InvalidGrant != 0 || counts.Usable != 1 {
		t.Fatalf("after a successful refresh with the same token: %+v, want usable", counts)
	}
}

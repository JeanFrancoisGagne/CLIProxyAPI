package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestManager_MarkResultClaudeThreadNotFoundKeepsAvailability(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, model := range []string{"", "claude-opus-5-5"} {
		for _, code := range []string{"", "upstream_error"} {
			scope := "auth"
			if model != "" {
				scope = "model"
			}
			errorKind := "plain"
			if code != "" {
				errorKind = "coded"
			}
			t.Run(scope+"/"+errorKind, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				auth := &Auth{ID: "thread-mark-result", Provider: "claude", Status: StatusActive}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
				t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
				manager.MarkResult(context.Background(), Result{
					AuthID: auth.ID, Provider: auth.Provider, Model: model,
					Error: &Error{Code: code, HTTPStatus: http.StatusNotFound, Message: `{"type":"error","error":{"type":"not_found_error","details":{"error_code":"thread_not_found"}}}`},
				})
				updated, ok := manager.GetByID(auth.ID)
				if !ok || updated == nil {
					t.Fatal("auth was removed")
				}
				if updated.Unavailable || !updated.NextRetryAfter.IsZero() || updated.Status != StatusActive || updated.LastError != nil || updated.Quota.Exceeded || len(updated.ModelStates) != 0 {
					t.Fatalf("thread error changed availability: status=%s unavailable=%v retry=%v models=%d", updated.Status, updated.Unavailable, updated.NextRetryAfter, len(updated.ModelStates))
				}
				if updated.Failed != 1 {
					t.Fatalf("failed count = %d, want 1", updated.Failed)
				}
				if reg.IsModelSuspendedForClient(auth.ID, "claude-opus-5-5") {
					t.Fatal("thread error suspended the model in the registry")
				}
			})
		}
	}
}

package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// claudeThreadNotFoundBody is the verbatim 404 body from the 2026-10-02 incident:
// Anthropic keeps thread state per account, so the thread cannot exist on any
// other credential and only the client can recover (replay with thread create).
const claudeThreadNotFoundBody = `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested ` + "`previous_message_id`" + `. Replay the full conversation with ` + "`thread: {\\\"type\\\": \\\"create\\\"}`" + ` to start a new Thread.","details":{"error_code":"thread_not_found"}},"request_id":"req_011Cfde"}`

// genericNotFoundBody is a 404 nothing classifies: neither model support nor
// missing conversation state.
const genericNotFoundBody = `{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`

const modelNotFoundBody = `{"error":{"type":"not_found_error","code":"model_not_found","message":"The model does not exist."}}`

// upstreamStatusErr mirrors the executors' statusErr: Error() is the raw
// upstream body and StatusCode() the upstream HTTP status.
type upstreamStatusErr struct {
	code int
	body string
}

func (e upstreamStatusErr) Error() string   { return e.body }
func (e upstreamStatusErr) StatusCode() int { return e.code }

// scriptedStatusExecutor answers every call with failFor(callIndex), or success
// when it returns nil. ExecuteStream fails before the stream starts, as the
// Claude executor does for a non-2xx upstream response.
type scriptedStatusExecutor struct {
	id      string
	failFor func(call int) error

	mu    sync.Mutex
	calls []string
}

func (e *scriptedStatusExecutor) Identifier() string { return e.id }

func (e *scriptedStatusExecutor) next(authID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	call := len(e.calls)
	e.calls = append(e.calls, authID)
	return e.failFor(call)
}

func (e *scriptedStatusExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.next(auth.ID); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *scriptedStatusExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.next(auth.ID); err != nil {
		return nil, err
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: ch}, nil
}

func (e *scriptedStatusExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *scriptedStatusExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *scriptedStatusExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *scriptedStatusExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func alwaysFail(err error) func(int) error {
	return func(int) error { return err }
}

// newRequestScopedTestManager registers count Claude credentials serving model
// with cooldowns enabled, so a wrong classification shows up as a cooldown.
func newRequestScopedTestManager(t *testing.T, model string, count int, failFor func(int) error) (*Manager, *scriptedStatusExecutor, []string) {
	t.Helper()
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	m := NewManager(nil, nil, nil)
	executor := &scriptedStatusExecutor{id: "claude", failFor: failFor}
	m.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-auth-%d", model, i)
		reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, errRegister := m.Register(context.Background(), &Auth{ID: id, Provider: "claude"}); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
		ids = append(ids, id)
	}
	return m, executor, ids
}

func assertNoRequestCooldown(t *testing.T, m *Manager, authID, model string) {
	t.Helper()
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %s missing", authID)
	}
	if !auth.NextRetryAfter.IsZero() || auth.Unavailable || auth.StatusMessage == "not_found" {
		t.Errorf("auth %s cooled down: nextRetryAfter=%v unavailable=%v status=%q", authID, auth.NextRetryAfter, auth.Unavailable, auth.StatusMessage)
	}
	if state := auth.ModelStates[model]; state != nil && (!state.NextRetryAfter.IsZero() || state.Unavailable) {
		t.Errorf("auth %s model %s cooled down until %v (unavailable=%v)", authID, model, state.NextRetryAfter, state.Unavailable)
	}
}

func assertRequestModelCooldown(t *testing.T, m *Manager, authID, model string) {
	t.Helper()
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %s missing", authID)
	}
	state := auth.ModelStates[model]
	if state == nil || state.NextRetryAfter.IsZero() {
		t.Errorf("auth %s model %s has no cooldown, want one", authID, model)
	}
}

func assertNotFoundReturned(t *testing.T, err error, wantBody string) {
	t.Helper()
	if err == nil {
		t.Error("expected the upstream 404, got nil")
		return
	}
	if status := statusCodeFromError(err); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (err = %v)", status, err)
	}
	if !strings.Contains(err.Error(), wantBody) {
		t.Errorf("error = %q, want the upstream body %q", err.Error(), wantBody)
	}
}

func executeOrStream(m *Manager, stream bool, model string) error {
	ctx := context.Background()
	req := cliproxyexecutor.Request{Model: model}
	if !stream {
		_, err := m.Execute(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
		return err
	}
	result, err := m.ExecuteStream(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
	if err != nil {
		return err
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return chunk.Err
		}
	}
	return nil
}

// The incident: one thread_not_found 404 put six credentials into 12-hour
// cooldowns and the client got auth_unavailable instead of the 404 it needed.
func TestManager_ClaudeThreadNotFound_ReturnsUpstream404WithoutRotationOrCooldown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			model := fmt.Sprintf("claude-thread-404-stream-%t", stream)
			threadErr := upstreamStatusErr{code: http.StatusNotFound, body: claudeThreadNotFoundBody}
			m, executor, ids := newRequestScopedTestManager(t, model, 6, alwaysFail(threadErr))

			err := executeOrStream(m, stream, model)

			assertNotFoundReturned(t, err, "thread_not_found")
			if calls := executor.Calls(); len(calls) != 1 {
				t.Errorf("upstream calls = %v, want exactly 1 (no rotation)", calls)
			}
			for _, id := range ids {
				assertNoRequestCooldown(t, m, id, model)
			}
		})
	}
}

func TestManager_MarkResult_ClaudeThreadNotFoundIsRequestScoped(t *testing.T) {
	model := "claude-thread-404-mark-result"
	m, _, ids := newRequestScopedTestManager(t, model, 1, alwaysFail(nil))
	threadErr := upstreamStatusErr{code: http.StatusNotFound, body: claudeThreadNotFoundBody}

	if !isRequestInvalidError(threadErr) {
		t.Error("isRequestInvalidError(thread_not_found) = false, want true (stops rotation)")
	}
	resultErr := resultErrorFromError(threadErr)
	if !shouldSkipCredentialCooldown(resultErr) {
		t.Errorf("shouldSkipCredentialCooldown(%#v) = false, want true (keeps cooldown and session binding)", resultErr)
	}
	// A result built without the request_scoped code still skips cooldown.
	if !shouldSkipCredentialCooldown(&Error{HTTPStatus: http.StatusNotFound, Message: claudeThreadNotFoundBody}) {
		t.Error("shouldSkipCredentialCooldown(raw thread_not_found result) = false, want true")
	}

	m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: model, Error: resultErr})
	assertNoRequestCooldown(t, m, ids[0], model)
}

// model_not_found is a credential-model mismatch: it must keep rotating to the
// next credential and cool the model on the credential that lacks it.
func TestManager_ModelNotFound404StillRotatesAndCoolsDown(t *testing.T) {
	model := "claude-model-not-found-rotates"
	modelErr := upstreamStatusErr{code: http.StatusNotFound, body: modelNotFoundBody}
	m, executor, _ := newRequestScopedTestManager(t, model, 3, func(call int) error {
		if call < 2 {
			return modelErr
		}
		return nil
	})

	if err := executeOrStream(m, false, model); err != nil {
		t.Fatalf("Execute error = %v, want success on the third credential", err)
	}
	calls := executor.Calls()
	if len(calls) != 3 {
		t.Fatalf("upstream calls = %v, want 3 (rotate past both model_not_found credentials)", calls)
	}
	assertRequestModelCooldown(t, m, calls[0], model)
	assertRequestModelCooldown(t, m, calls[1], model)
	assertNoRequestCooldown(t, m, calls[2], model)
}

// Second guard: a 404 that nothing classifies, answered by two credentials in a
// row for one request, is a property of the request. Stop after the second and
// cool neither credential.
func TestManager_Repeated404AcrossCredentialsStopsRotationWithoutCooldown(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			model := fmt.Sprintf("claude-repeated-404-stream-%t", stream)
			notFound := upstreamStatusErr{code: http.StatusNotFound, body: genericNotFoundBody}
			m, executor, ids := newRequestScopedTestManager(t, model, 6, alwaysFail(notFound))

			err := executeOrStream(m, stream, model)

			assertNotFoundReturned(t, err, "Not Found")
			if calls := executor.Calls(); len(calls) != 2 {
				t.Errorf("upstream calls = %v (%d), want exactly 2", calls, len(calls))
			}
			for _, id := range ids {
				assertNoRequestCooldown(t, m, id, model)
			}
		})
	}
}

// A 404 followed by a success on the next credential is credential specific:
// the first credential keeps its not-found cooldown.
func TestManager_Single404ThenSuccessStillCoolsFirstCredential(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			model := fmt.Sprintf("claude-single-404-stream-%t", stream)
			notFound := upstreamStatusErr{code: http.StatusNotFound, body: genericNotFoundBody}
			m, executor, _ := newRequestScopedTestManager(t, model, 3, func(call int) error {
				if call == 0 {
					return notFound
				}
				return nil
			})

			if err := executeOrStream(m, stream, model); err != nil {
				t.Fatalf("error = %v, want success on the second credential", err)
			}
			calls := executor.Calls()
			if len(calls) != 2 {
				t.Fatalf("upstream calls = %v, want 2", calls)
			}
			assertRequestModelCooldown(t, m, calls[0], model)
			assertNoRequestCooldown(t, m, calls[1], model)
		})
	}
}

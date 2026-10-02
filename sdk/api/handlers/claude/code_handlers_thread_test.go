package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

const missingClaudeThreadBody = `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested previous_message_id. Replay the full conversation with thread type create.","details":{"error_code":"thread_not_found"}}}`

type missingClaudeThreadExecutor struct{}

func (*missingClaudeThreadExecutor) Identifier() string { return "claude" }
func (*missingClaudeThreadExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{HTTPStatus: http.StatusNotFound, Message: missingClaudeThreadBody}
}
func (*missingClaudeThreadExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, &coreauth.Error{HTTPStatus: http.StatusNotFound, Message: missingClaudeThreadBody}
}
func (*missingClaudeThreadExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (*missingClaudeThreadExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}
func (*missingClaudeThreadExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestClaudeMessagesPreservesThreadRecoveryDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		streamValue := "false"
		if stream {
			name, streamValue = "initial streaming failure", "true"
		}
		t.Run(name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(&missingClaudeThreadExecutor{})
			auth := &coreauth.Auth{ID: "thread-recovery-handler", Provider: "claude", Status: coreauth.StatusActive}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
			router := gin.New()
			router.POST("/v1/messages", handler.ClaudeMessages)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","thread":{"type":"continue","previous_message_id":"msg_missing"},"messages":[{"role":"user","content":"continue"}],"stream":`+streamValue+"}"))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
			}
			assertClaudeThreadRecoveryBody(t, recorder.Body.Bytes())
		})
	}
}

func TestClaudeStreamTerminalErrorPreservesThreadRecoveryDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Header("Content-Type", "text/event-stream")
	_, _ = c.Writer.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	c.Writer.Flush()
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusNotFound, Error: errors.New(missingClaudeThreadBody)}
	close(errs)
	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	handler.forwardClaudeStream(c, c.Writer, func(error) {}, nil, errs)
	if recorder.Code != http.StatusOK {
		t.Fatalf("started stream status = %d, want 200", recorder.Code)
	}
	marker := "event: error\ndata: "
	_, body, ok := strings.Cut(recorder.Body.String(), marker)
	if !ok {
		t.Fatalf("missing terminal event: %s", recorder.Body.String())
	}
	assertClaudeThreadRecoveryBody(t, []byte(strings.TrimSpace(body)))
}

func assertClaudeThreadRecoveryBody(t *testing.T, body []byte) {
	t.Helper()
	if got := gjson.GetBytes(body, "error.type").String(); got != "not_found_error" {
		t.Fatalf("error.type = %q; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != gjson.Get(missingClaudeThreadBody, "error.message").String() {
		t.Fatalf("message changed: %q", got)
	}
	// Claude Code only replays a missing thread after this machine-readable code.
	if got := gjson.GetBytes(body, "error.details.error_code").String(); got != "thread_not_found" {
		t.Fatalf("recovery code = %q, want thread_not_found; body=%s", got, body)
	}
}

func TestWriteClaudeErrorResponsePreservesAndNormalizesRecoveryDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantCode    string
		wantDetails string
	}{
		{"structured unsupported", http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"thread is unsupported","details":{"error_code":"thread_unsupported_request","field":"thread","sequence":9007199254740993}}}`, "thread_unsupported_request", `{"error_code":"thread_unsupported_request","field":"thread","sequence":9007199254740993}`},
		{"message only", http.StatusNotFound, strings.ReplaceAll(`{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested previous_message_id."}}`, "previous_message_id", "`previous_message_id`"), "thread_not_found", ""},
		{"plain message", http.StatusNotFound, "No thread state was found for the requested `previous_message_id`.", "thread_not_found", ""},
		{"ordinary missing", http.StatusNotFound, `{"error":{"type":"not_found_error","message":"Not Found"}}`, "", ""},
		{"quota stays quota", http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"No thread state was found for the requested previous_message_id."}}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			handler := &ClaudeCodeAPIHandler{}
			handler.WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: tc.status, Error: errors.New(tc.body)})
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.status)
			}
			body := recorder.Body.Bytes()
			if got := gjson.GetBytes(body, "error.details.error_code").String(); got != tc.wantCode {
				t.Fatalf("code = %q, want %q; body=%s", got, tc.wantCode, body)
			}
			if tc.wantDetails != "" && gjson.GetBytes(body, "error.details").Raw != tc.wantDetails {
				t.Fatalf("details changed: %s", body)
			}
			if tc.wantCode == "" && gjson.GetBytes(body, "error.details").Exists() {
				t.Fatalf("unrelated error gained details: %s", body)
			}
		})
	}
}

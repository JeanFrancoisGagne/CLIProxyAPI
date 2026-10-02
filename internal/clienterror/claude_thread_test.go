package clienterror

import (
	"errors"
	"net/http"
	"testing"
)

func TestIsRequestFaultClaudeThreadNotFound(t *testing.T) {
	const threadError = `{"type":"not_found_error","message":"No thread state was found for the requested previous_message_id. Replay the full conversation with thread type create.","details":{"error_code":"thread_not_found"}}`
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "anthropic error envelope", status: http.StatusNotFound, body: `{"type":"error","error":` + threadError + `}`, want: true},
		{name: "bare error", status: http.StatusNotFound, body: threadError, want: true},
		{name: "response error envelope", status: http.StatusNotFound, body: `{"response":{"error":` + threadError + `}}`, want: true},
		{name: "body error envelope", status: http.StatusNotFound, body: `{"body":{"error":` + threadError + `}}`, want: true},
		{name: "ordinary not found", status: http.StatusNotFound, body: `{"error":{"type":"not_found_error","message":"Not Found"}}`},
		{name: "message only", status: http.StatusNotFound, body: "{\"error\":{\"type\":\"not_found_error\",\"message\":\"No thread state was found for the requested `previous_message_id`. Replay the full conversation with thread type create.\"}}", want: true},
		{name: "plain upstream message", status: http.StatusNotFound, body: "No thread state was found for the requested `previous_message_id`. Replay the full conversation with thread type create.", want: true},
		{name: "unrelated mention", status: http.StatusNotFound, body: "example error: No thread state was found for the requested `previous_message_id`."},
		{name: "unrelated details", status: http.StatusNotFound, body: `{"error":{"type":"not_found_error","details":{"error_code":"resource_not_found"}}}`},
		{name: "direct code", status: http.StatusNotFound, body: `{"error":{"code":"thread_not_found"}}`, want: true},
		{name: "invalid JSON", status: http.StatusNotFound, body: `{"error":{"type":"not_found_error","details":{"error_code":"thread_not_found"}}`},
		{name: "401 stays credential scoped", status: http.StatusUnauthorized, body: `{"error":{"type":"authentication_error","details":{"error_code":"thread_not_found"}}}`},
		{name: "429 stays quota scoped", status: http.StatusTooManyRequests, body: `{"error":` + threadError + `}`},
		{name: "401 with thread identifier stays credential scoped", status: http.StatusUnauthorized, body: `{"error":` + threadError + `}`},
		{name: "403 with thread identifier stays credential scoped", status: http.StatusForbidden, body: `{"error":` + threadError + `}`},
		{name: "500 with thread identifier stays server scoped", status: http.StatusInternalServerError, body: `{"error":` + threadError + `}`},
		{name: "401 with message stays credential scoped", status: http.StatusUnauthorized, body: "No thread state was found for the requested `previous_message_id`."},
		{name: "403 with message stays credential scoped", status: http.StatusForbidden, body: "No thread state was found for the requested `previous_message_id`."},
		{name: "429 with message stays quota scoped", status: http.StatusTooManyRequests, body: "No thread state was found for the requested `previous_message_id`."},
		{name: "500 with message stays server scoped", status: http.StatusInternalServerError, body: "No thread state was found for the requested `previous_message_id`."},

		{name: "model not found stays model scoped", status: http.StatusNotFound, body: `{"error":{"type":"not_found_error","code":"model_not_found","message":"model claude-missing was not found"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRequestFault(tc.status, errors.New(tc.body)); got != tc.want {
				t.Fatalf("IsRequestFault() = %v, want %v", got, tc.want)
			}
		})
	}
}

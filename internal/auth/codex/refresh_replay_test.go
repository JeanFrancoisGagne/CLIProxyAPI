package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingBody) Close() error             { return nil }

func statusResponse(status int, body string) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
}

// OpenAI refresh tokens are single use. After an error that leaves open
// whether the server rotated the token, a replay of the same token draws
// refresh_token_reused and kills the grant, so RefreshTokensWithRetry must
// send it once. Only a 500 or 503 answer is retried.
func TestRefreshTokensWithRetry_DoesNotReplayAmbiguousFailures(t *testing.T) {
	cases := []struct {
		name      string
		respond   func(*http.Request) (*http.Response, error)
		wantCalls int32
	}{
		{name: "transport error", wantCalls: 1, respond: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset by peer")
		}},
		{name: "body read error", wantCalls: 1, respond: func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: failingBody{}, Header: make(http.Header), Request: req}, nil
		}},
		{name: "json decode error", wantCalls: 1, respond: statusResponse(http.StatusOK, `not json`)},
		{name: "502 from a gateway", wantCalls: 1, respond: statusResponse(http.StatusBadGateway, `bad gateway`)},
		{name: "504 from a gateway", wantCalls: 1, respond: statusResponse(http.StatusGatewayTimeout, `gateway timeout`)},
		{name: "400 invalid_grant", wantCalls: 1, respond: statusResponse(http.StatusBadRequest, `{"error":"invalid_grant"}`)},
		{name: "500 from the token server", wantCalls: 3, respond: statusResponse(http.StatusInternalServerError, `oops`)},
		{name: "503 from the token server", wantCalls: 3, respond: statusResponse(http.StatusServiceUnavailable, `busy`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCodexRefreshGroupForTest()
			t.Cleanup(resetCodexRefreshGroupForTest)
			var calls int32
			auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return tc.respond(req)
			})}}

			_, err := auth.RefreshTokensWithRetry(context.Background(), "single-use-refresh-token", 3)
			if err == nil {
				t.Fatal("expected refresh error")
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
				t.Fatalf("refresh attempts with the same token = %d, want %d (err %v)", got, tc.wantCalls, err)
			}
		})
	}
}

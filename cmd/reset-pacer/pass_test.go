package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProxy serves the three management routes the pacer uses.
type fakeProxy struct {
	t       *testing.T
	key     string
	files   []map[string]any
	usage   map[string]apiCallResponse // by auth_index
	mu      sync.Mutex
	calls   []apiCallRequest
	patches []map[string]any
}

func (f *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid management key"}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /v0/management/auth-files":
		json.NewEncoder(w).Encode(map[string]any{"files": f.files})
	case "POST /v0/management/api-call":
		var req apiCallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Errorf("api-call body: %v", err)
		}
		f.calls = append(f.calls, req)
		resp, ok := f.usage[req.AuthIndex]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"auth token not found"}`)
			return
		}
		json.NewEncoder(w).Encode(resp)
	case "PATCH /v0/management/auth-files/fields":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("patch body: %v", err)
		}
		f.patches = append(f.patches, body)
		fmt.Fprint(w, `{"status":"ok"}`)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func claudeBody(five, seven float64, reset time.Time) string {
	return fmt.Sprintf(`{"five_hour":{"utilization":%g,"resets_at":null},"seven_day":{"utilization":%g,"resets_at":%q}}`,
		five, seven, reset.Format(time.RFC3339))
}

func newFakeProxy(t *testing.T) *fakeProxy {
	now := time.Now()
	return &fakeProxy{
		t:   t,
		key: "k",
		files: []map[string]any{
			{"name": "claude-a@example.com.json", "provider": "claude", "email": "alice@example.com", "auth_index": "ia", "priority": 90, "account_type": "oauth"},
			{"name": "claude-b@example.com.json", "provider": "claude", "email": "bob@example.com", "auth_index": "ib", "priority": 10, "account_type": "oauth"},
			{"name": "claude-off.json", "provider": "claude", "email": "off@example.com", "auth_index": "ioff", "priority": 50, "disabled": true},
			{"name": "codex-x.json", "provider": "codex", "email": "xavier@example.com", "auth_index": "ix", "account_type": "oauth",
				"id_token": map[string]any{"chatgpt_account_id": "acct-123", "plan_type": "pro"}},
			{"name": "gemini-g.json", "provider": "gemini", "auth_index": "ig", "priority": 5},
		},
		usage: map[string]apiCallResponse{
			// b resets sooner than a, so b should move to 100 and a to 90 (unchanged).
			"ia": {StatusCode: 200, Body: claudeBody(10, 20, now.Add(72*time.Hour))},
			"ib": {StatusCode: 200, Body: claudeBody(10, 60, now.Add(24*time.Hour))},
			"ix": {StatusCode: 429, Body: `{"error":{"message":"rate limited"}}`},
		},
	}
}

func newTestPacer(srv *httptest.Server, key string, dryRun bool, out io.Writer) *pacer {
	return &pacer{client: newMgmtClient(srv.URL+"/", key), out: out, dryRun: dryRun, now: time.Now}
}

func TestPassPatchesOnlyChangedPriorities(t *testing.T) {
	fake := newFakeProxy(t)
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var out bytes.Buffer
	if err := newTestPacer(srv, "k", false, &out).pass(context.Background()); err != nil {
		t.Fatalf("pass: %v\n%s", err, out.String())
	}

	// Usage is fetched for the enabled Claude and Codex credentials only.
	gotIdx := map[string]apiCallRequest{}
	for _, c := range fake.calls {
		gotIdx[c.AuthIndex] = c
	}
	if len(fake.calls) != 3 || gotIdx["ia"].URL != claudeUsageURL || gotIdx["ib"].URL != claudeUsageURL || gotIdx["ix"].URL != codexUsageURL {
		t.Fatalf("api-calls = %+v, want ia and ib on the Claude usage URL and ix on the Codex one", fake.calls)
	}
	if h := gotIdx["ia"].Header; h["Authorization"] != "Bearer $TOKEN$" || h["anthropic-beta"] != "oauth-2025-04-20" {
		t.Errorf("claude headers = %v", h)
	}
	if h := gotIdx["ix"].Header; h["Authorization"] != "Bearer $TOKEN$" || h["Chatgpt-Account-Id"] != "acct-123" {
		t.Errorf("codex headers = %v", h)
	}

	// b: 10 -> 100. x: unset -> 10 (usage call failed). a stays at 90, no PATCH.
	want := map[string]float64{"claude-b@example.com.json": 100, "codex-x.json": 10}
	if len(fake.patches) != len(want) {
		t.Fatalf("patches = %v, want %v", fake.patches, want)
	}
	for _, p := range fake.patches {
		if len(p) != 2 {
			t.Errorf("patch %v carries more than name and priority", p)
		}
		name, _ := p["name"].(string)
		if got, _ := p["priority"].(float64); got != want[name] {
			t.Errorf("patch %s priority %v, want %v", name, p["priority"], want[name])
		}
	}

	report := out.String()
	t.Logf("report:\n%s", report)
	for _, s := range []string{"al***@example.com", "10 -> 100", "90 -> 90", "50 -> 50", "disabled, left alone", "upstream 429"} {
		if !strings.Contains(report, s) {
			t.Errorf("report lacks %q:\n%s", s, report)
		}
	}
	for _, s := range []string{"alice@", "gemini"} {
		if strings.Contains(report, s) {
			t.Errorf("report contains %q:\n%s", s, report)
		}
	}
}

func TestPassDryRunWritesNothing(t *testing.T) {
	fake := newFakeProxy(t)
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var out bytes.Buffer
	if err := newTestPacer(srv, "k", true, &out).pass(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(fake.patches) != 0 {
		t.Errorf("dry run sent patches: %v", fake.patches)
	}
	if !strings.Contains(out.String(), "dry-run, not written") {
		t.Errorf("report does not mark the dry run:\n%s", out.String())
	}
}

func TestPassStopsAtRefusedKey(t *testing.T) {
	fake := newFakeProxy(t)
	srv := httptest.NewServer(fake)
	defer srv.Close()

	err := newTestPacer(srv, "wrong", false, io.Discard).pass(context.Background())
	if !errors.Is(err, errManagementAuth) {
		t.Fatalf("err = %v, want errManagementAuth", err)
	}
	if len(fake.calls) != 0 || len(fake.patches) != 0 {
		t.Errorf("refused key still made %d api-calls and %d patches", len(fake.calls), len(fake.patches))
	}
}

func TestRunReadsClipProxyMgmtKey(t *testing.T) {
	fake := newFakeProxy(t)
	srv := httptest.NewServer(fake)
	defer srv.Close()

	t.Setenv("CPA_MGMT_KEY", "")
	t.Setenv("CLIPROXY_MGMT_KEY", "k")
	t.Setenv("CPA_BASE", srv.URL)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-once", "-dry-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	if len(fake.calls) == 0 {
		t.Error("no usage calls made")
	}
}

func TestRunWithoutKeyExits2(t *testing.T) {
	t.Setenv("CPA_MGMT_KEY", "")
	t.Setenv("CLIPROXY_MGMT_KEY", "")
	var stderr bytes.Buffer
	if code := run([]string{"-once"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestMaskAccount(t *testing.T) {
	tests := []struct{ email, name, want string }{
		{"jfgagne50@gmail.com", "x.json", "jf***@gmail.com"},
		{"ab@x.io", "", "a***@x.io"},
		{"", "claude-jfgagne50@gmail.com.json", "cl***@gmail.com.json"},
		{"", "codex-team.json", "codex-team.json"},
	}
	for _, tc := range tests {
		if got := maskAccount(tc.email, tc.name); got != tc.want {
			t.Errorf("maskAccount(%q, %q) = %q, want %q", tc.email, tc.name, got, tc.want)
		}
	}
}

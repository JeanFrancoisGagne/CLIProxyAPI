package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakeResolver struct {
	mu     sync.Mutex
	fail   map[string]bool
	looked []string
}

func (r *fakeResolver) resolve(_ context.Context, host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.looked = append(r.looked, host)
	if r.fail[host] {
		return errors.New("lookup " + host + ": server misbehaving")
	}
	return nil
}

func (r *fakeResolver) lookedUp() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string{}, r.looked...)
	sort.Strings(out)
	return out
}

var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func testReadyChecker(t *testing.T, grants map[string]*auth.GrantCounts, res *fakeResolver) (*readyChecker, string) {
	t.Helper()
	dir := t.TempDir()
	return &readyChecker{
		grants:  func(time.Time, time.Duration) map[string]*auth.GrantCounts { return grants },
		authDir: func() string { return dir },
		resolve: res.resolve,
		now:     func() time.Time { return testNow },
	}, dir
}

// writeOverrideAt writes the override file with an mtime of written.
func writeOverrideAt(t *testing.T, dir, value string, written time.Time) {
	t.Helper()
	path := filepath.Join(dir, readyOverrideFile)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
}

func writeOverride(t *testing.T, dir, value string) {
	t.Helper()
	writeOverrideAt(t, dir, value, testNow.Add(-time.Minute))
}

func claudeCounts(usable, expiring, invalid int) map[string]*auth.GrantCounts {
	return map[string]*auth.GrantCounts{"claude": {
		Total: usable + expiring + invalid, Usable: usable, Expiring: expiring, InvalidGrant: invalid,
	}}
}

func TestReadyCheckStates(t *testing.T) {
	codexAndClaude := claudeCounts(1, 0, 0)
	codexAndClaude["codex"] = &auth.GrantCounts{Total: 1, Usable: 1}

	cases := []struct {
		name      string
		grants    map[string]*auth.GrantCounts
		failDNS   []string
		override  string
		age       time.Duration
		wantReady bool
		wantHosts []string
	}{
		{name: "one usable claude grant", grants: claudeCounts(1, 2, 0), wantReady: true,
			wantHosts: []string{"api.anthropic.com", "platform.claude.com"}},
		{name: "no grants at all", grants: map[string]*auth.GrantCounts{}, wantReady: false},
		{name: "claude only expiring", grants: claudeCounts(0, 3, 0), wantReady: false},
		{name: "claude only invalid_grant", grants: claudeCounts(0, 0, 2), wantReady: false},
		{name: "codex usable but no claude", grants: map[string]*auth.GrantCounts{"codex": {Total: 1, Usable: 1}}, wantReady: false},
		{name: "claude api host does not resolve", grants: claudeCounts(1, 0, 0), failDNS: []string{"api.anthropic.com"}, wantReady: false},
		{name: "claude token host does not resolve", grants: claudeCounts(1, 0, 0), failDNS: []string{"platform.claude.com"}, wantReady: false},
		{name: "codex host fails while codex grants loaded", grants: codexAndClaude, failDNS: []string{"chatgpt.com"}, wantReady: false,
			wantHosts: []string{"api.anthropic.com", "auth.openai.com", "chatgpt.com", "platform.claude.com"}},
		{name: "codex host not checked without codex grants", grants: claudeCounts(1, 0, 0), failDNS: []string{"chatgpt.com"}, wantReady: true,
			wantHosts: []string{"api.anthropic.com", "platform.claude.com"}},
		{name: "override not-ready on a healthy proxy", grants: claudeCounts(1, 0, 0), override: "not-ready\n", wantReady: false},
		{name: "fresh override ready on a dead proxy", grants: map[string]*auth.GrantCounts{}, failDNS: []string{"api.anthropic.com"}, override: " READY ", age: 14 * time.Minute, wantReady: true},
		{name: "stale override ready on a dead proxy", grants: map[string]*auth.GrantCounts{}, failDNS: []string{"api.anthropic.com"}, override: "ready", age: 16 * time.Minute, wantReady: false},
		{name: "override ready dated in the future", grants: map[string]*auth.GrantCounts{}, override: "ready", age: -2 * time.Hour, wantReady: false},
		{name: "old override not-ready still holds", grants: claudeCounts(1, 0, 0), override: "not-ready", age: 48 * time.Hour, wantReady: false},
		{name: "unknown override is ignored", grants: claudeCounts(1, 0, 0), override: "maybe", wantReady: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &fakeResolver{fail: map[string]bool{}}
			for _, h := range tc.failDNS {
				res.fail[h] = true
			}
			rc, dir := testReadyChecker(t, tc.grants, res)
			if tc.override != "" {
				age := tc.age
				if age == 0 {
					age = time.Minute
				}
				writeOverrideAt(t, dir, tc.override, testNow.Add(-age))
			}
			report := rc.check(context.Background())
			if report.Ready != tc.wantReady {
				t.Fatalf("ready = %v, want %v; reasons %v", report.Ready, tc.wantReady, report.Reasons)
			}
			if tc.override == "" && report.Ready != (len(report.Reasons) == 0) {
				t.Fatalf("ready %v disagrees with reasons %v", report.Ready, report.Reasons)
			}
			for _, h := range tc.failDNS {
				if status, checked := report.DNS[h]; checked && status == "ok" {
					t.Fatalf("dns[%s] = ok for a failing resolver", h)
				}
			}
			if tc.wantHosts != nil {
				if got := res.lookedUp(); !equalStrings(got, tc.wantHosts) {
					t.Fatalf("looked up %v, want %v", got, tc.wantHosts)
				}
			}
		})
	}
}

func TestReadyOverrideIsReportedInBody(t *testing.T) {
	res := &fakeResolver{fail: map[string]bool{}}
	rc, dir := testReadyChecker(t, claudeCounts(1, 0, 0), res)
	writeOverride(t, dir, "not-ready")
	if got := rc.check(context.Background()).Override; got != "not-ready" {
		t.Fatalf("override = %q", got)
	}
	writeOverride(t, dir, "maybe")
	if got := rc.check(context.Background()).Override; got != "ignored: maybe" {
		t.Fatalf("override = %q", got)
	}
	writeOverrideAt(t, dir, "ready", testNow.Add(-20*time.Minute))
	report := rc.check(context.Background())
	if want := "ignored: ready, written 20m0s ago, limit 15m0s"; report.Override != want || report.OverrideAge != "20m0s" {
		t.Fatalf("stale ready: override %q age %q, want %q", report.Override, report.OverrideAge, want)
	}
}

// A resolver that hangs must not hang the probe: the check gives up after
// readyResolveTimeout and answers not ready.
func TestReadyCheckHungResolverTimesOut(t *testing.T) {
	rc, _ := testReadyChecker(t, claudeCounts(1, 0, 0), &fakeResolver{})
	rc.resolve = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	report := rc.check(context.Background())
	if elapsed := time.Since(start); elapsed > readyResolveTimeout+time.Second {
		t.Fatalf("check took %v", elapsed)
	}
	if report.Ready {
		t.Fatal("ready with a hung resolver")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The route needs the same client key as /v1/models, answers 503 with counts
// when no Claude grant can serve, 200 once one can, and obeys the override
// file in the auth dir.
func TestReadyRoute(t *testing.T) {
	server := newTestServer(t)
	res := &fakeResolver{fail: map[string]bool{}}
	server.ready.resolve = res.resolve

	get := func(key string) (int, readyReport) {
		req := httptest.NewRequest(http.MethodGet, "/healthz/ready", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		var report readyReport
		if rec.Code == http.StatusOK || rec.Code == http.StatusServiceUnavailable {
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
		}
		return rec.Code, report
	}

	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d, want 401", code)
	}
	if code, _ := get("wrong-key"); code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status %d, want 401", code)
	}
	code, report := get("test-key")
	if code != http.StatusServiceUnavailable || report.Ready {
		t.Fatalf("empty auth dir: status %d ready %v, want 503", code, report.Ready)
	}

	claude := &auth.Auth{
		ID:       "claude-test.json",
		Provider: "claude",
		Metadata: map[string]any{
			"access_token":  "at",
			"refresh_token": "rt",
			"expired":       time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), claude); err != nil {
		t.Fatalf("register: %v", err)
	}
	code, report = get("test-key")
	if code != http.StatusOK || !report.Ready {
		t.Fatalf("one usable grant: status %d reasons %v, want 200", code, report.Reasons)
	}
	if c := report.Providers["claude"]; c == nil || c.Usable != 1 || c.Total != 1 {
		t.Fatalf("claude counts = %+v", c)
	}

	res.mu.Lock()
	res.fail["platform.claude.com"] = true
	res.mu.Unlock()
	if code, report = get("test-key"); code != http.StatusServiceUnavailable {
		t.Fatalf("failed resolver: status %d, want 503", code)
	}
	if report.DNS["platform.claude.com"] == "ok" {
		t.Fatalf("dns = %v", report.DNS)
	}
	res.mu.Lock()
	res.fail = map[string]bool{}
	res.mu.Unlock()

	writeOverride(t, server.cfg.AuthDir, "not-ready")
	if code, report = get("test-key"); code != http.StatusServiceUnavailable || report.Override != "not-ready" {
		t.Fatalf("override not-ready: status %d override %q", code, report.Override)
	}
	if err := os.Remove(filepath.Join(server.cfg.AuthDir, readyOverrideFile)); err != nil {
		t.Fatal(err)
	}
	if code, _ = get("test-key"); code != http.StatusOK {
		t.Fatalf("override removed: status %d, want 200", code)
	}
}

// Through the route with the real auth manager: a Claude access token that
// expires within 10 min does not count, nor does a grant the selector skips.
func TestReadyRouteUsesManagerState(t *testing.T) {
	server := newTestServer(t)
	server.ready.resolve = (&fakeResolver{fail: map[string]bool{}}).resolve

	get := func() (int, readyReport) {
		req := httptest.NewRequest(http.MethodGet, "/healthz/ready", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		var report readyReport
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
		return rec.Code, report
	}
	register := func(a *auth.Auth) {
		t.Helper()
		if _, err := server.handlers.AuthManager.Register(context.Background(), a); err != nil {
			t.Fatalf("register %s: %v", a.ID, err)
		}
	}
	claudeGrant := func(id string, expires time.Time) *auth.Auth {
		return &auth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{
			"access_token":  "at-" + id,
			"refresh_token": "rt-" + id,
			"expired":       expires.UTC().Format(time.RFC3339),
		}}
	}

	register(claudeGrant("claude-soon.json", time.Now().Add(5*time.Minute)))
	code, report := get()
	if c := report.Providers["claude"]; code != http.StatusServiceUnavailable || c == nil || c.Expiring != 1 || c.Usable != 0 {
		t.Fatalf("token valid 5 min: status %d counts %+v, want 503 with 1 expiring", code, c)
	}

	held := claudeGrant("claude-held.json", time.Now().Add(5*time.Hour))
	held.Unavailable = true
	held.Status = auth.StatusError
	held.LastError = &auth.Error{Code: "unauthorized", Message: "token revoked", HTTPStatus: http.StatusUnauthorized}
	register(held)
	code, report = get()
	if c := report.Providers["claude"]; code != http.StatusServiceUnavailable || c == nil || c.Blocked != 1 || c.Usable != 0 {
		t.Fatalf("terminal 401 grant: status %d counts %+v, want 503 with 1 blocked", code, c)
	}

	register(claudeGrant("claude-live.json", time.Now().Add(11*time.Minute)))
	code, report = get()
	if c := report.Providers["claude"]; code != http.StatusOK || c == nil || c.Usable != 1 {
		t.Fatalf("token valid 11 min: status %d counts %+v, want 200 with 1 usable", code, c)
	}
}

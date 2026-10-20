package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAPICallHonorsRetryAfterAcrossManagementRoutes(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"fixture rate limit"}`))
	}))
	defer upstream.Close()

	h := &Handler{}
	router := gin.New()
	router.POST("/v8/management/requests/api-call", h.APICall)
	router.POST("/v0/management/api-call", h.APICall)
	for i, route := range []string{"/v8/management/requests/api-call", "/v0/management/api-call"} {
		response := callBackoffFixture(t, router, route, apiCallRequest{Method: http.MethodGet, URL: upstream.URL + "/usage", Header: map[string]string{"User-Agent": []string{"fixture dashboard", "fixture pacer"}[i]}})
		if response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("upstream status = %d, want 429", response.StatusCode)
		}
		if http.Header(response.Header).Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls during 360-second cooldown = %d, want 1", got)
	}
}

func TestAPICallRetryAfterExpiresAndReportsRemainingSeconds(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, header := range []string{"360", "Tue, 20 Oct 2026 16:06:00 GMT"} {
			t.Run(method+"/"+header, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if calls.Add(1) == 1 {
						w.Header().Set("Date", "Tue, 20 Oct 2026 16:00:00 GMT")
						w.Header().Set("Retry-After", header)
						w.WriteHeader(http.StatusTooManyRequests)
						return
					}
					_, _ = w.Write([]byte("fixture quota recovered"))
				}))
				defer upstream.Close()
				clock := newAPICallFixtureClock()
				h := &Handler{apiReadBackoff: apiCallReadBackoff{now: clock.now}}
				router := backoffFixtureRouter(h)
				body := apiCallRequest{Method: method, URL: upstream.URL + "/usage"}
				callBackoffFixture(t, router, "/read", body)
				clock.advance(60*time.Second + 200*time.Millisecond)
				response := callBackoffFixture(t, router, "/read", body)
				if got := http.Header(response.Header).Get("Retry-After"); got != "300" {
					t.Fatalf("remaining Retry-After = %q, want 300", got)
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("calls before deadline = %d, want 1", got)
				}
				clock.advance(299*time.Second + 800*time.Millisecond)
				response = callBackoffFixture(t, router, "/read", body)
				if response.StatusCode != http.StatusOK || calls.Load() != 2 {
					t.Fatalf("at deadline: status = %d, upstream calls = %d; want 200 and 2", response.StatusCode, calls.Load())
				}
			})
		}
	}
}

func TestAPICallDoesNotInventBackoffForInvalidRetryAfter(t *testing.T) {
	for _, header := range []string{"", "later", "-1", "0", "Tue, 20 Oct 2026 15:59:59 GMT", "184467440737095516160"} {
		t.Run(header, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", header)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer upstream.Close()
			clock := newAPICallFixtureClock()
			router := backoffFixtureRouter(&Handler{apiReadBackoff: apiCallReadBackoff{now: clock.now}})
			body := apiCallRequest{Method: http.MethodGet, URL: upstream.URL}
			callBackoffFixture(t, router, "/read", body)
			callBackoffFixture(t, router, "/read", body)
			if got := calls.Load(); got != 2 {
				t.Fatalf("calls with invalid/expired Retry-After = %d, want 2", got)
			}
		})
	}
}

func TestAPICallHTTPDateRetryAfterUsesProviderClock(t *testing.T) {
	for _, skew := range []time.Duration{-10 * time.Minute, -5 * time.Minute, 5 * time.Minute, 10 * time.Minute} {
		t.Run(skew.String(), func(t *testing.T) {
			clock := newAPICallFixtureClock()
			providerNow := clock.now().UTC().Add(skew)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Date", providerNow.Format(http.TimeFormat))
					w.Header().Set("Retry-After", providerNow.Add(360*time.Second).Format(http.TimeFormat))
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				_, _ = w.Write([]byte("fixture recovered"))
			}))
			defer upstream.Close()
			router := backoffFixtureRouter(&Handler{apiReadBackoff: apiCallReadBackoff{now: clock.now}})
			body := apiCallRequest{Method: http.MethodGet, URL: upstream.URL}
			callBackoffFixture(t, router, "/read", body)
			clock.advance(359 * time.Second)
			response := callBackoffFixture(t, router, "/read", body)
			if response.StatusCode != http.StatusTooManyRequests || calls.Load() != 1 || http.Header(response.Header).Get("Retry-After") != "1" {
				t.Fatalf("before provider deadline: status = %d, calls = %d, Retry-After = %q; want 429, 1, 1", response.StatusCode, calls.Load(), http.Header(response.Header).Get("Retry-After"))
			}
			clock.advance(time.Second)
			response = callBackoffFixture(t, router, "/read", body)
			if response.StatusCode != http.StatusOK || calls.Load() != 2 {
				t.Fatalf("at provider deadline: status = %d, calls = %d; want 200, 2", response.StatusCode, calls.Load())
			}
		})
	}
}

func TestAPICallCooldownIsolatesCredentialResourceAndRoute(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer proxy.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	register := func(id, token string) *coreauth.Auth {
		t.Helper()
		auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": token}})
		if err != nil {
			t.Fatal(err)
		}
		return auth
	}
	first := register("fixture-first", "fixture-token-one")
	second := register("fixture-second", "fixture-token-two")
	router := backoffFixtureRouter(&Handler{authManager: manager})
	base := apiCallRequest{AuthIndexSnake: &first.Index, Method: http.MethodGet, URL: upstream.URL + "/usage", Header: map[string]string{"Authorization": "Bearer $TOKEN$"}}
	callBackoffFixture(t, router, "/read", base)
	callBackoffFixture(t, router, "/read", base)
	if calls.Load() != 1 {
		t.Fatalf("initial account calls = %d, want 1", calls.Load())
	}
	cases := []apiCallRequest{base, base, base, base, base, base}
	cases[0].AuthIndexSnake = &second.Index
	cases[1].URL = upstream.URL + "/profile"
	cases[2].URL = upstream.URL + "/usage?different=1"
	cases[3].ProxyURL = proxy.URL
	cases[4].Header = map[string]string{"Authorization": "Bearer fixture-explicit-other-token"}
	cases[5].Header = map[string]string{"Authorization": "Bearer $TOKEN$", "Host": "fixture-other-host.invalid"}
	for i, body := range cases {
		callBackoffFixture(t, router, "/read", body)
		if got := calls.Load(); got != int32(i+2) {
			t.Fatalf("isolation case %d: upstream calls = %d, want %d", i, got, i+2)
		}
	}
	first.Metadata["access_token"] = "fixture-rotated-token"
	if _, err := manager.Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	callBackoffFixture(t, router, "/read", base)
	if calls.Load() != 8 {
		t.Fatalf("rotated token upstream calls = %d, want 8", calls.Load())
	}
	// A file replacement for a different account must not inherit its embargo.
	replacement := register("fixture-first", "fixture-rotated-token")
	replacement.Metadata["account_uuid"] = "fixture-other-account"
	if _, err := manager.Update(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	callBackoffFixture(t, router, "/read", base)
	if calls.Load() != 9 {
		t.Fatalf("replacement registration upstream calls = %d, want 9", calls.Load())
	}
}

func TestAPICallCooldownSurvivesPriorityAndWatcherReregistration(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "fixture-watched", Provider: "claude",
		Metadata: map[string]any{"access_token": "fixture-stable-token", "account_uuid": "fixture-stable-account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := backoffFixtureRouter(&Handler{authManager: manager})
	body := apiCallRequest{AuthIndexSnake: &auth.Index, Method: http.MethodGet, URL: upstream.URL}
	callBackoffFixture(t, router, "/read", body)
	auth.Attributes = map[string]string{"priority": "90"}
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	callBackoffFixture(t, router, "/read", body)
	if calls.Load() != 1 {
		t.Fatalf("priority update bypassed cooldown: calls = %d, want 1", calls.Load())
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	callBackoffFixture(t, router, "/read", body)
	if calls.Load() != 1 {
		t.Fatalf("watcher registration bypassed cooldown: calls = %d, want 1", calls.Load())
	}
}

func TestAPICallDoesNotShareSuccessOrGateWrites(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/limited" {
			w.Header().Set("Retry-After", "360")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(r.Header.Get("Accept")))
	}))
	defer upstream.Close()
	router := backoffFixtureRouter(&Handler{})
	for _, accept := range []string{"application/json", "text/plain", "application/json"} {
		response := callBackoffFixture(t, router, "/read", apiCallRequest{Method: http.MethodGet, URL: upstream.URL + "/ok", Header: map[string]string{"Accept": accept}})
		if response.Body != accept {
			t.Fatalf("representation = %q, want %q", response.Body, accept)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for range 2 {
			callBackoffFixture(t, router, "/read", apiCallRequest{Method: method, URL: upstream.URL + "/limited"})
		}
	}
	if calls.Load() != 11 {
		t.Fatalf("uncached successes and writes calls = %d, want 11", calls.Load())
	}
}

func TestAPICallConcurrentReadsShareOnlyRateLimit(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/limited" {
			_, _ = w.Write([]byte("unrelated proceeds"))
			return
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		<-finish
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	router := backoffFixtureRouter(&Handler{})
	body := apiCallRequest{Method: http.MethodGet, URL: upstream.URL + "/limited"}
	var callers sync.WaitGroup
	for i := range 10 {
		callers.Go(func() {
			requestBody := body
			requestBody.Header = map[string]string{"User-Agent": fmt.Sprintf("fixture caller %d", i)}
			callBackoffFixture(t, router, "/read", requestBody)
		})
	}
	<-started
	// A stalled read for one resource must not hold a network-wide mutex.
	response := callBackoffFixture(t, router, "/read", apiCallRequest{Method: http.MethodGet, URL: upstream.URL + "/other"})
	if response.Body != "unrelated proceeds" {
		t.Fatalf("unrelated response = %q", response.Body)
	}
	close(finish)
	callers.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent upstream calls = %d, want 1", got)
	}
}

func TestAPICallCanceledWaiterDoesNotCallUpstream(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(started)
		<-finish
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	router := backoffFixtureRouter(&Handler{})
	body := apiCallRequest{Method: http.MethodGet, URL: upstream.URL}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		callBackoffFixture(t, router, "/read", body)
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	observedCtx := &apiCallObservedContext{Context: ctx, waiting: waiting, once: &sync.Once{}}
	payload, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/read", strings.NewReader(string(payload))).WithContext(observedCtx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		router.ServeHTTP(recorder, request)
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("second read did not wait behind the first read")
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled waiting read did not return")
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled waiter status = %d, want 503", recorder.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls after cancellation = %d, want 1", calls.Load())
	}
	close(finish)
	<-firstDone
	callBackoffFixture(t, router, "/read", body)
	if calls.Load() != 1 {
		t.Fatalf("canceled waiter lost cooldown: calls = %d, want 1", calls.Load())
	}
}

func TestAPICallBoundsCooldownEntriesWithoutEvictingEmbargoes(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	clock := newAPICallFixtureClock()
	router := backoffFixtureRouter(&Handler{apiReadBackoff: apiCallReadBackoff{now: clock.now}})
	for i := range 1024 {
		callBackoffFixture(t, router, "/read", apiCallRequest{Method: http.MethodGet, URL: fmt.Sprintf("%s/usage/%d", upstream.URL, i)})
	}
	payload, _ := json.Marshal(apiCallRequest{Method: http.MethodGet, URL: upstream.URL + "/new-resource"})
	request := httptest.NewRequest(http.MethodPost, "/read", strings.NewReader(string(payload)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || calls.Load() != 1024 {
		t.Fatalf("full gate: outer status = %d, upstream calls = %d; want 503 and 1024", recorder.Code, calls.Load())
	}
	callBackoffFixture(t, router, "/read", apiCallRequest{Method: http.MethodGet, URL: upstream.URL + "/usage/0"})
	if calls.Load() != 1024 {
		t.Fatalf("capacity eviction lost embargo: calls = %d", calls.Load())
	}
	clock.advance(360 * time.Second)
	callBackoffFixture(t, router, "/read", apiCallRequest{Method: http.MethodGet, URL: upstream.URL + "/new-resource"})
	if calls.Load() != 1025 {
		t.Fatalf("expired capacity was not freed: calls = %d", calls.Load())
	}
}

func TestAPICallOwnTimeoutBudgetIncludesQueueWait(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-finish
		w.Header().Set("Retry-After", "360")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	var contextCalls atomic.Int32
	waiting := make(chan struct{})
	observedOnce := &sync.Once{}
	budgetCtx, cancelBudget := context.WithCancel(context.Background())
	defer cancelBudget()
	budget := &apiCallObservedContext{Context: budgetCtx, waiting: waiting, once: observedOnce}
	h := &Handler{apiCallContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
		if contextCalls.Add(1) == 1 {
			return context.WithCancel(ctx)
		}
		return budget, cancelBudget
	}}
	router := backoffFixtureRouter(h)
	body := apiCallRequest{Method: http.MethodGet, URL: upstream.URL}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		callBackoffFixture(t, router, "/read", body)
	}()
	<-started
	payload, _ := json.Marshal(body)
	parent := &apiCallObservedContext{Context: context.Background(), waiting: waiting, once: observedOnce}
	request := httptest.NewRequest(http.MethodPost, "/read", strings.NewReader(string(payload))).WithContext(parent)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		router.ServeHTTP(recorder, request)
	}()
	<-waiting
	cancelBudget()
	select {
	case <-secondDone:
		close(finish)
		<-firstDone
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("queue timeout status = %d, want 503", recorder.Code)
		}
	case <-time.After(2 * time.Second):
		close(finish)
		<-firstDone
		<-secondDone
		t.Fatal("APICall's own timeout budget did not cancel the queued read")
	}
}

type apiCallObservedContext struct {
	context.Context
	waiting chan struct{}
	once    *sync.Once
}

func (c *apiCallObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func backoffFixtureRouter(h *Handler) http.Handler {
	router := gin.New()
	router.POST("/read", h.APICall)
	return router
}

type apiCallFixtureClock struct{ unixNanos atomic.Int64 }

func newAPICallFixtureClock() *apiCallFixtureClock {
	clock := &apiCallFixtureClock{}
	clock.unixNanos.Store(time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC).UnixNano())
	return clock
}

func (c *apiCallFixtureClock) now() time.Time          { return time.Unix(0, c.unixNanos.Load()) }
func (c *apiCallFixtureClock) advance(d time.Duration) { c.unixNanos.Add(int64(d)) }

func callBackoffFixture(t *testing.T, router http.Handler, route string, body apiCallRequest) apiCallResponse {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(string(payload)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("management status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var response apiCallResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

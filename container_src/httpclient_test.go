package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func noSleep(context.Context, time.Duration) error { return nil }

func testClientFor(t *testing.T, base string, readOnly bool) *apiClient {
	t.Helper()
	cfg := testConfig("http://x/", base)
	c := newAPIClient("test", base, http.Header{}, cfg.ProductiveRPS, cfg.ProductiveBurstRPS, cfg, readOnly, testLogger())
	c.sleep = noSleep // skip the real backoff so the suite stays fast
	return c
}

// The retry policy is the single most consequential safety rule in the program:
// a 429 proves nothing happened, everything else on a POST is ambiguous.
func TestRetryablePolicy(t *testing.T) {
	tests := []struct {
		method string
		status int
		err    error
		want   bool
		why    string
	}{
		{http.MethodGet, 429, nil, true, "reads may always retry"},
		{http.MethodGet, 500, nil, true, "reads may always retry"},
		{http.MethodGet, 0, errors.New("boom"), true, "reads may always retry"},
		{http.MethodGet, 404, nil, false, "a definite 4xx is final"},

		{http.MethodPost, 429, nil, true, "429 proves the write was rejected"},
		{http.MethodPost, 500, nil, false, "AMBIGUOUS: the task may exist; retrying duplicates it"},
		{http.MethodPost, 502, nil, false, "AMBIGUOUS"},
		{http.MethodPost, 504, nil, false, "AMBIGUOUS"},
		{http.MethodPost, 0, errors.New("connection reset"), false, "AMBIGUOUS"},
		{http.MethodPost, 422, nil, false, "definite rejection"},

		{http.MethodPatch, 429, nil, true, "content-idempotent"},
		{http.MethodPatch, 500, nil, true, "content-idempotent"},
		{http.MethodPatch, 0, errors.New("boom"), true, "content-idempotent"},
	}
	for _, tc := range tests {
		got := retryable(tc.method, tc.status, tc.err)
		if got != tc.want {
			t.Errorf("retryable(%s, %d, %v) = %v, want %v — %s",
				tc.method, tc.status, tc.err, got, tc.want, tc.why)
		}
	}
}

func TestDoRetriesGetOn429ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	c := testClientFor(t, srv.URL+"/", false)
	status, _, err := c.do(context.Background(), http.MethodGet, "thing", nil)
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if got := c.Stats(); got.Requests != 3 || got.Retries != 2 || got.RateLimited != 2 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestDoDoesNotRetryPostOn5xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	c := testClientFor(t, srv.URL+"/", false)
	status, _, err := c.do(context.Background(), http.MethodPost, "tasks", []byte(`{}`))
	if err != nil {
		t.Fatalf("a 502 is a status, not a transport error: %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d", status)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("POST attempted %d times; a 5xx POST must never be retried (it may have created the task)", n)
	}
}

func TestDoRetriesPostOn429(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":"1"}}`))
	}))
	t.Cleanup(srv.Close)

	c := testClientFor(t, srv.URL+"/", false)
	status, _, err := c.do(context.Background(), http.MethodPost, "tasks", []byte(`{}`))
	if err != nil || status != http.StatusCreated {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
}

func TestDoReportsRateLimitedWhenRetriesExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := testClientFor(t, srv.URL+"/", false)
	c.maxRetries = 1
	_, _, err := c.do(context.Background(), http.MethodGet, "thing", nil)
	if !errors.Is(err, errRateLimited) {
		t.Fatalf("want errRateLimited, got %v", err)
	}
}

// The read-only switch lives at the lowest layer so a bug anywhere above it
// cannot mutate production during a dry run.
func TestReadOnlyBlocksWrites(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := testClientFor(t, srv.URL+"/", true)

	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		if _, _, err := c.do(context.Background(), m, "tasks", []byte(`{}`)); !errors.Is(err, errReadOnly) {
			t.Fatalf("%s: want errReadOnly, got %v", m, err)
		}
	}
	if _, _, err := c.do(context.Background(), http.MethodGet, "tasks", nil); err != nil {
		t.Fatalf("GET must still work: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests; only the GET may reach it", n)
	}
}

func TestGetJSONTreatsNon2xxAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"title":"Unauthorized","detail":"bad token"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := testClientFor(t, srv.URL+"/", true)
	c.maxRetries = 0

	var out map[string]any
	err := c.getJSON(context.Background(), "tasks", &out)
	if err == nil {
		t.Fatal("non-2xx must be an error, not a zero-valued decode")
	}
	var he *httpError
	if !errors.As(err, &he) || he.Status != 401 {
		t.Fatalf("want httpError 401, got %v", err)
	}
	if got := he.Error(); !strings.Contains(got, "bad token") {
		t.Fatalf("JSON:API error detail not surfaced: %s", got)
	}
}

// --- limiter ---

func TestLimiterPacesWithFakeClock(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var slept []time.Duration

	l := newLimiter(2, 1) // 2 per second, burst 1
	l.now = func() time.Time { return now }
	l.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		now = now.Add(d)
		return nil
	}

	for range 3 {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}

	// First goes through on the initial token; the next two each wait ~500ms.
	if len(slept) != 2 {
		t.Fatalf("slept %v, want two waits", slept)
	}
	for _, d := range slept {
		if d < 400*time.Millisecond || d > 600*time.Millisecond {
			t.Fatalf("wait %v is not ~500ms for a 2/s limiter", d)
		}
	}
}

func TestLimiterRefillsOverTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLimiter(2, 1)
	l.now = func() time.Time { return now }
	l.sleep = func(_ context.Context, d time.Duration) error { t.Fatalf("unexpected wait of %v", d); return nil }

	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	now = now.Add(time.Second) // plenty of time for a refill
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

func TestLimiterRespectsContextCancellation(t *testing.T) {
	l := newLimiter(0.001, 1) // effectively "wait forever"
	ctx, cancel := context.WithCancel(context.Background())

	if err := l.Wait(ctx); err != nil {
		t.Fatalf("first token should be free: %v", err)
	}
	cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

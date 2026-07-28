package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, cfg Config) (*server, http.Handler) {
	t.Helper()
	s := &server{cfg: cfg, log: testLogger()}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/ready", s.handlePing)
	mux.HandleFunc("/healthz", s.handlePing)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/sync", s.handleSync)
	mux.HandleFunc("/", s.handleRoot)
	return s, mux
}

// The platform health-checks the container during startup. The documented default
// pingEndpoint is "ping", but the docs' examples use a host+path form, so every
// plausible variant answers 200 — and none of them requires auth.
func TestHealthPathsAnswerWithoutAuth(t *testing.T) {
	_, h := newTestServer(t, testConfig("http://x/", "http://y/"))

	for _, path := range []string{"/ping", "/ready", "/healthz", "/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Fatalf("%s: Cache-Control = %q", path, got)
		}
	}
}

func TestRoutingRejectsWrongMethodsAndPaths(t *testing.T) {
	_, h := newTestServer(t, testConfig("http://x/", "http://y/"))

	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/sync", http.StatusMethodNotAllowed},
		{http.MethodPut, "/sync", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/sync", http.StatusMethodNotAllowed},
		{http.MethodPost, "/status", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

// In-process single flight. This is defence in depth only — it cannot survive a
// container restart, which is why the authoritative lock is the lease in the
// Durable Object — but it must still reject an overlapping local run.
func TestSyncRejectsOverlappingRun(t *testing.T) {
	s, h := newTestServer(t, testConfig("http://x/", "http://y/"))
	s.active = &runInfo{RunID: "in-flight", StartedAt: time.Now().UTC()}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sync", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Error  string  `json:"error"`
		Active runInfo `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Active.RunID != "in-flight" {
		t.Fatalf("the 409 must name the run holding the slot: %+v", body)
	}
}

func TestStatusReportsIdleAndLastRun(t *testing.T) {
	s, h := newTestServer(t, testConfig("http://x/", "http://y/"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var idle map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &idle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if idle["running"] != false || idle["active"] != nil {
		t.Fatalf("idle status = %v", idle)
	}

	s.last = &Result{RunID: "r1", Created: 3}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(rec.Body.String(), `"run_id": "r1"`) {
		t.Fatalf("last run not reported: %s", rec.Body.String())
	}
}

func TestParseRunOptions(t *testing.T) {
	mk := func(query string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/sync?"+query, nil)
	}

	got, err := parseRunOptions(mk("dry_run=true&explain=1&max_creates=99&max_writes=500"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.DryRun || !got.Explain {
		t.Fatalf("flags = %+v", got)
	}
	if got.MaxCreates == nil || *got.MaxCreates != 99 || got.MaxWrites == nil || *got.MaxWrites != 500 {
		t.Fatalf("overrides = %+v", got)
	}

	// Absent overrides must stay nil so the configured defaults apply.
	got, err = parseRunOptions(mk(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.DryRun || got.Explain || got.MaxCreates != nil || got.MaxWrites != nil {
		t.Fatalf("empty query = %+v", got)
	}

	// dry_run=false must actually mean false, or a "safe" call would write.
	for _, falsey := range []string{"dry_run=false", "dry_run=0", "dry_run="} {
		got, err = parseRunOptions(mk(falsey))
		if err != nil || got.DryRun {
			t.Fatalf("%s -> DryRun=%v err=%v", falsey, got.DryRun, err)
		}
	}

	for _, bad := range []string{"max_creates=abc", "max_writes=-1"} {
		if _, err := parseRunOptions(mk(bad)); err == nil {
			t.Fatalf("%s must be rejected", bad)
		}
	}
}

func TestSyncRunsAndRecordsLastResult(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(2))
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())
	s, h := newTestServer(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sync?dry_run=true", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Run-Id") == "" {
		t.Fatal("X-Run-Id header missing — the caller needs it to correlate logs")
	}

	s.mu.Lock()
	active, last := s.active, s.last
	s.mu.Unlock()
	if active != nil {
		t.Fatal("the slot must be released once the run finishes")
	}
	if last == nil || last.Planned.Create != 2 {
		t.Fatalf("last run not recorded: %+v", last)
	}
}

// An aborted run must not look like a success to the caller.
func TestSyncReturns422WhenAborted(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(2))
	pr := newFakeProductive(t)
	pr.readFailStatus = 500
	cfg := testConfig(cu.baseURL(), pr.baseURL())
	cfg.MaxRetries = 0
	_, h := newTestServer(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sync", nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
}

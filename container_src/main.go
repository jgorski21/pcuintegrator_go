package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// shuttingDown is flipped by SIGTERM. Cloudflare sends SIGTERM and SIGKILLs 15
// minutes later on a host recycle or a container rollout; Execute() polls this
// between writes so a shutdown stops cleanly at a task boundary instead of
// somewhere inside an HTTP round trip.
var shuttingDown atomic.Bool

func isShuttingDown() bool { return shuttingDown.Load() }

type runInfo struct {
	RunID     string    `json:"run_id"`
	StartedAt time.Time `json:"started_at"`
	DryRun    bool      `json:"dry_run"`
}

type server struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	active *runInfo // non-nil while a sync is in flight
	last   *Result
}

func main() {
	cfg, err := LoadConfig()
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	if err != nil {
		// Fail fast and loudly. A blank PRODUCTIVE_TOKEN would make every read 401,
		// and a read failure that is not treated as fatal looks exactly like
		// "Productive is empty" — which would duplicate the entire task list.
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	s := &server{cfg: cfg, log: log}

	mux := http.NewServeMux()
	// The platform health-checks the container during startup; the documented
	// default pingEndpoint is "ping", but the docs' examples use a host+path form.
	// Answering on every plausible variant is cheaper than debugging a failing
	// health check. These paths carry no auth by design.
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/ready", s.handlePing)
	mux.HandleFunc("/healthz", s.handlePing)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/sync", s.handleSync)
	mux.HandleFunc("/", s.handleRoot)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout is deliberately zero: a sync legitimately holds the response
		// open for minutes.
		IdleTimeout: 90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The key itself is never logged: only its "dit_" + 8 prefix, the same string
	// DeliverIT shows in /integracje.
	ditSetup := cfg.deliverITSetup()
	ditBase := ""
	if ditSetup.Base != nil {
		ditBase = ditSetup.Base.String()
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening",
			"deliverit_enabled", ditSetup.Enabled,
			"deliverit_base_url", ditBase,
			"deliverit_proxied", ditSetup.Proxied,
			"deliverit_api_key", ditSetup.KeyLabel,
			"deliverit_disabled_reason", ditSetup.DisabledReason,
			"deliverit_rps", cfg.DeliverITRPS,
			"addr", srv.Addr,
			"clickup_list", cfg.ClickUpListID,
			"productive_task_list", cfg.ProductiveTaskListID,
			"productive_rps", cfg.ProductiveRPS,
			"merge_custom_fields", cfg.MergeCustomFields,
			"include_closed", cfg.IncludeClosed,
			"max_creates", cfg.MaxCreates,
			"max_writes", cfg.MaxWrites,
			"sync_timeout", cfg.SyncTimeout.String())
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shuttingDown.Store(true)
		log.Warn("shutdown signal received; draining")
		s.waitForIdle(13 * time.Minute) // stay well inside the 15min SIGKILL window
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("graceful shutdown incomplete", "err", err)
		}
		log.Info("bye")
	}
}

// waitForIdle lets an in-flight sync reach a task boundary and finish reporting.
func (s *server) waitForIdle(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if active == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.log.Warn("still busy at the drain deadline; exiting anyway")
}

func (s *server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		s.handlePing(w, r)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.mu.Lock()
	active, last := s.active, s.last
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"running": active != nil,
		"active":  active,
		"last":    last,
	})
}

func (s *server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	opts, err := parseRunOptions(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	opts.RunID = newRunID()

	// Single-flight. This is defence in depth only: it cannot survive a container
	// restart, so the authoritative lock is the lease in the Durable Object (see
	// src/index.ts). Two overlapping runs would each read Productive before either
	// wrote, and both would POST the same task.
	s.mu.Lock()
	if s.active != nil {
		busy := *s.active
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "a sync is already running",
			"active": busy,
		})
		return
	}
	info := &runInfo{RunID: opts.RunID, StartedAt: time.Now().UTC(), DryRun: opts.DryRun}
	s.active = info
	s.mu.Unlock()

	// The sync context is DETACHED from the request on purpose. If it were derived
	// from r.Context(), a TCP blip or a caller-side timeout would abort the write
	// loop halfway through. Here a disconnect costs the summary, not the run — the
	// result still lands in /status and in the logs.
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.SyncTimeout)
	done := make(chan *Result, 1)

	go func() {
		defer cancel()
		res := Run(ctx, s.cfg, opts, s.log)
		s.mu.Lock()
		s.active = nil
		s.last = res
		s.mu.Unlock()
		done <- res
	}()

	select {
	case res := <-done:
		status := http.StatusOK
		switch {
		case res.Aborted != "":
			status = http.StatusUnprocessableEntity
		// A DeliverIT failure is a partial failure of the run and never a 422:
		// the ClickUp -> Productive stage did its job.
		case res.Failed > 0 || res.DeliverIT.failed():
			status = http.StatusMultiStatus
		}
		w.Header().Set("X-Run-Id", res.RunID)
		writeJSON(w, status, res)
	case <-r.Context().Done():
		s.log.Warn("caller disconnected; sync continues", "run_id", opts.RunID)
	}
}

func parseRunOptions(r *http.Request) (RunOptions, error) {
	q := r.URL.Query()
	opts := RunOptions{
		DryRun:  parseBoolParam(q.Get("dry_run")),
		Explain: parseBoolParam(q.Get("explain")),
	}
	if v := q.Get("max_creates"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return opts, errors.New("max_creates must be a non-negative integer")
		}
		opts.MaxCreates = &n
	}
	if v := q.Get("max_writes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return opts, errors.New("max_writes must be a non-negative integer")
		}
		opts.MaxWrites = &n
	}
	return opts, nil
}

func parseBoolParam(v string) bool {
	switch v {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", http.MethodGet)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, private")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

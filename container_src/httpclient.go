package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// errReadOnly is returned by do() for any non-GET while the client is in
	// dry-run mode. Enforced at the lowest layer on purpose: a bug anywhere in
	// sync.go then cannot mutate production, which is what makes the dry-run
	// trustworthy enough to leave running against the live org for days.
	errReadOnly = errors.New("read-only client: write suppressed (dry run)")

	// errRateLimited means retries were exhausted on 429. Neither API documents
	// any rate-limit response header on Productive's side, so once the sustained
	// bucket is hit there is no way to know the window — backing off blindly just
	// burns the run deadline. Aborting makes the problem visible instead.
	errRateLimited = errors.New("rate limited: retries exhausted")
)

// httpError carries the response so callers can log Productive's JSON:API
// errors[] body on a 422 instead of an opaque status code. The .NET code ignores
// the PATCH status entirely (PCUIntegratorService.cs:175), so any pre-existing
// 422 has been invisible until now.
type httpError struct {
	Method, Path string
	Status       int
	Body         []byte
}

func (e *httpError) Error() string {
	detail := strings.TrimSpace(string(e.Body))
	if len(detail) > 400 {
		detail = detail[:400] + "…"
	}
	if d := jsonAPIErrorDetail(e.Body); d != "" {
		detail = d
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, detail)
}

// jsonAPIErrorDetail pulls the human-readable bits out of a JSON:API error body.
func jsonAPIErrorDetail(body []byte) string {
	var env struct {
		Errors []struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
			Source struct {
				Pointer   string `json:"pointer"`
				Parameter string `json:"parameter"`
			} `json:"source"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Errors) == 0 {
		return ""
	}
	parts := make([]string, 0, len(env.Errors))
	for _, e := range env.Errors {
		s := strings.TrimSpace(e.Title + " " + e.Detail)
		if p := e.Source.Pointer + e.Source.Parameter; p != "" {
			s += " (" + p + ")"
		}
		parts = append(parts, strings.TrimSpace(s))
	}
	return strings.Join(parts, "; ")
}

// --- limiter ---

// limiter is a token bucket. Hand-rolled rather than golang.org/x/time/rate so the
// module has zero dependencies: `go build` then needs no network at all, which
// removes a failure mode from `wrangler deploy`'s Docker build.
//
// Debt is allowed to go negative, so concurrent callers each take their share and
// wait proportionally instead of thundering.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time

	now   func() time.Time                                 // injectable for tests
	sleep func(ctx context.Context, d time.Duration) error // injectable for tests
}

func newLimiter(rps, burst float64) *limiter {
	if burst < 1 {
		burst = 1
	}
	return &limiter{
		rate:   rps,
		burst:  burst,
		tokens: burst, // start full: the first few requests of a run go straight through
		now:    time.Now,
		sleep:  sleepCtx,
	}
}

// reserve consumes one token and reports how long the caller must wait for it.
func (l *limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if l.last.IsZero() {
		l.last = now
	}
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += elapsed.Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}

	l.tokens--
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / l.rate * float64(time.Second))
}

func (l *limiter) Wait(ctx context.Context) error {
	d := l.reserve()
	if d <= 0 {
		return ctx.Err()
	}
	return l.sleep(ctx, d)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- client ---

type clientStats struct {
	Requests    int `json:"requests"`
	Retries     int `json:"retries"`
	RateLimited int `json:"rate_limited"`
}

type apiClient struct {
	name string
	base string // must end with "/"
	hdr  http.Header
	hc   *http.Client

	// Two buckets, both consulted before every attempt including retries, because
	// a 429 response still counts against the API's own counter.
	sustained *limiter
	burst     *limiter

	maxRetries int
	readOnly   bool
	log        *slog.Logger
	sleep      func(ctx context.Context, d time.Duration) error // injectable for tests

	mu        sync.Mutex
	stats     clientStats
	retryHint time.Duration // Retry-After / X-RateLimit-Reset off the last response
}

func newAPIClient(name, base string, hdr http.Header, rps, burstRPS float64, cfg Config, readOnly bool, log *slog.Logger) *apiClient {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return &apiClient{
		name:       name,
		base:       base,
		hdr:        hdr,
		hc:         &http.Client{Timeout: cfg.HTTPTimeout},
		sustained:  newLimiter(rps, math.Max(1, rps*3)),
		burst:      newLimiter(burstRPS, math.Max(1, burstRPS)),
		maxRetries: cfg.MaxRetries,
		readOnly:   readOnly,
		log:        log.With("api", name),
		sleep:      sleepCtx,
	}
}

func (c *apiClient) Stats() clientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *apiClient) countRequest()     { c.mu.Lock(); c.stats.Requests++; c.mu.Unlock() }
func (c *apiClient) countRetry()       { c.mu.Lock(); c.stats.Retries++; c.mu.Unlock() }
func (c *apiClient) countRateLimited() { c.mu.Lock(); c.stats.RateLimited++; c.mu.Unlock() }

// do performs one logical request, retrying only where a retry is provably safe.
//
// Returns the final status and body. A non-2xx is NOT an error here — callers
// decide (reads abort the run, writes fail one item and continue). Transport
// errors and exhausted retries are returned as errors.
func (c *apiClient) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if c.readOnly && method != http.MethodGet {
		return 0, nil, fmt.Errorf("%w: %s %s", errReadOnly, method, path)
	}

	var lastStatus int
	var lastBody []byte

	for attempt := 0; ; attempt++ {
		if err := c.wait(ctx); err != nil {
			return 0, nil, err
		}

		status, respBody, err := c.attempt(ctx, method, path, body)
		c.countRequest()

		if status == http.StatusTooManyRequests {
			c.countRateLimited()
		}
		if err == nil {
			lastStatus, lastBody = status, respBody
		}

		if err == nil && status < 500 && status != http.StatusTooManyRequests {
			return status, respBody, nil
		}

		if !retryable(method, status, err) || attempt >= c.maxRetries {
			switch {
			case err != nil:
				return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
			case status == http.StatusTooManyRequests:
				return status, respBody, fmt.Errorf("%s %s: %w", method, path, errRateLimited)
			default:
				return status, respBody, nil
			}
		}

		delay := backoff(attempt, status)
		if hint := c.takeRetryHint(); hint > 0 {
			delay = hint
		}
		c.countRetry()
		c.log.Warn("retrying", "method", method, "path", path,
			"attempt", attempt+1, "status", status, "err", errString(err), "in", delay.String())
		if serr := c.sleep(ctx, delay); serr != nil {
			return lastStatus, lastBody, serr
		}
	}
}

func (c *apiClient) wait(ctx context.Context) error {
	if err := c.burst.Wait(ctx); err != nil {
		return err
	}
	return c.sustained.Wait(ctx)
}

func (c *apiClient) attempt(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	for k, vs := range c.hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		// Mandatory on Productive writes; a different value yields 415.
		req.Header.Set("Content-Type", "application/vnd.api+json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	c.rememberRetryHint(resp)
	return resp.StatusCode, respBody, nil
}

// rememberRetryHint stashes Retry-After / X-RateLimit-Reset off the response,
// since attempt() only returns status+body. Productive documents no rate-limit
// headers of any name, so this is opportunistic — the backoff must be correct
// without it.
func (c *apiClient) rememberRetryHint(resp *http.Response) {
	d := time.Duration(0)
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(strings.TrimSpace(v)); err == nil {
			if until := time.Until(t); until > 0 {
				d = until
			}
		}
	}
	// ClickUp documents X-RateLimit-Reset as a unix timestamp, on error responses only.
	if d == 0 {
		if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
			if ts, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				if until := time.Until(time.Unix(ts, 0)); until > 0 && until < 5*time.Minute {
					d = until
				}
			}
		}
	}
	c.mu.Lock()
	c.retryHint = d
	c.mu.Unlock()
}

func (c *apiClient) takeRetryHint() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.retryHint
	c.retryHint = 0
	return d
}

// retryable encodes the single most consequential safety rule in this program.
//
// A 429 PROVES the request was rejected, so retrying it can never duplicate work.
// Everything else on a POST — transport error, read timeout, connection reset,
// 502/503/504 — is AMBIGUOUS: the task may well have been created, and retrying
// creates a twin keyed to the same ClickUp id. That duplicate is expensive
// (Productive's custom field is the only join key, written at POST time) and it
// crash-loops the .NET reader.
//
// Losing a POST response is self-healing instead: the key is written atomically
// with the task, so the next run reads it back and takes the PATCH path.
//
// PATCH is content-idempotent — the body fully describes the desired state — so it
// may be retried freely.
func retryable(method string, status int, err error) bool {
	switch method {
	case http.MethodGet:
		return err != nil || status == http.StatusTooManyRequests || status >= 500
	case http.MethodPatch:
		return err != nil || status == http.StatusTooManyRequests || status >= 500
	case http.MethodPost:
		return status == http.StatusTooManyRequests
	default:
		return false
	}
}

func backoff(attempt, status int) time.Duration {
	base := time.Second << uint(min(attempt, 5)) // 1s, 2s, 4s, 8s, …
	if status == http.StatusTooManyRequests && base < 5*time.Second {
		base = 5 * time.Second
	}
	// Full jitter: avoids synchronised retries when several writes fail together.
	return base/2 + time.Duration(rand.Int64N(int64(base/2)+1))
}

// getJSON reads and decodes, treating ANY non-2xx as a hard error.
//
// This is deliberately unforgiving. .NET gets this right only by accident:
// GetFromJsonAsync throws on non-2xx and the exception escapes to the top-level
// catch, so nothing is written. A naive Go port (`if err != nil` then Decode)
// would decode an error page into a zero-valued struct, see "zero Productive
// tasks", and POST a duplicate of every single ClickUp task.
func (c *apiClient) getJSON(ctx context.Context, path string, out any) error {
	status, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return &httpError{Method: http.MethodGet, Path: path, Status: status, Body: body}
	}
	if err := json.Unmarshal(body, out); err != nil {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return fmt.Errorf("GET %s: decode: %w (body: %s)", path, err, snippet)
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

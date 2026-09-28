package main

// Client for DeliverIT RCP's /api/integrations (deliverit-internal repository).
// Ported from the undeployed syncBridge prototype together with its tests.
//
// The contract (routes, JSON shape, validator rules, error codes) is frozen by the
// SAME JSON literals in deliverit_test.go here and in the .NET tests on the other
// side: changing /api/integrations means changing both repositories.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	deliverITUserAgent = "pcuintegrator/1 (+deliverit-internal)"

	// deliverITProxyHost is the virtual host the Worker intercepts (outboundByHost in
	// src/index.ts). A ".internal" name does not resolve publicly, so a wrong
	// configuration fails instead of sending anything elsewhere.
	deliverITProxyHost = "deliverit.internal"

	deliverITKeyPrefix       = "dit_"
	deliverITKeyPrefixLength = 12 // ApiKey.KeyPrefix in DeliverIT: "dit_" + 8

	deliverITServerMaxTasks   = 500  // SyncProjectTasksCommand.MaxTasks
	deliverITBatchSize        = 200  // headroom under the server limit: the body grows with names, not count
	deliverITMaxExternalIDLen = 100  // \A[A-Za-z0-9_-]{1,100}\z
	deliverITMaxNameRunes     = 1000 // body size only; the server itself cuts names to 300 with "…"

	deliverITMaxRPS = 4.0 // 250 ms between requests

	// The DeliverIT Worker waits up to ~65 s for a cold container (20 s for the
	// instance + 45 s for the port) before answering 503 SERVICE_UNAVAILABLE.
	deliverITRequestTimeout   = 90 * time.Second
	deliverITDefaultRetryWait = 10 * time.Second // = the Worker's Retry-After on a cold start
	deliverITMaxRetryWait     = 30 * time.Second
	deliverITMinRetryWait     = time.Second

	deliverITMaxResponseBytes = 16 << 20
	deliverITSnippetRunes     = 200
)

// --- wire types ---

// taskSource is numeric on the wire (DeliverIT §16): new values only ever go at the end.
type taskSource int

const (
	taskSourceManual     taskSource = 0
	taskSourceClickUp    taskSource = 1
	taskSourceProductive taskSource = 2
)

func (s taskSource) String() string {
	switch s {
	case taskSourceManual:
		return "manual"
	case taskSourceClickUp:
		return "clickup"
	case taskSourceProductive:
		return "productive"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

type ditTaskList struct {
	Source     taskSource `json:"source"`
	ExternalID string     `json:"externalId"`
}

type ditLinkedProject struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	ClientName string      `json:"clientName"`
	TaskList   ditTaskList `json:"taskList"`
}

type ditSyncTask struct {
	ExternalID string `json:"externalId"`
	Name       string `json:"name"`
}

type ditSyncRequest struct {
	TaskList ditTaskList   `json:"taskList"` // sent back exactly as listLinkedProjects returned it
	Tasks    []ditSyncTask `json:"tasks"`    // never null: an empty list marks the project as synced
}

type ditSkipReason int

const (
	ditSkipNameTaken                ditSkipReason = 0
	ditSkipExternalIDInOtherProject ditSkipReason = 1
)

func (r ditSkipReason) String() string {
	switch r {
	case ditSkipNameTaken:
		return "name_taken"
	case ditSkipExternalIDInOtherProject:
		return "external_id_in_other_project"
	default:
		return fmt.Sprintf("unknown_reason_%d", int(r))
	}
}

type ditSkippedTask struct {
	ExternalID string        `json:"externalId"`
	Name       string        `json:"name"`
	Reason     ditSkipReason `json:"reason"`
}

type ditSyncResult struct {
	Created   int              `json:"created"`
	Renamed   int              `json:"renamed"`
	Unchanged int              `json:"unchanged"`
	Skipped   []ditSkippedTask `json:"skipped"`
}

// --- rules repeated from the DeliverIT validator ---

// validDeliverITTaskExternalID mirrors ExternalTaskId: \A[A-Za-z0-9_-]{1,100}\z, ASCII only.
func validDeliverITTaskExternalID(id string) bool {
	if len(id) == 0 || len(id) > deliverITMaxExternalIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !isBase64URLByte(id[i]) {
			return false
		}
	}
	return true
}

// normalizeDeliverITTaskName removes NUL, trims (as the server does) and caps the
// name in RUNES. Deterministic, so the next run with the same source name is
// "unchanged". An empty result is a name the server rejects — the caller drops
// that task.
//
// NUL goes first: PostgreSQL text cannot hold U+0000, the server does not strip
// it, and one such name would cost the WHOLE batch a 500.
func normalizeDeliverITTaskName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\x00", ""))
	if utf8.RuneCountInString(name) <= deliverITMaxNameRunes {
		return name
	}
	return strings.TrimSpace(string([]rune(name)[:deliverITMaxNameRunes]))
}

// validDeliverITAPIKey is a cheap typo filter (prefix + base64url alphabet): a space
// or a trailing newline would otherwise be a baffling 401. Length is the server's call.
func validDeliverITAPIKey(key string) bool {
	if !strings.HasPrefix(key, deliverITKeyPrefix) || len(key) == len(deliverITKeyPrefix) {
		return false
	}
	for i := len(deliverITKeyPrefix); i < len(key); i++ {
		if !isBase64URLByte(key[i]) {
			return false
		}
	}
	return true
}

// validDeliverITKeyPrefix checks what src/index.ts derives from the Worker secret.
func validDeliverITKeyPrefix(prefix string) bool {
	return len(prefix) == deliverITKeyPrefixLength && validDeliverITAPIKey(prefix)
}

// deliverITKeyLabel is the only form of the key that may appear in logs and
// results: "dit_" + 8, the same prefix DeliverIT lists in /integracje.
func deliverITKeyLabel(key string) string {
	if !validDeliverITAPIKey(key) || len(key) <= deliverITKeyPrefixLength {
		return "set (malformed)"
	}
	return key[:deliverITKeyPrefixLength] + "…"
}

func isBase64URLByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// parseDeliverITBaseURL accepts https://… (DIRECT), http://deliverit.internal
// (PROXIED) and plain http to loopback (tests, a local DeliverIT). Plain http to any
// other host would put the key on the wire in clear text. Errors never echo the
// URL: it could carry credentials.
func parseDeliverITBaseURL(raw string) (base *url.URL, proxied bool, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, false, errors.New("not an absolute URL (e.g. http://deliverit.internal or https://internal.deliverit.pl)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, false, errors.New("must not contain credentials, a query or a fragment")
	}
	host := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
		if host == deliverITProxyHost {
			return nil, false, errors.New(deliverITProxyHost + " is the Worker's virtual host and takes plain http://")
		}
		return u, false, nil
	case "http":
		switch host {
		case deliverITProxyHost:
			return u, true, nil
		case "localhost", "127.0.0.1", "::1":
			return u, false, nil
		}
	}
	return nil, false, errors.New("must be https:// (plain http only for " + deliverITProxyHost + " and loopback)")
}

// isGUID — 8-4-4-4-12 hex. The project id goes into the path, where ".." would be
// cleaned by JoinPath into a different route; the server only ever issues GUIDs.
func isGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			c := s[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// --- errors ---

// deliverITProblem is application/problem+json from DeliverIT (and from its
// Worker). It never contains request headers.
type deliverITProblem struct {
	Status  int                 `json:"status"`
	Title   string              `json:"title"`
	Detail  string              `json:"detail"`
	Code    string              `json:"code"`
	TraceID string              `json:"traceId"`
	Errors  map[string][]string `json:"errors,omitempty"`
}

// Error: "deliverit: 409 PROJECT_TASK_LIST_MISMATCH: <detail> [traceId …] (Tasks[0].Name: …)".
// The traceId is how a human finds the request in the .NET log.
func (p *deliverITProblem) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "deliverit: %d %s: %s", p.Status, p.Code, p.Detail)
	if p.TraceID != "" {
		fmt.Fprintf(&b, " [traceId %s]", p.TraceID)
	}
	if len(p.Errors) > 0 {
		fields := make([]string, 0, len(p.Errors))
		for _, field := range slices.Sorted(maps.Keys(p.Errors)) {
			fields = append(fields, field+": "+strings.Join(p.Errors[field], ", "))
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(fields, "; "))
	}
	return b.String()
}

// isAuth — the key is missing, wrong or revoked: every further request is pointless.
func (p *deliverITProblem) isAuth() bool {
	return p.Code == "UNAUTHENTICATED" || p.Code == "API_KEY_INVALID"
}

// deliverITHTTPError is a response without problem+json: a Cloudflare challenge
// page, .NET's empty-bodied 413, a proxy's error page.
type deliverITHTTPError struct {
	Status      int
	ContentType string
	Snippet     string // ≤ 200 runes, one line, key masked
	Challenged  bool   // cf-mitigated: challenge
}

func (e *deliverITHTTPError) Error() string {
	msg := fmt.Sprintf("deliverit: %d without problem+json (%s): %s", e.Status, e.ContentType, e.Snippet)
	if e.Challenged {
		msg += " — Cloudflare challenged the request (Bot Fight Mode / Browser Integrity Check on the deliverit.pl " +
			"zone); the proxied mode (service binding) avoids the zone entirely — see README „Fanout do DeliverIT”"
	}
	return msg
}

func isDeliverITAuthError(err error) bool {
	var p *deliverITProblem
	return errors.As(err, &p) && p.isAuth()
}

// --- client ---

// deliverITClient is sequential by design: DeliverIT's limiter is per IP and a
// handful of projects gains nothing from parallelism.
type deliverITClient struct {
	base   *url.URL
	apiKey string // "" in PROXIED mode: the Worker's outbound handler sets Authorization
	hc     *http.Client
	// redactor replaces the key and DELIVERIT_BASE_URL's host and path wherever a
	// transport error, a redirect or an echoing proxy could carry them.
	redactor *strings.Replacer

	// pace spaces EVERY attempt, retries included (burst 1 = a strict interval):
	// DeliverIT's limiter runs before authentication, so a 429 still counts.
	pace       *limiter
	maxRetries int
	readOnly   bool // dry run: the POST is suppressed at the lowest layer
	log        *slog.Logger
	sleep      func(ctx context.Context, d time.Duration) error // injectable for tests

	mu    sync.Mutex
	stats clientStats
}

func newDeliverITClient(setup deliverITSetup, cfg Config, readOnly bool, log *slog.Logger) *deliverITClient {
	return &deliverITClient{
		base:   setup.Base,
		apiKey: setup.APIKey,
		// Its own client, never shared with ClickUp/Productive, and it never follows a
		// redirect: net/http re-sends Authorization to the same host name or a
		// subdomain of it on ANY port and scheme (plain http included), and a 307/308
		// re-sends the batch. A 3xx is returned as is and decode turns it into an error.
		hc: &http.Client{
			Timeout:       deliverITRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		redactor:   newDeliverITRedactor(setup),
		pace:       newLimiter(cfg.DeliverITRPS, 1),
		maxRetries: cfg.MaxRetries,
		readOnly:   readOnly,
		log:        log.With("api", "deliverit"),
		sleep:      sleepCtx,
	}
}

func (c *deliverITClient) Stats() clientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *deliverITClient) count(f func(*clientStats)) { c.mu.Lock(); f(&c.stats); c.mu.Unlock() }

// listLinkedProjects — GET /api/integrations/projects: the projects linked to a
// task list in a source. DeliverIT owns this mapping; pcuintegrator has none.
func (c *deliverITClient) listLinkedProjects(ctx context.Context) ([]ditLinkedProject, error) {
	var projects []ditLinkedProject
	if err := c.call(ctx, http.MethodGet, []string{"api", "integrations", "projects"}, nil, &projects); err != nil {
		return nil, err
	}
	if projects == nil {
		projects = []ditLinkedProject{}
	}
	return projects, nil
}

// syncTasks — POST /api/integrations/projects/{id}/tasks/sync, one batch.
func (c *deliverITClient) syncTasks(ctx context.Context, projectID string, req ditSyncRequest) (ditSyncResult, error) {
	if !isGUID(projectID) {
		return ditSyncResult{}, fmt.Errorf("deliverit: malformed project id %q", projectID)
	}
	body, err := marshalDeliverITRequest(req)
	if err != nil {
		return ditSyncResult{}, fmt.Errorf("deliverit: %w", err)
	}
	var result ditSyncResult
	route := []string{"api", "integrations", "projects", projectID, "tasks", "sync"}
	if err := c.call(ctx, http.MethodPost, route, body, &result); err != nil {
		return ditSyncResult{}, err
	}
	return result, nil
}

// marshalDeliverITRequest renders the exact batch body (also shown by explain).
// "<", ">" and "&" stay literal so the explain output reads like the source names.
func marshalDeliverITRequest(req ditSyncRequest) ([]byte, error) {
	if req.Tasks == nil {
		req.Tasks = []ditSyncTask{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// call performs one logical request with the contract's retry policy:
//
//   - 429 (DeliverIT's limiter) and 502/503/504 (its Worker on a cold start) are
//     retried after Retry-After, clamped to [1 s, 30 s], 10 s when absent;
//   - transport errors are retried too. Unlike the Productive POST this is SAFE:
//     GET is a read and the batch is an idempotent upsert keyed by externalId;
//   - 413 (the body does not shrink by waiting), every other 4xx, 500 and every
//     3xx (redirects are never followed) are final.
//
// A wait that does not fit in what is left of the run deadline fails at once.
//
// Messages cite the ROUTE ("GET /api/integrations/projects"), never the request
// URL: its scheme, host and base path are the value of DELIVERIT_BASE_URL.
func (c *deliverITClient) call(ctx context.Context, method string, route []string, body []byte, out any) error {
	target := c.base.JoinPath(route...)
	path := "/" + strings.Join(route, "/")
	if c.readOnly && method != http.MethodGet {
		return fmt.Errorf("deliverit: %w: %s %s", errReadOnly, method, path)
	}

	for attempt := 0; ; attempt++ {
		if err := c.pace.Wait(ctx); err != nil {
			return fmt.Errorf("deliverit: %s %s: %w", method, path, err)
		}
		status, header, respBody, err := c.attempt(ctx, method, target, body)
		c.count(func(s *clientStats) { s.Requests++ })
		if status == http.StatusTooManyRequests {
			c.count(func(s *clientStats) { s.RateLimited++ })
		}

		var wait time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil || attempt >= c.maxRetries {
				return fmt.Errorf("deliverit: %s %s: %s", method, path, c.redact(err.Error()))
			}
			wait = deliverITDefaultRetryWait
		case deliverITRetryableStatus(status) && attempt < c.maxRetries:
			wait = deliverITRetryAfter(header, time.Now())
		default:
			return c.decode(method, path, status, header, respBody, out)
		}

		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			last := fmt.Sprintf("HTTP %d", status)
			if err != nil {
				last = c.redact(err.Error())
			}
			return fmt.Errorf("deliverit: %s %s: %s; a retry in %v would outlive the run deadline", method, path, last, wait)
		}
		c.count(func(s *clientStats) { s.Retries++ })
		c.log.Warn("retrying", "method", method, "path", path, "attempt", attempt+1,
			"status", status, "err", c.redact(errString(err)), "in", wait.String())
		if serr := c.sleep(ctx, wait); serr != nil {
			return fmt.Errorf("deliverit: %s %s: %w", method, path, serr)
		}
	}
}

func deliverITRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// deliverITRetryAfter reads Retry-After as seconds or an HTTP date (RFC 9110).
func deliverITRetryAfter(h http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(h.Get("Retry-After"))
	wait := deliverITDefaultRetryWait
	if value != "" {
		if secs, err := strconv.ParseInt(value, 10, 64); err == nil && secs >= 0 {
			wait = time.Duration(min(secs, int64(deliverITMaxRetryWait/time.Second))) * time.Second
		} else if at, err := http.ParseTime(value); err == nil {
			wait = at.Sub(now)
		}
	}
	return min(max(wait, deliverITMinRetryWait), deliverITMaxRetryWait)
}

func (c *deliverITClient) attempt(ctx context.Context, method string, target *url.URL, body []byte) (int, http.Header, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	// An explicit User-Agent: Cloudflare's Browser Integrity Check challenges
	// clients without one (DIRECT mode goes through the zone).
	req.Header.Set("User-Agent", deliverITUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// *url.Error prefixes the cause with the full request URL, i.e. with
		// DELIVERIT_BASE_URL: only the cause is kept (call names the route). The cause
		// itself (a DNS or TLS error) may still name the host — call redacts it.
		var ue *url.Error
		if errors.As(err, &ue) {
			return 0, nil, nil, ue.Err
		}
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, deliverITMaxResponseBytes))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// decode maps the final response: 2xx -> out; 3xx -> an "unexpected redirect"
// error; problem+json with a code -> *deliverITProblem; anything else ->
// *deliverITHTTPError.
func (c *deliverITClient) decode(method, path string, status int, header http.Header, body []byte, out any) error {
	contentType := header.Get("Content-Type")
	if status >= 200 && status <= 299 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("deliverit: %s %s: unreadable %d response (%s): %v", method, path, status, contentType, err)
		}
		return nil
	}
	if status >= 300 && status <= 399 {
		// Both routes answer 200 or an error; a redirect means DELIVERIT_BASE_URL (or
		// something in front of it) points somewhere else — a login page, an
		// http -> https upgrade. Nothing is retried: waiting does not change that.
		return fmt.Errorf("deliverit: %s %s: unexpected redirect %d%s; redirects are never followed "+
			"(the Authorization header must not follow one) — check DELIVERIT_BASE_URL",
			method, path, status, c.redirectTarget(header.Get("Location")))
	}
	if strings.Contains(contentType, "json") {
		var p deliverITProblem
		if json.Unmarshal(body, &p) == nil && p.Code != "" {
			if p.Status == 0 {
				p.Status = status
			}
			return &p
		}
	}
	text := strings.Join(strings.Fields(strings.ToValidUTF8(c.redact(string(body)), "�")), " ")
	if utf8.RuneCountInString(text) > deliverITSnippetRunes {
		text = string([]rune(text)[:deliverITSnippetRunes])
	}
	return &deliverITHTTPError{
		Status:      status,
		ContentType: contentType,
		Snippet:     text,
		Challenged:  strings.EqualFold(header.Get("cf-mitigated"), "challenge"),
	}
}

// redirectTarget names where a 3xx pointed WITHOUT its query and fragment (a login
// page's returnUrl, a token) and without credentials; what is left is redacted.
func (c *deliverITClient) redirectTarget(location string) string {
	if location == "" {
		return " without a Location"
	}
	u, err := url.Parse(location)
	if err != nil {
		return " with an unreadable Location"
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return " to " + c.redact(u.String())
}

// redact replaces the key with its label and DELIVERIT_BASE_URL's host and path
// with the variable's name wherever a transport error or a response could echo
// them (a DNS error naming the host, a proxy reflecting request headers, an error
// page quoting the path). A value pasted into the wrong variable — a key in
// DELIVERIT_BASE_URL — must not travel on in a message.
func (c *deliverITClient) redact(s string) string {
	return c.redactor.Replace(s)
}

func newDeliverITRedactor(setup deliverITSetup) *strings.Replacer {
	var pairs []string
	add := func(value, label string, anyCase bool) {
		if value == "" {
			return
		}
		pairs = append(pairs, value, label)
		// Host names are case-insensitive: a resolver or a proxy may echo one lowercased.
		if lower := strings.ToLower(value); anyCase && lower != value {
			pairs = append(pairs, lower, label)
		}
	}
	// The key first: at one position the replacer tries the pairs in order.
	if setup.APIKey != "" {
		add(setup.APIKey, deliverITKeyLabel(setup.APIKey), false)
	}
	if base := setup.Base; base != nil {
		// Proxied mode has exactly one host, the compile-time deliverITProxyHost; it
		// stays readable ("lookup deliverit.internal: no such host" is the symptom
		// the README documents).
		if !setup.Proxied {
			add(base.Host, "[DELIVERIT_BASE_URL host]", true) // host:port, before the bare host
			add(base.Hostname(), "[DELIVERIT_BASE_URL host]", true)
		}
		add(strings.TrimSuffix(base.EscapedPath(), "/"), "[DELIVERIT_BASE_URL path]", false)
		add(strings.TrimSuffix(base.Path, "/"), "[DELIVERIT_BASE_URL path]", false)
	}
	return strings.NewReplacer(pairs...)
}

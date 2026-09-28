package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// Contract literals of /api/integrations. The SAME literals freeze the .NET side
// (IntegrationSyncTests, ApiKeyAuthenticationTests and SyncProjectTasksValidatorTests
// in deliverit-internal; the cold-start 503 is its Worker, deploy/cloudflare/src/index.ts):
// changing the shape, a rule or an error code means changing both repositories.
const (
	ditProjectID = "01997a3c-5e1f-7c2a-9b3d-4e5f6a7b8c9d"

	ditLinkedProjectsJSON = `[{"id":"01997a3c-5e1f-7c2a-9b3d-4e5f6a7b8c9d","name":"Portal klienta","clientName":"ACME","taskList":{"source":1,"externalId":"901234567"}}]`
	ditSyncRequestJSON    = `{
  "taskList": { "source": 1, "externalId": "901234567" },
  "tasks": [
    { "externalId": "86c0abc12", "name": "Analiza wymagań" },
    { "externalId": "86c0abc13", "name": "Integracja z API płatności" }
  ]
}`
	ditSyncResultJSON = `{"created":1,"renamed":0,"unchanged":41,"skipped":[{"externalId":"86c0abc14","name":"Analiza","reason":0}]}`

	ditMismatchJSON = `{"type":"https://tools.ietf.org/html/rfc9110#section-15.5.10","title":"Konflikt stanu.","status":409,` +
		`"detail":"Projekt nie jest powiązany z tą listą zadań. Pobierz listę powiązanych projektów ponownie.",` +
		`"code":"PROJECT_TASK_LIST_MISMATCH","traceId":"00-abc-def-01"}`
	ditAPIKeyInvalidJSON = `{"type":"https://tools.ietf.org/html/rfc9110#section-15.5.2","title":"Wymagane uwierzytelnienie.","status":401,` +
		`"detail":"Klucz API jest nieprawidłowy albo został unieważniony.","code":"API_KEY_INVALID","traceId":"00-1-2-01"}`
	ditUnauthenticatedJSON = `{"title":"Wymagane uwierzytelnienie.","status":401,"detail":"Wymagane uwierzytelnienie.","code":"UNAUTHENTICATED","traceId":"00-3-4-01"}`
	ditValidationJSON      = `{"title":"Żądanie jest niepoprawne.","status":400,"detail":"Żądanie jest niepoprawne.","code":"VALIDATION_FAILED",` +
		`"traceId":"00-abc-01","errors":{"Tasks[1].ExternalId":["Identyfikator zadania jest wymagany."],"Tasks[0].Name":["Nazwa zadania jest wymagana."]}}`
	ditColdStartJSON = `{"type":"about:blank","title":"Serwer jest chwilowo niedostępny.","status":503,` +
		`"detail":"Uruchamiamy serwer aplikacji. Spróbuj ponownie za kilkanaście sekund.","code":"SERVICE_UNAVAILABLE","traceId":"8c1f2e3d4c5b6a79-WAW"}`
	ditRateLimitedJSON = `{"title":"Zbyt wiele żądań.","status":429,"detail":"Zbyt wiele prób. Odczekaj chwilę i spróbuj ponownie.","code":"RATE_LIMITED","traceId":"00-5-6-01"}`
)

// scriptedDeliverIT answers with the scripted responses in order; the last one
// repeats forever. It does not check the key — the tests below assert headers.
type scriptedDeliverIT struct {
	mu        sync.Mutex
	requests  []ditRecorded
	hosts     []string
	responses []func(w http.ResponseWriter, r *http.Request)
	srv       *httptest.Server
}

func newScriptedDeliverIT(t *testing.T, responses ...func(w http.ResponseWriter, r *http.Request)) *scriptedDeliverIT {
	t.Helper()
	f := &scriptedDeliverIT{responses: responses}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, ditRecorded{r.Method, r.URL.EscapedPath(), r.Header.Clone(), string(body)})
		f.hosts = append(f.hosts, r.Host)
		i := min(len(f.requests), len(f.responses)) - 1
		f.mu.Unlock()
		f.responses[i](w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *scriptedDeliverIT) recorded() []ditRecorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func respondWith(status int, contentType, body string, headers ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func ditOK(body string) func(w http.ResponseWriter, r *http.Request) {
	return respondWith(http.StatusOK, "application/json; charset=utf-8", body)
}

func ditProblem(status int, body string, headers ...string) func(w http.ResponseWriter, r *http.Request) {
	return respondWith(status, "application/problem+json", body, headers...)
}

// testDeliverITClient builds a direct-mode client with the production retry budget
// (MAX_RETRIES=4, i.e. 5 attempts) and records every retry wait instead of sleeping.
func testDeliverITClient(t *testing.T, base string) (*deliverITClient, *[]time.Duration) {
	t.Helper()
	cfg := testConfig("http://x/", "http://y/")
	cfg.DeliverITBaseURL = base
	cfg.DeliverITAPIKey = testDeliverITKey
	cfg.MaxRetries = 4
	setup := cfg.deliverITSetup()
	if !setup.Enabled {
		t.Fatalf("setup disabled: %+v", setup)
	}
	c := newDeliverITClient(setup, cfg, false, testLogger())
	var mu sync.Mutex
	sleeps := &[]time.Duration{}
	c.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		*sleeps = append(*sleeps, d)
		return nil
	}
	return c, sleeps
}

func sampleSyncRequest() ditSyncRequest {
	return ditSyncRequest{
		TaskList: ditTaskList{Source: taskSourceClickUp, ExternalID: "901234567"},
		Tasks: []ditSyncTask{
			{ExternalID: "86c0abc12", Name: "Analiza wymagań"},
			{ExternalID: "86c0abc13", Name: "Integracja z API płatności"},
		},
	}
}

func assertSameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON differs from the contract:\ngot:  %s\nwant: %s", got, want)
	}
}

func TestDeliverITListLinkedProjectsSendsTheKeyAndDecodesTheContract(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditOK(ditLinkedProjectsJSON))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	projects, err := c.listLinkedProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ditLinkedProject{{
		ID: ditProjectID, Name: "Portal klienta", ClientName: "ACME",
		TaskList: ditTaskList{Source: taskSourceClickUp, ExternalID: "901234567"},
	}}
	if !slices.Equal(projects, want) {
		t.Fatalf("projects = %+v", projects)
	}
	req := fake.recorded()[0]
	if req.Method != http.MethodGet || req.Path != "/api/integrations/projects" {
		t.Fatalf("request: %s %s", req.Method, req.Path)
	}
	for header, want := range map[string]string{
		"Authorization": "Bearer " + testDeliverITKey,
		"Accept":        "application/json",
		"User-Agent":    "pcuintegrator/1 (+deliverit-internal)",
	} {
		if got := req.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// SameSiteGuard in DeliverIT lets a non-browser client through only without these.
	for _, h := range []string{"Origin", "Sec-Fetch-Site", "Cookie"} {
		if req.Header.Get(h) != "" {
			t.Errorf("%s must not be sent", h)
		}
	}
}

func TestDeliverITEmptyProjectListIsAnEmptySlice(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditOK(`[]`))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	projects, err := c.listLinkedProjects(context.Background())
	if err != nil || projects == nil || len(projects) != 0 {
		t.Fatalf("want an empty list, got %#v / %v", projects, err)
	}
}

func TestDeliverITBaseURLPathIsJoinedWithTheAPIPath(t *testing.T) {
	for _, suffix := range []string{"/", "/proxy", "/proxy/"} {
		fake := newScriptedDeliverIT(t, ditOK(`[]`))
		c, _ := testDeliverITClient(t, fake.srv.URL+suffix)

		if _, err := c.listLinkedProjects(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := strings.TrimSuffix(suffix, "/") + "/api/integrations/projects"
		if got := fake.recorded()[0].Path; got != want {
			t.Fatalf("base %q: path %q, want %q", suffix, got, want)
		}
	}
}

func TestDeliverITSyncTasksSendsTheContractBodyAndDecodesTheReport(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditOK(ditSyncResultJSON))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	result, err := c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest())
	if err != nil {
		t.Fatal(err)
	}
	req := fake.recorded()[0]
	if req.Method != http.MethodPost || req.Path != "/api/integrations/projects/"+ditProjectID+"/tasks/sync" {
		t.Fatalf("request: %s %s", req.Method, req.Path)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testDeliverITKey {
		t.Fatalf("Authorization = %q", got)
	}
	assertSameJSON(t, req.Body, ditSyncRequestJSON)
	want := ditSyncResult{Created: 1, Unchanged: 41, Skipped: []ditSkippedTask{
		{ExternalID: "86c0abc14", Name: "Analiza", Reason: ditSkipNameTaken},
	}}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("report = %+v", result)
	}
}

func TestDeliverITNilTasksAreSentAsAnEmptyList(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditOK(`{"created":0,"renamed":0,"unchanged":0,"skipped":[]}`))
	c, _ := testDeliverITClient(t, fake.srv.URL)
	req := sampleSyncRequest()
	req.Tasks = nil

	if _, err := c.syncTasks(context.Background(), ditProjectID, req); err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, fake.recorded()[0].Body, `{"taskList":{"source":1,"externalId":"901234567"},"tasks":[]}`)
}

// The DeliverIT Worker answers a cold container with 503 SERVICE_UNAVAILABLE and
// Retry-After: 10. The batch is an idempotent upsert, so re-sending it is safe.
func TestDeliverITColdStartIsRetriedAfterRetryAfter(t *testing.T) {
	fake := newScriptedDeliverIT(t,
		ditProblem(503, ditColdStartJSON, "Retry-After", "10"),
		ditProblem(503, ditColdStartJSON, "Retry-After", "10"),
		ditOK(ditSyncResultJSON))
	c, sleeps := testDeliverITClient(t, fake.srv.URL)

	if _, err := c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*sleeps, []time.Duration{10 * time.Second, 10 * time.Second}) {
		t.Fatalf("waits = %v", *sleeps)
	}
	requests := fake.recorded()
	if len(requests) != 3 {
		t.Fatalf("attempts = %d, want 3", len(requests))
	}
	for _, req := range requests {
		assertSameJSON(t, req.Body, ditSyncRequestJSON)
	}
	if st := c.Stats(); st.Requests != 3 || st.Retries != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestDeliverITRetriedStatusesHonourRetryAfter(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504} {
		fake := newScriptedDeliverIT(t, ditProblem(status, ditRateLimitedJSON, "Retry-After", "7"), ditOK(`[]`))
		c, sleeps := testDeliverITClient(t, fake.srv.URL)

		if _, err := c.listLinkedProjects(context.Background()); err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if !slices.Equal(*sleeps, []time.Duration{7 * time.Second}) {
			t.Fatalf("%d: waits %v", status, *sleeps)
		}
	}

	// No header -> the cold-start default; absurd header -> capped; "0" -> floor.
	for header, want := range map[string]time.Duration{
		"":    10 * time.Second,
		"600": 30 * time.Second,
		"0":   time.Second,
	} {
		var headers []string
		if header != "" {
			headers = []string{"Retry-After", header}
		}
		fake := newScriptedDeliverIT(t, ditProblem(503, ditColdStartJSON, headers...), ditOK(`[]`))
		c, sleeps := testDeliverITClient(t, fake.srv.URL)
		if _, err := c.listLinkedProjects(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(*sleeps, []time.Duration{want}) {
			t.Fatalf("Retry-After %q: waits %v, want [%v]", header, *sleeps, want)
		}
	}
}

func TestDeliverITRetriesStopAfterMaxRetries(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditProblem(503, ditColdStartJSON, "Retry-After", "10"))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	_, err := c.listLinkedProjects(context.Background())

	var p *deliverITProblem
	if !errors.As(err, &p) || p.Code != "SERVICE_UNAVAILABLE" {
		t.Fatalf("want SERVICE_UNAVAILABLE, got %v", err)
	}
	if got := len(fake.recorded()); got != 5 {
		t.Fatalf("attempts = %d, want 5 (MAX_RETRIES=4)", got)
	}
}

func TestDeliverITRejectedKeyIsAnAuthProblemWithoutRetries(t *testing.T) {
	for _, body := range []string{ditAPIKeyInvalidJSON, ditUnauthenticatedJSON} {
		fake := newScriptedDeliverIT(t, ditProblem(401, body, "WWW-Authenticate", "Bearer"))
		c, sleeps := testDeliverITClient(t, fake.srv.URL)

		_, err := c.listLinkedProjects(context.Background())

		var p *deliverITProblem
		if !errors.As(err, &p) || !p.isAuth() || p.Status != 401 {
			t.Fatalf("want an auth problem, got %v", err)
		}
		if len(fake.recorded()) != 1 || len(*sleeps) != 0 {
			t.Fatalf("401 must not be retried: %d attempts", len(fake.recorded()))
		}
	}
}

// 4xx is final; so is 500 — DeliverIT answers a unique-index race with 500 and the
// next run heals it, so hammering it now buys nothing. 500 is its exception handler:
// code UNEXPECTED (ErrorCodes.Unexpected), never a platform 5xx without JSON.
func TestDeliverITClientErrorsAreNotRetried(t *testing.T) {
	for status, body := range map[int]string{
		400: ditValidationJSON,
		404: `{"title":"Nie znaleziono.","status":404,"detail":"Projekt nie istnieje.","code":"PROJECT_NOT_FOUND","traceId":"t"}`,
		409: ditMismatchJSON,
		500: `{"title":"Błąd serwera.","status":500,"detail":"Wystąpił nieoczekiwany błąd.","code":"UNEXPECTED","traceId":"t"}`,
	} {
		fake := newScriptedDeliverIT(t, ditProblem(status, body))
		c, _ := testDeliverITClient(t, fake.srv.URL)

		_, err := c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest())

		var p *deliverITProblem
		if !errors.As(err, &p) || p.Status != status || p.isAuth() {
			t.Fatalf("%d: want a problem, got %v", status, err)
		}
		if got := len(fake.recorded()); got != 1 {
			t.Fatalf("%d must not be retried, attempts: %d", status, got)
		}
	}
}

func TestDeliverITProblemMessages(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditProblem(409, ditMismatchJSON))
	c, _ := testDeliverITClient(t, fake.srv.URL)
	_, err := c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest())
	want := "deliverit: 409 PROJECT_TASK_LIST_MISMATCH: Projekt nie jest powiązany z tą listą zadań. " +
		"Pobierz listę powiązanych projektów ponownie. [traceId 00-abc-def-01]"
	if err == nil || err.Error() != want {
		t.Fatalf("message:\n%v\nwant:\n%s", err, want)
	}

	fake = newScriptedDeliverIT(t, ditProblem(400, ditValidationJSON))
	c, _ = testDeliverITClient(t, fake.srv.URL)
	_, err = c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest())
	want = "deliverit: 400 VALIDATION_FAILED: Żądanie jest niepoprawne. [traceId 00-abc-01] " +
		"(Tasks[0].Name: Nazwa zadania jest wymagana.; Tasks[1].ExternalId: Identyfikator zadania jest wymagany.)"
	if err == nil || err.Error() != want {
		t.Fatalf("message:\n%v\nwant:\n%s", err, want)
	}
}

// A Cloudflare challenge (Bot Fight Mode / Browser Integrity Check on the
// deliverit.pl zone) is a 403 HTML page, not problem+json.
func TestDeliverITCloudflareChallengeIsAnHTTPError(t *testing.T) {
	fake := newScriptedDeliverIT(t, respondWith(403, "text/html; charset=UTF-8",
		"<!DOCTYPE html>\n<html><head><title>Just a moment...</title></head></html>", "cf-mitigated", "challenge"))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	_, err := c.listLinkedProjects(context.Background())

	var httpErr *deliverITHTTPError
	if !errors.As(err, &httpErr) || !httpErr.Challenged || httpErr.Status != 403 {
		t.Fatalf("want a challenged HTTP error, got %v", err)
	}
	if httpErr.Snippet != "<!DOCTYPE html> <html><head><title>Just a moment...</title></head></html>" {
		t.Fatalf("snippet = %q", httpErr.Snippet)
	}
	for _, hint := range []string{"Cloudflare", "Bot Fight Mode", "README"} {
		if !strings.Contains(err.Error(), hint) {
			t.Fatalf("message lacks %q: %s", hint, err)
		}
	}
}

// .NET answers an oversized body with 413 and an EMPTY body; waiting does not make
// the batch smaller.
func TestDeliverITTooLargeBatchIsAnErrorWithoutRetries(t *testing.T) {
	fake := newScriptedDeliverIT(t, respondWith(413, "", ""))
	c, sleeps := testDeliverITClient(t, fake.srv.URL)

	_, err := c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest())

	var httpErr *deliverITHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 413 || httpErr.Challenged {
		t.Fatalf("want HTTP error 413, got %v", err)
	}
	if len(fake.recorded()) != 1 || len(*sleeps) != 0 {
		t.Fatalf("413 must not be retried: %d attempts", len(fake.recorded()))
	}
}

func TestDeliverITJSONWithoutCodeIsAnHTTPError(t *testing.T) {
	fake := newScriptedDeliverIT(t, respondWith(400, "application/json", `{"message":"bad"}`))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	_, err := c.listLinkedProjects(context.Background())

	var httpErr *deliverITHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 400 {
		t.Fatalf("want HTTP error 400, got %v", err)
	}
}

func TestDeliverITUnreadableSuccessIsAnError(t *testing.T) {
	fake := newScriptedDeliverIT(t, respondWith(200, "text/html", "<html>login</html>"))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	_, err := c.listLinkedProjects(context.Background())
	if err == nil || !strings.Contains(err.Error(), "200") {
		t.Fatalf("want a decode error naming the status, got %v", err)
	}
}

func TestDeliverITKeyNeverAppearsInErrors(t *testing.T) {
	echo := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<pre>Authorization: "+r.Header.Get("Authorization")+"</pre>")
	}
	for name, response := range map[string]func(w http.ResponseWriter, r *http.Request){
		"problem":   ditProblem(401, ditAPIKeyInvalidJSON),
		"html-echo": echo,
		"undecoded": respondWith(200, "application/json", "{"),
	} {
		fake := newScriptedDeliverIT(t, response)
		c, _ := testDeliverITClient(t, fake.srv.URL)
		_, err := c.listLinkedProjects(context.Background())
		if err == nil || strings.Contains(err.Error(), testDeliverITKey) {
			t.Fatalf("%s: error %v must not contain the key", name, err)
		}
		if name == "html-echo" && !strings.Contains(err.Error(), "dit_Ab3dE5gH…") {
			t.Fatalf("an echoed key must be replaced by its prefix: %v", err)
		}
	}
}

// A value pasted into DELIVERIT_BASE_URL (here a second key, in the path or as the
// host) must not come back in any client error or log line: the messages cite the
// ROUTE and name the variable. Transport errors (*url.Error carries the full URL,
// a DNS error the host), an error page echoing the path and the client's own
// messages are all covered.
func TestDeliverITClientErrorsNeverQuoteTheBaseURL(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	echoPath := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<p>Cannot GET "+r.URL.Path+" on "+r.Host+"</p>")
	}
	failDNS := &http.Transport{DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		// What net.Dialer returns when the name does not resolve.
		return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
	}}

	// behindBase serves one scripted response under a base path that is the stray key.
	behindBase := func(response func(w http.ResponseWriter, r *http.Request)) func(t *testing.T) string {
		return func(t *testing.T) string { return newScriptedDeliverIT(t, response).srv.URL + "/" + strayDeliverITKey }
	}

	cases := []struct {
		name      string
		base      func(t *testing.T) string
		transport http.RoundTripper
		deadline  time.Duration // 0 = none
		// namesVariable: the message replaces the echoed value with the variable's name.
		namesVariable bool
		// citesRoute: the client's own message says which request failed (a response
		// error keeps the frozen "deliverit: <status> …" shape instead).
		citesRoute bool
	}{
		{name: "transport error, key as the base path", namesVariable: true, citesRoute: true,
			base: func(*testing.T) string { return closed.URL + "/" + strayDeliverITKey }},
		{name: "DNS failure, key as the host", namesVariable: true, citesRoute: true, transport: failDNS,
			base: func(*testing.T) string { return "https://" + strayDeliverITKey }},
		{name: "error page echoing the path and host", namesVariable: true, base: behindBase(echoPath)},
		{name: "unreadable success", citesRoute: true,
			base: behindBase(respondWith(200, "text/html", "<html>login</html>"))},
		{name: "retry that would outlive the deadline", citesRoute: true, deadline: 2 * time.Second,
			base: behindBase(ditProblem(503, ditColdStartJSON, "Retry-After", "10"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := testDeliverITClient(t, tc.base(t))
			if tc.transport != nil {
				c.hc.Transport = tc.transport
			}
			var logs bytes.Buffer
			c.log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			ctx := context.Background()
			if tc.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.deadline)
				defer cancel()
			}

			_, err := c.listLinkedProjects(ctx)

			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), strayDeliverITKey) || strings.Contains(logs.String(), strayDeliverITKey) {
				t.Fatalf("the base URL is quoted:\nerror: %v\nlogs: %s", err, logs.String())
			}
			if tc.citesRoute && !strings.Contains(err.Error(), "GET /api/integrations/projects") {
				t.Fatalf("the error must cite the route: %v", err)
			}
			if tc.namesVariable && !strings.Contains(err.Error(), "DELIVERIT_BASE_URL") {
				t.Fatalf("the error must name the variable: %v", err)
			}
		})
	}
}

// The client never follows a redirect: Go would re-send Authorization to the same
// host name (or a subdomain) on any port and scheme, plain http included — as here,
// 127.0.0.1 on another port — and a 307/308 would re-send the batch too. A 3xx is a final error naming the target WITHOUT its query
// (which could carry a token) — and the second server never sees a request.
func TestDeliverITClientNeverFollowsRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			other := newScriptedDeliverIT(t, ditOK(`[]`))
			fake := newScriptedDeliverIT(t, respondWith(status, "text/html", `<a href="elsewhere">Moved</a>`,
				"Location", other.srv.URL+"/logowanie?returnUrl=%2Fapi&token=s3cr3t#frag"))
			c, sleeps := testDeliverITClient(t, fake.srv.URL)

			var err error
			if method == http.MethodGet {
				_, err = c.listLinkedProjects(context.Background())
			} else {
				_, err = c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest())
			}

			if err == nil || !strings.Contains(err.Error(), "unexpected redirect "+strconv.Itoa(status)) {
				t.Fatalf("%d %s: want an unexpected redirect error, got %v", status, method, err)
			}
			if !strings.Contains(err.Error(), "/logowanie") {
				t.Fatalf("%d %s: the error should name the redirect target: %v", status, method, err)
			}
			for _, leak := range []string{"s3cr3t", "token=", "returnUrl", "frag"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("%d %s: the Location query/fragment leaked (%s): %v", status, method, leak, err)
				}
			}
			if got := len(other.recorded()); got != 0 {
				t.Fatalf("%d %s: the redirect was followed, the other server got %d request(s)", status, method, got)
			}
			if len(fake.recorded()) != 1 || len(*sleeps) != 0 {
				t.Fatalf("%d %s: a redirect must not be retried: %d attempts", status, method, len(fake.recorded()))
			}
		}
	}
}

// Both routes are idempotent (GET, and a server-side upsert keyed by externalId),
// so unlike the Productive POST a lost response may be retried.
func TestDeliverITTransportErrorsAreRetriedForGetAndPost(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	c, sleeps := testDeliverITClient(t, closed.URL)
	if _, err := c.listLinkedProjects(context.Background()); err == nil || strings.Contains(err.Error(), testDeliverITKey) {
		t.Fatalf("GET: transport error %v (must exist, must not contain the key)", err)
	}
	if len(*sleeps) != 4 {
		t.Fatalf("GET: %d retry waits, want 4", len(*sleeps))
	}

	c, sleeps = testDeliverITClient(t, closed.URL)
	if _, err := c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest()); err == nil {
		t.Fatal("POST: want a transport error")
	}
	if len(*sleeps) != 4 {
		t.Fatalf("POST: %d retry waits, want 4", len(*sleeps))
	}
}

// DELIVERIT_RPS paces EVERY attempt, including retries: DeliverIT's limiter
// (300/min per IP) runs before authentication.
func TestDeliverITPacingKeepsTheMinimumInterval(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditOK(`[]`))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	now := time.Unix(1_700_000_000, 0)
	var paced []time.Duration
	c.pace = newLimiter(deliverITMaxRPS, 1)
	c.pace.now = func() time.Time { return now }
	c.pace.sleep = func(_ context.Context, d time.Duration) error {
		paced = append(paced, d)
		now = now.Add(d)
		return nil
	}

	for range 3 {
		if _, err := c.listLinkedProjects(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	want := []time.Duration{250 * time.Millisecond, 250 * time.Millisecond}
	if !slices.Equal(paced, want) {
		t.Fatalf("intervals %v, want %v (DELIVERIT_RPS ceiling = 250 ms apart)", paced, want)
	}
}

func TestDeliverITSyncRejectsMalformedProjectIDWithoutARequest(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditOK(ditSyncResultJSON))
	c, _ := testDeliverITClient(t, fake.srv.URL)

	for _, id := range []string{"", "..", "../users", ditProjectID + "/x", "01997a3c-5e1f-7c2a-9b3d-4e5f6a7b8c9"} {
		if _, err := c.syncTasks(context.Background(), id, sampleSyncRequest()); err == nil {
			t.Fatalf("project id %q must be rejected", id)
		}
	}
	if got := len(fake.recorded()); got != 0 {
		t.Fatalf("no request may leave, got %d", got)
	}
}

// dry_run=true: the POST is suppressed at the lowest layer, like the Productive client.
func TestDeliverITReadOnlyClientBlocksPost(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditOK(ditSyncResultJSON))
	cfg := withDeliverIT(testConfig("http://x/", "http://y/"), &fakeDeliverIT{srv: fake.srv})
	c := newDeliverITClient(cfg.deliverITSetup(), cfg, true, testLogger())

	if _, err := c.syncTasks(context.Background(), ditProjectID, sampleSyncRequest()); !errors.Is(err, errReadOnly) {
		t.Fatalf("want errReadOnly, got %v", err)
	}
	if got := len(fake.recorded()); got != 0 {
		t.Fatalf("a read-only client sent %d requests", got)
	}
}

// Proxied mode: the container talks plain HTTP to the virtual host
// deliverit.internal, the Worker's outbound handler sets Authorization from its own
// secret. The container must not carry or send the key.
func TestDeliverITProxiedModeSendsNoAuthorization(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditOK(ditLinkedProjectsJSON))
	cfg := testConfig("http://x/", "http://y/")
	cfg.DeliverITBaseURL = "http://deliverit.internal"
	cfg.DeliverITKeyPrefix = "dit_Ab3dE5gH"
	setup := cfg.deliverITSetup()
	if !setup.Enabled || !setup.Proxied {
		t.Fatalf("setup = %+v", setup)
	}
	c := newDeliverITClient(setup, cfg, false, testLogger())
	// Route "deliverit.internal" to the fake, as the platform's interception does.
	addr := fake.srv.Listener.Addr().String()
	c.hc.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}

	if _, err := c.listLinkedProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := fake.recorded()[0]
	if req.Header.Get("Authorization") != "" {
		t.Fatal("proxied mode must not send Authorization — the Worker injects it")
	}
	if fake.hosts[0] != "deliverit.internal" || req.Path != "/api/integrations/projects" {
		t.Fatalf("request went to %s%s", fake.hosts[0], req.Path)
	}
}

// A Retry-After that does not fit in what is left of SYNC_TIMEOUT fails now
// instead of sleeping into the deadline.
func TestDeliverITRetryWaitNeverOutlivesTheDeadline(t *testing.T) {
	fake := newScriptedDeliverIT(t, ditProblem(503, ditColdStartJSON, "Retry-After", "10"))
	c, sleeps := testDeliverITClient(t, fake.srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := c.listLinkedProjects(ctx)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("want a deadline error, got %v", err)
	}
	if len(*sleeps) != 0 || len(fake.recorded()) != 1 {
		t.Fatalf("waits %v, attempts %d", *sleeps, len(fake.recorded()))
	}
}

// --- rules repeated from the DeliverIT validator ---

// An element breaking these rules would cost the WHOLE batch a 400, so it is
// filtered client-side instead.
func TestValidDeliverITTaskExternalID(t *testing.T) {
	for _, id := range []string{"86c0abc12", "a", "A-b_9", strings.Repeat("x", 100), "123456789"} {
		if !validDeliverITTaskExternalID(id) {
			t.Errorf("%q must be valid", id)
		}
	}
	for _, id := range []string{"", " ", "a b", "a/b", "a.b", "ą", "abc\n", "١٢٣", strings.Repeat("x", 101)} {
		if validDeliverITTaskExternalID(id) {
			t.Errorf("%q must be rejected", id)
		}
	}
}

func TestNormalizeDeliverITTaskName(t *testing.T) {
	if got := normalizeDeliverITTaskName("  Analiza wymagań \t\n"); got != "Analiza wymagań" {
		t.Fatalf("trim: %q", got)
	}
	if got := normalizeDeliverITTaskName("  \t "); got != "" {
		t.Fatalf("whitespace only -> empty, got %q", got)
	}
	// PostgreSQL text cannot hold U+0000: one such name would cost the whole batch a 500.
	for name, want := range map[string]string{
		"Ana\x00liza\x00": "Analiza",
		"\x00  Analiza":   "Analiza",
		" \x00 \x00 ":     "",
	} {
		if got := normalizeDeliverITTaskName(name); got != want {
			t.Fatalf("NUL: %q -> %q, want %q", name, got, want)
		}
	}
	long := strings.Repeat("ż", deliverITMaxNameRunes+5)
	got := normalizeDeliverITTaskName(long)
	if utf8.RuneCountInString(got) != deliverITMaxNameRunes || !utf8.ValidString(got) {
		t.Fatalf("long name: %d runes, valid UTF-8: %v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	if deliverITMaxNameRunes != 1000 {
		t.Fatalf("name cap is 1000 runes, got %d", deliverITMaxNameRunes)
	}
}

func TestValidDeliverITAPIKeyAndPrefix(t *testing.T) {
	if !validDeliverITAPIKey(testDeliverITKey) {
		t.Fatal("the test key has a valid shape")
	}
	for _, key := range []string{"", "dit_", "abc", "DIT_abc", "dit_a b", "dit_a\n", "dit_a+b", "dit_a=b"} {
		if validDeliverITAPIKey(key) {
			t.Errorf("%q must be rejected", key)
		}
	}
	// ApiKey.KeyPrefix in DeliverIT: "dit_" + 8 — the same string /integracje shows.
	if got := deliverITKeyLabel(testDeliverITKey); got != "dit_Ab3dE5gH…" {
		t.Fatalf("label = %q", got)
	}
	if !validDeliverITKeyPrefix("dit_Ab3dE5gH") {
		t.Fatal("a 12-char prefix is valid")
	}
	for _, prefix := range []string{"", "invalid", "dit_Ab3", "dit_Ab3dE5gHq", "dit_Ab3dE5g+"} {
		if validDeliverITKeyPrefix(prefix) {
			t.Errorf("prefix %q must be rejected", prefix)
		}
	}
}

func TestParseDeliverITBaseURL(t *testing.T) {
	for base, proxied := range map[string]bool{
		"https://internal.deliverit.pl":  false,
		"https://internal.deliverit.pl/": false,
		"http://127.0.0.1:5080":          false,
		"http://localhost:5080":          false,
		"http://[::1]:5080":              false,
		"http://deliverit.internal":      true,
	} {
		u, isProxied, err := parseDeliverITBaseURL(base)
		if err != nil || u == nil || isProxied != proxied {
			t.Errorf("%s: proxied=%v err=%v", base, isProxied, err)
		}
	}
	for _, base := range []string{"internal.deliverit.pl", "http://internal.deliverit.pl", "ftp://internal.deliverit.pl",
		"https://", "https://user:secret@internal.deliverit.pl", "https://internal.deliverit.pl?x=1",
		"https://internal.deliverit.pl#x", "http://127.0.0.2.example.com", "https://deliverit.internal"} {
		if _, _, err := parseDeliverITBaseURL(base); err == nil {
			t.Errorf("%q must be rejected", base)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error must not echo credentials: %v", base, err)
		}
	}
}

func TestDeliverITEnumsReadForHumans(t *testing.T) {
	for reason, want := range map[ditSkipReason]string{
		ditSkipNameTaken:                "name_taken",
		ditSkipExternalIDInOtherProject: "external_id_in_other_project",
		7:                               "unknown_reason_7",
	} {
		if got := reason.String(); got != want {
			t.Errorf("reason %d = %q", int(reason), got)
		}
	}
	for source, want := range map[taskSource]string{
		taskSourceManual:     "manual",
		taskSourceClickUp:    "clickup",
		taskSourceProductive: "productive",
		3:                    "unknown(3)",
	} {
		if got := source.String(); got != want {
			t.Errorf("source %d = %q", int(source), got)
		}
	}
}

func TestDeliverITBatchSizeLeavesHeadroomUnderTheServerLimit(t *testing.T) {
	if deliverITBatchSize != 200 || deliverITBatchSize > deliverITServerMaxTasks || deliverITServerMaxTasks != 500 {
		t.Fatalf("batch %d, server limit %d", deliverITBatchSize, deliverITServerMaxTasks)
	}
}

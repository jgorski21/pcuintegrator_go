package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- shared helpers ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testConfig mirrors production defaults but with limiters wound right up, so the
// suite runs in milliseconds instead of one write per second. Built directly
// rather than through LoadConfig so validate()'s (correct) production ceilings do
// not apply.
func testConfig(clickUpBase, productiveBase string) Config {
	return Config{
		ClickUpToken:    "pk_test",
		ProductiveToken: "prod_test",
		ProductiveOrgID: "org1",

		ClickUpBaseURL:    clickUpBase,
		ProductiveBaseURL: productiveBase,

		ClickUpListID:        "900501332334",
		ProductiveProjectID:  "860646",
		ProductiveTaskListID: "2385788",
		CFClickUpID:          "242457",
		CFClickUpTags:        "242565",
		StatusIDOpen:         "161082",
		StatusIDDone:         "161083",

		ClickUpDoneStatuses: map[string]bool{"zawieszone": true, "gotowe do wydania": true, "wydane": true},
		ProductiveDoneNames: map[string]bool{"Closed": true},
		EstimateFieldIDs: []string{
			"33afbaec-1fae-49be-9e9f-35eb789ef911",
			"611a0462-acb0-4c72-addc-927e62b116d6",
		},
		EstimateLabelMinutes: defaultEstimateLabels(),

		MergeCustomFields: true,
		EstimateMode:      EstimateIgnoreNil,

		ProductiveRPS:      10_000,
		ProductiveBurstRPS: 10_000,
		ClickUpRPS:         10_000,
		MaxCreates:         1_000,
		MaxWrites:          10_000,
		MaxPages:           50,
		MaxRetries:         3,
		SyncTimeout:        30 * time.Second,
		HTTPTimeout:        5 * time.Second,
		LogLevel:           slog.LevelError,
	}
}

type recordedCall struct {
	Method string
	Path   string
	Body   string
}

// --- fake ClickUp ---

type fakeClickUp struct {
	mu    sync.Mutex
	tasks []map[string]any // raw JSON task objects, so tests can use odd shapes
	calls []recordedCall

	// failWith, when non-zero, makes every read return this status with failBody.
	failWith int
	failBody string

	srv *httptest.Server
}

func newFakeClickUp(t *testing.T, tasks []map[string]any) *fakeClickUp {
	t.Helper()
	f := &fakeClickUp{tasks: tasks}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClickUp) baseURL() string { return f.srv.URL + "/api/v2/" }

func (f *fakeClickUp) recorded() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls...)
}

func (f *fakeClickUp) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{Method: r.Method, Path: r.URL.RequestURI()})
	failWith, failBody := f.failWith, f.failBody
	tasks := f.tasks
	f.mu.Unlock()

	if failWith != 0 {
		w.WriteHeader(failWith)
		_, _ = w.Write([]byte(failBody))
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	const size = 100 // ClickUp's fixed page size
	start := page * size
	if start > len(tasks) {
		start = len(tasks)
	}
	end := min(start+size, len(tasks))
	slice := tasks[start:end]

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tasks":     slice,
		"last_page": end >= len(tasks),
	})
}

// clickUpTaskJSON builds a minimal-but-realistic ClickUp task object.
func clickUpTaskJSON(id, name, status string) map[string]any {
	return map[string]any{
		"id":     id,
		"name":   name,
		"status": map[string]any{"status": status, "type": "custom", "orderindex": 0},
		"tags":   []any{},
	}
}

// --- fake Productive ---

type fakeProductiveTask struct {
	ID               string
	Title            string
	InitialEstimate  *int
	CustomFields     map[string]json.RawMessage
	WorkflowStatusID string
	ParentTaskID     string
}

type fakeProductive struct {
	mu     sync.Mutex
	tasks  map[string]*fakeProductiveTask
	nextID int
	calls  []recordedCall

	statusNames map[string]string

	// readFailStatus makes every GET return this status with readFailBody. This is
	// the "soft read failure" scenario: without a hard abort it would look like an
	// empty Productive and duplicate the entire ClickUp list.
	readFailStatus int
	readFailBody   string

	// postThenHangup stores the task and then drops the connection with no
	// response, i.e. the ambiguous-write case that must NOT be retried.
	postThenHangup bool

	// rateLimitFirstN returns 429 for the first N write attempts.
	rateLimitFirstN int

	// includedOnlyFirstPage exercises the "page with no `included`" shape that the
	// .NET code dereferences unconditionally.
	includedOnlyFirstPage bool

	// omitTotals drops meta.total_pages/total_count so the empty-page fallback runs.
	omitTotals bool

	srv *httptest.Server
}

func newFakeProductive(t *testing.T) *fakeProductive {
	t.Helper()
	f := &fakeProductive{
		tasks:                 map[string]*fakeProductiveTask{},
		nextID:                9000,
		statusNames:           map[string]string{"161082": "Open", "161083": "Closed"},
		includedOnlyFirstPage: true,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProductive) baseURL() string { return f.srv.URL + "/api/v2/" }

func (f *fakeProductive) recorded() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls...)
}

func (f *fakeProductive) writes() []recordedCall {
	var out []recordedCall
	for _, c := range f.recorded() {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeProductive) seed(t *fakeProductiveTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.CustomFields == nil {
		t.CustomFields = map[string]json.RawMessage{}
	}
	if t.WorkflowStatusID == "" {
		t.WorkflowStatusID = "161082"
	}
	f.tasks[t.ID] = t
}

func seededTask(id, clickUpID, title, tags string) *fakeProductiveTask {
	return &fakeProductiveTask{
		ID:    id,
		Title: title,
		CustomFields: map[string]json.RawMessage{
			"242457": json.RawMessage(`"` + clickUpID + `"`),
			"242565": json.RawMessage(`"` + tags + `"`),
		},
		WorkflowStatusID: "161082",
	}
}

func (f *fakeProductive) handle(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{
		Method: r.Method,
		Path:   r.URL.RequestURI(),
		Body:   string(bodyBytes),
	})
	f.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		f.handleList(w, r)
	case http.MethodPost:
		f.handleCreate(w, bodyBytes)
	case http.MethodPatch:
		f.handleUpdate(w, r, bodyBytes)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeProductive) handleList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	failStatus, failBody := f.readFailStatus, f.readFailBody
	all := make([]*fakeProductiveTask, 0, len(f.tasks))
	for _, t := range f.tasks {
		all = append(all, t)
	}
	statusNames := f.statusNames
	includedOnlyFirst, omitTotals := f.includedOnlyFirstPage, f.omitTotals
	f.mu.Unlock()

	if failStatus != 0 {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		w.WriteHeader(failStatus)
		_, _ = w.Write([]byte(failBody))
		return
	}

	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	page, _ := strconv.Atoi(r.URL.Query().Get("page[number]"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("page[size]"))
	if size <= 0 {
		size = 30
	}
	if size > 200 {
		size = 200
	}

	start := (page - 1) * size
	if start > len(all) {
		start = len(all)
	}
	end := min(start+size, len(all))

	data := make([]map[string]any, 0, end-start)
	for _, t := range all[start:end] {
		attrs := map[string]any{
			"title":         t.Title,
			"custom_fields": t.CustomFields,
		}
		if t.InitialEstimate != nil {
			attrs["initial_estimate"] = *t.InitialEstimate
		} else {
			attrs["initial_estimate"] = nil
		}
		rels := map[string]any{
			"workflow_status": map[string]any{
				"data": map[string]any{"type": "workflow_statuses", "id": t.WorkflowStatusID},
			},
		}
		if t.ParentTaskID != "" {
			rels["parent_task"] = map[string]any{
				"data": map[string]any{"type": "tasks", "id": t.ParentTaskID},
			}
		} else {
			rels["parent_task"] = map[string]any{"data": nil}
		}
		data = append(data, map[string]any{
			"id": t.ID, "type": "tasks", "attributes": attrs, "relationships": rels,
		})
	}

	resp := map[string]any{"data": data}

	if page == 1 || !includedOnlyFirst {
		included := make([]map[string]any, 0, len(statusNames))
		ids := make([]string, 0, len(statusNames))
		for id := range statusNames {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			included = append(included, map[string]any{
				"id": id, "type": "workflow_statuses",
				"attributes": map[string]any{"name": statusNames[id]},
			})
		}
		resp["included"] = included
	}

	if !omitTotals {
		totalPages := (len(all) + size - 1) / size
		if totalPages == 0 {
			totalPages = 1
		}
		resp["meta"] = map[string]any{
			"current_page":  page,
			"total_pages":   totalPages,
			"total_count":   len(all),
			"page_size":     size,
			"max_page_size": 200,
		}
	}

	w.Header().Set("Content-Type", "application/vnd.api+json")
	_ = json.NewEncoder(w).Encode(resp)
}

type incomingWrite struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Title           string                     `json:"title"`
			InitialEstimate json.RawMessage            `json:"initial_estimate"`
			CustomFields    map[string]json.RawMessage `json:"custom_fields"`
		} `json:"attributes"`
		Relationships struct {
			WorkflowStatus *struct {
				Data *productiveRef `json:"data"`
			} `json:"workflow_status"`
			ParentTask *struct {
				Data *productiveRef `json:"data"`
			} `json:"parent_task"`
		} `json:"relationships"`
	} `json:"data"`
}

func (f *fakeProductive) handleCreate(w http.ResponseWriter, body []byte) {
	f.mu.Lock()
	if f.rateLimitFirstN > 0 {
		f.rateLimitFirstN--
		f.mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	f.mu.Unlock()

	var in incomingWrite
	if err := json.Unmarshal(body, &in); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	t := &fakeProductiveTask{
		ID:           id,
		Title:        in.Data.Attributes.Title,
		CustomFields: in.Data.Attributes.CustomFields,
	}
	if !isJSONNull(in.Data.Attributes.InitialEstimate) {
		if n, err := strconv.Atoi(strings.TrimSpace(string(in.Data.Attributes.InitialEstimate))); err == nil {
			t.InitialEstimate = intPtr(n)
		}
	}
	if in.Data.Relationships.WorkflowStatus != nil && in.Data.Relationships.WorkflowStatus.Data != nil {
		t.WorkflowStatusID = in.Data.Relationships.WorkflowStatus.Data.ID
	}
	if in.Data.Relationships.ParentTask != nil && in.Data.Relationships.ParentTask.Data != nil {
		t.ParentTaskID = in.Data.Relationships.ParentTask.Data.ID
	}
	f.tasks[id] = t
	hangup := f.postThenHangup
	f.mu.Unlock()

	if hangup {
		// The task IS created but the caller never learns its id. Retrying would
		// create a twin.
		panic(http.ErrAbortHandler)
	}

	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "type": "tasks"}})
}

func (f *fakeProductive) handleUpdate(w http.ResponseWriter, r *http.Request, body []byte) {
	id := strings.TrimPrefix(r.URL.Path, "/api/v2/tasks/")

	f.mu.Lock()
	if f.rateLimitFirstN > 0 {
		f.rateLimitFirstN--
		f.mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	t, ok := f.tasks[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var in incomingWrite
	if err := json.Unmarshal(body, &in); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	t.Title = in.Data.Attributes.Title
	// Productive replaces the WHOLE custom_fields hash. Reproducing that here is
	// what makes the merge test meaningful.
	if in.Data.Attributes.CustomFields != nil {
		t.CustomFields = in.Data.Attributes.CustomFields
	}
	switch {
	case len(in.Data.Attributes.InitialEstimate) == 0:
		// absent -> unchanged
	case isJSONNull(in.Data.Attributes.InitialEstimate):
		t.InitialEstimate = nil
	default:
		if n, err := strconv.Atoi(strings.TrimSpace(string(in.Data.Attributes.InitialEstimate))); err == nil {
			t.InitialEstimate = intPtr(n)
		}
	}
	if in.Data.Relationships.WorkflowStatus != nil && in.Data.Relationships.WorkflowStatus.Data != nil {
		t.WorkflowStatusID = in.Data.Relationships.WorkflowStatus.Data.ID
	}
	if in.Data.Relationships.ParentTask != nil && in.Data.Relationships.ParentTask.Data != nil {
		t.ParentTaskID = in.Data.Relationships.ParentTask.Data.ID
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/vnd.api+json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id, "type": "tasks"}})
}

func (f *fakeProductive) task(id string) *fakeProductiveTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[id]
}

func (f *fakeProductive) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tasks)
}

// findByClickUpID is how a test asserts "exactly one Productive task claims this
// ClickUp id" — the invariant a duplicate create would break.
func (f *fakeProductive) findByClickUpID(clickUpID string) []*fakeProductiveTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeProductiveTask
	for _, t := range f.tasks {
		if rawToString(t.CustomFields["242457"]) == clickUpID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func fmtCalls(calls []recordedCall) string {
	var sb strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&sb, "%s %s\n", c.Method, c.Path)
	}
	return sb.String()
}

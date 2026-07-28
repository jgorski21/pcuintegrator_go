package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func newProductiveTestClient(t *testing.T, cfg Config, readOnly bool) *apiClient {
	t.Helper()
	return newAPIClient("productive", cfg.ProductiveBaseURL, productiveHeader(cfg),
		cfg.ProductiveRPS, cfg.ProductiveBurstRPS, cfg, readOnly, testLogger())
}

func TestFetchProductiveIndexesByClickUpID(t *testing.T) {
	pr := newFakeProductive(t)
	pr.seed(seededTask("9001", "cu-1", "One", "backend, urgent"))
	pr.seed(seededTask("9002", "cu-2", "Two", ""))
	// A task with no ClickUp id is simply not part of the sync (parity).
	pr.seed(&fakeProductiveTask{ID: "9003", Title: "Manual", CustomFields: map[string]json.RawMessage{}})

	cfg := testConfig("http://x/", pr.baseURL())
	snap, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if len(snap.ByClickUpID) != 2 {
		t.Fatalf("indexed %d tasks, want 2", len(snap.ByClickUpID))
	}
	if snap.Stats.WithoutKey != 1 {
		t.Fatalf("without_clickup_id = %d, want 1", snap.Stats.WithoutKey)
	}
	one := snap.ByClickUpID["cu-1"]
	if one.ProductiveID != "9001" || !tagsEqual(one.Tags, []string{"backend", "urgent"}) {
		t.Fatalf("cu-1 = %+v", one)
	}
	// CHURN FIX #1 on the read side: an empty tags field must yield no tags.
	if two := snap.ByClickUpID["cu-2"]; len(two.Tags) != 0 {
		t.Fatalf("cu-2 tags = %#v, want none", two.Tags)
	}
	// The dry-run needs these names to answer "is 161083 actually called Closed?".
	if snap.Stats.WorkflowStatusNames["161083"] != "Closed" {
		t.Fatalf("workflow status names not reported: %v", snap.Stats.WorkflowStatusNames)
	}
}

func TestFetchProductiveStatusByName(t *testing.T) {
	pr := newFakeProductive(t)
	open := seededTask("9001", "cu-open", "Open one", "")
	done := seededTask("9002", "cu-done", "Done one", "")
	done.WorkflowStatusID = "161083"
	pr.seed(open)
	pr.seed(done)

	cfg := testConfig("http://x/", pr.baseURL())
	snap, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snap.ByClickUpID["cu-open"].Status != StatusOpen {
		t.Fatal("expected open")
	}
	if snap.ByClickUpID["cu-done"].Status != StatusDone {
		t.Fatal("expected done")
	}
}

// .NET indexes the status dictionary directly and throws KeyNotFoundException when
// a task's workflow_status is not in `included`, killing the run. We warn and
// default to Open.
func TestFetchProductiveToleratesMissingStatusInIncluded(t *testing.T) {
	pr := newFakeProductive(t)
	orphan := seededTask("9001", "cu-1", "One", "")
	orphan.WorkflowStatusID = "999999" // not in statusNames
	pr.seed(orphan)

	cfg := testConfig("http://x/", pr.baseURL())
	snap, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
	if err != nil {
		t.Fatalf("fetch must not fail: %v", err)
	}
	if snap.ByClickUpID["cu-1"].Status != StatusOpen {
		t.Fatal("unknown status must default to open")
	}
	if len(snap.Warnings) != 1 || !strings.Contains(snap.Warnings[0], "missing from `included`") {
		t.Fatalf("expected a warning, got %v", snap.Warnings)
	}
}

// .NET dereferences fetchedTasks.Included! on every page and NREs when a page
// carries none. The fake omits `included` on pages after the first.
func TestFetchProductiveToleratesPageWithoutIncluded(t *testing.T) {
	pr := newFakeProductive(t)
	for i := range 250 {
		id := strconv.Itoa(9000 + i)
		pr.seed(seededTask(id, "cu-"+strconv.Itoa(i), "t"+strconv.Itoa(i), ""))
	}

	cfg := testConfig("http://x/", pr.baseURL())
	snap, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(snap.ByClickUpID) != 250 {
		t.Fatalf("indexed %d, want 250", len(snap.ByClickUpID))
	}
	// Driven by meta.total_pages: 250 tasks at page[size]=200 is exactly 2 pages,
	// with no trailing empty request.
	if snap.Stats.Pages != 2 {
		t.Fatalf("pages = %d, want 2", snap.Stats.Pages)
	}
}

func TestFetchProductiveFallsBackToEmptyPageWhenMetaMissing(t *testing.T) {
	pr := newFakeProductive(t)
	pr.omitTotals = true
	pr.includedOnlyFirstPage = false
	pr.seed(seededTask("9001", "cu-1", "One", ""))

	cfg := testConfig("http://x/", pr.baseURL())
	snap, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(snap.ByClickUpID) != 1 || snap.Stats.Pages != 2 {
		t.Fatalf("indexed %d over %d pages", len(snap.ByClickUpID), snap.Stats.Pages)
	}
}

// THE most important test in this file. A read that fails "softly" — 401, 500, an
// HTML error page — decoded into a zero-valued struct looks exactly like an empty
// Productive, and would POST a duplicate of every ClickUp task, each stamped with
// the ClickUp id so it cannot be distinguished afterwards.
func TestFetchProductiveAbortsOnNon2xx(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, `{"errors":[{"status":"401","title":"Unauthorized"}]}`},
		{"server error", 500, `<html><body>500 Internal Server Error</body></html>`},
		{"forbidden", 403, `{"errors":[{"status":"403","title":"Forbidden","detail":"no access"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := newFakeProductive(t)
			pr.readFailStatus, pr.readFailBody = tc.status, tc.body

			cfg := testConfig("http://x/", pr.baseURL())
			cfg.MaxRetries = 0
			snap, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
			if err == nil {
				t.Fatal("non-2xx read MUST abort the run")
			}
			if len(snap.ByClickUpID) != 0 {
				t.Fatalf("no snapshot may be returned alongside an error, got %d entries", len(snap.ByClickUpID))
			}
		})
	}
}

// A task sliding across a page boundary while we page would be missing from the
// snapshot, look new, and get POSTed as a duplicate. meta.total_count catches it.
func TestFetchProductiveAbortsOnShortRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []any{map[string]any{
				"id": "9001", "type": "tasks",
				"attributes":    map[string]any{"title": "One", "custom_fields": map[string]any{"242457": "cu-1"}},
				"relationships": map[string]any{},
			}},
			"included": []any{},
			"meta":     map[string]any{"current_page": 1, "total_pages": 1, "total_count": 7, "page_size": 200},
		})
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig("http://x/", srv.URL+"/api/v2/")
	_, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
	if err == nil || !strings.Contains(err.Error(), "meta.total_count") {
		t.Fatalf("expected a short-read abort, got %v", err)
	}
}

// Two Productive tasks claiming one ClickUp id: .NET's ToDictionary throws and the
// sync crash-loops forever. A plain Go map assignment would silently keep one twin
// and let the other drift. Neither is acceptable — we refuse to write to either.
func TestFetchProductiveFlagsDuplicateClickUpIDs(t *testing.T) {
	pr := newFakeProductive(t)
	pr.seed(seededTask("9001", "cu-1", "Original", ""))
	pr.seed(seededTask("9002", "cu-1", "Accidental twin", ""))

	cfg := testConfig("http://x/", pr.baseURL())
	snap, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
	if err != nil {
		t.Fatalf("a duplicate must not fail the fetch: %v", err)
	}
	ids, flagged := snap.Conflicted["cu-1"]
	if !flagged {
		t.Fatal("duplicate not flagged")
	}
	if len(ids) != 2 || ids[0] != "9001" || ids[1] != "9002" {
		t.Fatalf("conflicted ids = %v, want both", ids)
	}
	if len(snap.Warnings) == 0 {
		t.Fatal("expected a warning naming both Productive ids")
	}
}

func TestFetchProductiveNonStringCustomFieldValues(t *testing.T) {
	pr := newFakeProductive(t)
	pr.seed(&fakeProductiveTask{
		ID:    "9001",
		Title: "One",
		CustomFields: map[string]json.RawMessage{
			"242457": json.RawMessage(`"cu-1"`),
			"242565": json.RawMessage(`"a, b"`),
			"1000":   json.RawMessage(`42`),        // number
			"1001":   json.RawMessage(`["x","y"]`), // multi-select
			"1002":   json.RawMessage(`null`),      // empty
		},
		WorkflowStatusID: "161082",
	})

	cfg := testConfig("http://x/", pr.baseURL())
	snap, err := fetchProductive(context.Background(), newProductiveTestClient(t, cfg, true), cfg)
	if err != nil {
		// map[string]string would fail the whole page here.
		t.Fatalf("polymorphic custom field values must decode: %v", err)
	}
	if snap.Stats.ExtraFields != 1 {
		t.Fatalf("extra custom fields not detected: %+v", snap.Stats)
	}
	if snap.Stats.ExtraCustomFieldIDs["1002"] != 0 {
		t.Fatal("a null-valued custom field must not count as extra")
	}
}

// --- buildBody ---

func goldenCompact(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact golden: %v", err)
	}
	return buf.Bytes()
}

func TestBuildBodyCreateGolden(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	task := Task{
		ClickUpID:       "cu-1",
		Title:           "Parent task",
		Status:          StatusOpen,
		Tags:            normalizeTags([]string{"urgent", "backend"}),
		InitialEstimate: intPtr(90),
	}

	got, err := buildBody(task, "9100", nil, cfg)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if want := goldenCompact(t, "want_create.json"); !bytes.Equal(got, want) {
		t.Fatalf("body mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

// Productive treats custom_fields as ONE attribute: a PATCH carrying only the two
// managed keys sets the whole hash to those two keys and loses everything else.
// The list fetch already returned the full hash, so merging costs no request.
func TestBuildBodyUpdateMergesCustomFieldsGolden(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	existing := existingTask{
		Task:         Task{ClickUpID: "cu-9", Title: "Old title", Status: StatusOpen},
		ProductiveID: "9001",
		RawCustomFields: map[string]json.RawMessage{
			"242457": json.RawMessage(`"cu-9"`),
			"242565": json.RawMessage(`"stale, tags"`),
			"999":    json.RawMessage(`"keep"`),
			"1000":   json.RawMessage(`42`),
			"1001":   json.RawMessage(`null`), // read-only / empty: must not be echoed
		},
	}
	task := Task{ClickUpID: "cu-9", Title: "Updated title", Status: StatusDone}

	got, err := buildBody(task, "", &existing, cfg)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if want := goldenCompact(t, "want_patch.json"); !bytes.Equal(got, want) {
		t.Fatalf("body mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func TestBuildBodyUpdateWithoutMergeDropsOtherFields(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	cfg.MergeCustomFields = false
	existing := existingTask{
		Task:         Task{ClickUpID: "cu-9"},
		ProductiveID: "9001",
		RawCustomFields: map[string]json.RawMessage{
			"242457": json.RawMessage(`"cu-9"`),
			"999":    json.RawMessage(`"lost"`),
		},
	}

	got, err := buildBody(Task{ClickUpID: "cu-9", Title: "T"}, "", &existing, cfg)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if bytes.Contains(got, []byte(`"999"`)) {
		t.Fatal("MERGE_CUSTOM_FIELDS=false must reproduce the destructive .NET behaviour exactly")
	}
}

// PATCH never carries parent_task: parenting is deliberately outside change
// detection, so re-parenting cannot undo a move somebody made on purpose.
func TestBuildBodyUpdateOmitsParentTask(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	existing := existingTask{Task: Task{ClickUpID: "cu-2"}, ProductiveID: "9002", ParentTaskID: ""}

	got, err := buildBody(Task{ClickUpID: "cu-2", Title: "T"}, "9001", &existing, cfg)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if bytes.Contains(got, []byte("parent_task")) {
		t.Fatalf("PATCH must not set parent_task: %s", got)
	}

	cfg.AllowReparent = true
	got, err = buildBody(Task{ClickUpID: "cu-2", Title: "T"}, "9001", &existing, cfg)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	if !bytes.Contains(got, []byte(`"parent_task":{"data":{"type":"tasks","id":"9001"}}`)) {
		t.Fatalf("ALLOW_REPARENT=true should set parent_task: %s", got)
	}
}

// .NET emits remaining_time / start_date / due_date as explicit nulls in every
// write, so every PATCH it makes clears the Productive task's start and due dates.
// Not reproduced.
func TestBuildBodyNeverClearsDates(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	got, err := buildBody(Task{ClickUpID: "cu-1", Title: "T"}, "", nil, cfg)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	for _, forbidden := range []string{"remaining_time", "start_date", "due_date"} {
		if bytes.Contains(got, []byte(forbidden)) {
			t.Fatalf("%s must not appear in the body: %s", forbidden, got)
		}
	}
}

func TestBuildBodyEstimate(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")

	t.Run("omitted when nil", func(t *testing.T) {
		got, _ := buildBody(Task{ClickUpID: "a", Title: "T"}, "", nil, cfg)
		if bytes.Contains(got, []byte("initial_estimate")) {
			t.Fatalf("must be omitted, not null: %s", got)
		}
	})

	t.Run("zero is sent, not dropped", func(t *testing.T) {
		got, _ := buildBody(Task{ClickUpID: "a", Title: "T", InitialEstimate: intPtr(0)}, "", nil, cfg)
		if !bytes.Contains(got, []byte(`"initial_estimate":0`)) {
			t.Fatalf("0 is a reachable value (a 30s ClickUp estimate) and must survive: %s", got)
		}
	})

	t.Run("explicit null only in clear mode", func(t *testing.T) {
		clearCfg := cfg
		clearCfg.EstimateMode = EstimateClearNil
		existing := existingTask{Task: Task{InitialEstimate: intPtr(480)}, ProductiveID: "9001"}

		got, _ := buildBody(Task{ClickUpID: "a", Title: "T"}, "", &existing, clearCfg)
		if !bytes.Contains(got, []byte(`"initial_estimate":null`)) {
			t.Fatalf("clear mode must send an explicit null: %s", got)
		}

		got, _ = buildBody(Task{ClickUpID: "a", Title: "T"}, "", &existing, cfg)
		if bytes.Contains(got, []byte("initial_estimate")) {
			t.Fatalf("ignore mode must omit the field: %s", got)
		}
	})
}

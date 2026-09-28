package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	cuMainProjectID  = "01997a3c-5e1f-7c2a-9b3d-000000000001" // linked to CLICKUP_LIST_ID
	cuOtherProjectID = "01997a3c-5e1f-7c2a-9b3d-000000000002" // another ClickUp list
	prProjectID      = "01997a3c-5e1f-7c2a-9b3d-000000000003" // a Productive task list
	unknownProjectID = "01997a3c-5e1f-7c2a-9b3d-000000000004" // a source this build does not know
)

var (
	cuMainProject  = linkedProjectJSON(cuMainProjectID, "Portal klienta", "ACME", 1, "900501332334")
	cuOtherProject = linkedProjectJSON(cuOtherProjectID, "Aplikacja mobilna", "Beta", 1, "901234567")
	prProject      = linkedProjectJSON(prProjectID, "Wdrożenie", "Gamma", 2, "4815162")
	unknownProject = linkedProjectJSON(unknownProjectID, "Nowe źródło", "Delta", 3, "77")
)

func subtaskJSON(id, name, parent string) map[string]any {
	m := clickUpTaskJSON(id, name, "open")
	m["parent"] = parent
	return m
}

func postedRequest(t *testing.T, rec ditRecorded) ditSyncRequest {
	t.Helper()
	var req ditSyncRequest
	if err := json.Unmarshal([]byte(rec.Body), &req); err != nil {
		t.Fatalf("POST body: %v\n%s", err, rec.Body)
	}
	return req
}

func projectResult(t *testing.T, res *Result, id string) deliverITProjectResult {
	t.Helper()
	for _, p := range res.DeliverIT.ProjectResults {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no project result for %s: %+v", id, res.DeliverIT.ProjectResults)
	return deliverITProjectResult{}
}

func hasSubstring(list []string, fragment string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, fragment) })
}

// The list the ClickUp -> Productive stage already read is NOT read a second time.
// DeliverIT gets the FULL title (Productive only accepts 140 characters) and, with
// the same filters as the main stage, subtasks too.
func TestFanoutReusesTheClickUpTasksAlreadyFetched(t *testing.T) {
	long := "Przygotować " + strings.Repeat("bardzo długi opis zadania ", 6) // > 140 runes
	cu := newFakeClickUp(t, []map[string]any{
		clickUpTaskJSON("86c0abc12", "  "+long+"  ", "open"),
		subtaskJSON("86c0abc13", "Testy", "86c0abc12"),
	})
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	if res.Created != 2 || res.Aborted != "" {
		t.Fatalf("the Productive stage must be unaffected: created=%d aborted=%q errors=%v", res.Created, res.Aborted, res.Errors)
	}
	if got := len(cu.recorded()); got != 2 {
		t.Fatalf("ClickUp saw %d requests; the main stage needs 2 and the fanout must add none:\n%s", got, fmtCalls(cu.recorded()))
	}
	posts := dit.posts()
	if len(posts) != 1 || posts[0].Path != "/api/integrations/projects/"+cuMainProjectID+"/tasks/sync" {
		t.Fatalf("posts = %+v", posts)
	}
	body, _ := json.Marshal(map[string]any{
		"taskList": map[string]any{"source": 1, "externalId": "900501332334"},
		"tasks": []map[string]string{
			{"externalId": "86c0abc12", "name": strings.TrimSpace(long)},
			{"externalId": "86c0abc13", "name": "Testy"},
		},
	})
	assertSameJSON(t, posts[0].Body, string(body))

	d := res.DeliverIT
	if !d.Enabled || d.Projects != 1 || d.SyncedProjects != 1 || d.Created != 2 || d.Tasks != 2 || d.Batches != 1 || len(d.Errors) != 0 {
		t.Fatalf("deliverit = %+v", d)
	}
	if p := projectResult(t, res, cuMainProjectID); p.Status != "synced" || p.Source != "clickup" || p.ListID != "900501332334" {
		t.Fatalf("project = %+v", p)
	}
	if d.API.Requests != 2 {
		t.Fatalf("DeliverIT api stats = %+v", d.API)
	}
}

func TestFanoutReadsAnotherClickUpListWithTheSameFilters(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": {
		clickUpTaskJSON("86c0abc12", "Analiza wymagań", "open"),
		clickUpTaskJSON("86c0abc13", "Integracja z API płatności", "open"),
	}}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuOtherProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	var other []string
	for _, c := range cu.recorded() {
		if strings.Contains(c.Path, "/list/901234567/") {
			other = append(other, c.Path)
		}
	}
	if len(other) != 2 || other[0] != "/api/v2/list/901234567/task?page=0&subtasks=true" {
		t.Fatalf("the other list must be read with the main stage's filters: %v", other)
	}
	posts := dit.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d, errors = %v", len(posts), res.DeliverIT.Errors)
	}
	assertSameJSON(t, posts[0].Body, ditSyncRequestJSON)
}

func TestFanoutReadsAProductiveTaskList(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	pr := newFakeProductive(t)
	pr.otherLists = map[string][]*fakeProductiveTask{"4815162": {
		{ID: "5001", Title: "Backlog", CustomFields: map[string]json.RawMessage{}, WorkflowStatusID: "161082"},
		{ID: "5002", Title: "Konfiguracja środowiska", CustomFields: map[string]json.RawMessage{}, WorkflowStatusID: "161082"},
	}}
	dit := newFakeDeliverIT(t, prProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	var reads []string
	for _, c := range pr.recorded() {
		if strings.Contains(c.Path, "filter[task_list_id]=4815162") {
			reads = append(reads, c.Path)
		}
	}
	if len(reads) != 1 || strings.Contains(reads[0], "include=") {
		t.Fatalf("the fanout reads id+title only, without include: %v", reads)
	}
	posts := dit.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d, errors = %v", len(posts), res.DeliverIT.Errors)
	}
	assertSameJSON(t, posts[0].Body, `{"taskList":{"source":2,"externalId":"4815162"},"tasks":[`+
		`{"externalId":"5001","name":"Backlog"},{"externalId":"5002","name":"Konfiguracja środowiska"}]}`)
	if p := projectResult(t, res, prProjectID); p.Status != "synced" || p.Source != "productive" || p.Created != 2 {
		t.Fatalf("project = %+v", p)
	}
}

func TestFanoutSkipsUnknownSourcesAndSourcesWithoutAToken(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, unknownProject, prProject, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	cfg.ProductiveToken = "" // the main stage's fake does not check it

	res := runSync(t, cfg, RunOptions{})

	d := res.DeliverIT
	if d.Projects != 3 || d.SkippedProjects != 2 || d.SyncedProjects != 1 || len(d.Errors) != 0 {
		t.Fatalf("deliverit = %+v", d)
	}
	if p := projectResult(t, res, unknownProjectID); p.Status != "skipped" || p.Reason != "unknown_source" || p.Source != "unknown(3)" {
		t.Fatalf("unknown source = %+v", p)
	}
	if p := projectResult(t, res, prProjectID); p.Status != "skipped" || p.Reason != "missing_source_token" {
		t.Fatalf("missing token = %+v", p)
	}
	if !hasSubstring(d.Warnings, "Nowe źródło") || !hasSubstring(d.Warnings, "PRODUCTIVE_TOKEN") {
		t.Fatalf("skips must be warnings naming the project/token: %v", d.Warnings)
	}
	if posts := dit.posts(); len(posts) != 1 || !strings.Contains(posts[0].Path, cuMainProjectID) {
		t.Fatalf("only the ClickUp project may be sent: %+v", posts)
	}
}

func TestFanoutFiltersTasksBreakingServerRules(t *testing.T) {
	longName := strings.Repeat("a", 1200)
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": {
		clickUpTaskJSON("ok1", "  Dobre  ", "open"),
		clickUpTaskJSON("bad id", "Zły identyfikator", "open"),
		clickUpTaskJSON("ok2", "   ", "open"),
		clickUpTaskJSON("ok3", longName, "open"),
		clickUpTaskJSON("ok1", "Nowsza wersja", "open"), // duplicate: the last read wins, at the first position
	}}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuOtherProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	req := postedRequest(t, dit.posts()[0])
	want := []ditSyncTask{{ExternalID: "ok1", Name: "Nowsza wersja"}, {ExternalID: "ok3", Name: longName[:deliverITMaxNameRunes]}}
	if !slices.Equal(req.Tasks, want) {
		t.Fatalf("sent %d tasks: %.200v", len(req.Tasks), req.Tasks)
	}
	d := res.DeliverIT
	if d.Skipped != 2 || d.SkippedReasons["invalid_external_id"] != 1 || d.SkippedReasons["empty_name"] != 1 {
		t.Fatalf("skipped=%d reasons=%v", d.Skipped, d.SkippedReasons)
	}
	for _, fragment := range []string{`"bad id"`, "ok2", "duplicate"} {
		if !hasSubstring(d.Warnings, fragment) {
			t.Fatalf("no warning mentioning %s: %v", fragment, d.Warnings)
		}
	}
}

// PostgreSQL text cannot hold U+0000, so DeliverIT would answer the WHOLE batch
// with a 500. NUL is removed before sending; a name left empty is dropped with a
// warning, like any other empty name.
func TestFanoutStripsNULFromTaskNames(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": {
		clickUpTaskJSON("86c0abc12", "Ana\x00liza wymagań\x00", "open"),
		clickUpTaskJSON("86c0abc13", "\x00 \x00", "open"),
		clickUpTaskJSON("86c0abc14", "Testy", "open"),
	}}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuOtherProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	posts := dit.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d, errors = %v", len(posts), res.DeliverIT.Errors)
	}
	if strings.Contains(posts[0].Body, `\u0000`) {
		t.Fatalf("NUL was sent: %s", posts[0].Body)
	}
	want := []ditSyncTask{{ExternalID: "86c0abc12", Name: "Analiza wymagań"}, {ExternalID: "86c0abc14", Name: "Testy"}}
	if got := postedRequest(t, posts[0]).Tasks; !slices.Equal(got, want) {
		t.Fatalf("sent %+v, want %+v", got, want)
	}
	d := res.DeliverIT
	if d.Skipped != 1 || d.SkippedReasons["empty_name"] != 1 || len(d.Errors) != 0 {
		t.Fatalf("skipped=%d reasons=%v errors=%v", d.Skipped, d.SkippedReasons, d.Errors)
	}
	if !hasSubstring(d.Warnings, "task 86c0abc13 skipped: empty name in the source after removing NUL characters") {
		t.Fatalf("the dropped task must be a warning that says why: %v", d.Warnings)
	}
}

func TestFanoutSendsBatchesOf200(t *testing.T) {
	var tasks []map[string]any
	for i := range 450 {
		tasks = append(tasks, clickUpTaskJSON("t"+strconv.Itoa(i), "Zadanie "+strconv.Itoa(i), "open"))
	}
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": tasks}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuOtherProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	var sizes []int
	for _, p := range dit.posts() {
		req := postedRequest(t, p)
		sizes = append(sizes, len(req.Tasks))
		if req.TaskList != (ditTaskList{Source: taskSourceClickUp, ExternalID: "901234567"}) {
			t.Fatalf("every batch carries the link: %+v", req.TaskList)
		}
	}
	if !slices.Equal(sizes, []int{200, 200, 50}) {
		t.Fatalf("batches %v", sizes)
	}
	if d := res.DeliverIT; d.Tasks != 450 || d.Batches != 3 || d.Created != 450 {
		t.Fatalf("deliverit = %+v", d)
	}
}

// An empty list is information too: DeliverIT marks the project as synced.
func TestFanoutEmptyListSendsOneEmptyBatch(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": {}}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuOtherProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	runSync(t, cfg, RunOptions{})

	posts := dit.posts()
	if len(posts) != 1 {
		t.Fatalf("want exactly one batch, got %d", len(posts))
	}
	assertSameJSON(t, posts[0].Body, `{"taskList":{"source":1,"externalId":"901234567"},"tasks":[]}`)
}

func TestFanoutCountsTasksTheServerSkipped(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": {
		clickUpTaskJSON("86c0abc12", "Analiza wymagań", "open"),
		clickUpTaskJSON("86c0abc13", "Integracja z API płatności", "open"),
	}}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuOtherProject)
	dit.onSync = func(_ int, _ string, _ ditSyncRequest, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":0,"renamed":0,"unchanged":0,"skipped":[`+
			`{"externalId":"86c0abc12","name":"Analiza wymagań","reason":0},`+
			`{"externalId":"86c0abc13","name":"Integracja z API płatności","reason":1}]}`)
	}
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	d := res.DeliverIT
	if d.Skipped != 2 || d.SkippedReasons["name_taken"] != 1 || d.SkippedReasons["external_id_in_other_project"] != 1 {
		t.Fatalf("skipped=%d reasons=%v", d.Skipped, d.SkippedReasons)
	}
	if !hasSubstring(d.Warnings, "86c0abc12") || !hasSubstring(d.Warnings, "name_taken") {
		t.Fatalf("a server-side skip must be a warning on every run: %v", d.Warnings)
	}
	if len(d.Errors) != 0 {
		t.Fatalf("a skip is not an error: %v", d.Errors)
	}
}

// The fanout only needs the source's tasks, so it runs even when the Productive
// read aborted the main stage (the ClickUp list is then read by the fanout itself).
func TestFanoutRunsEvenWhenTheProductiveReadAborted(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{clickUpTaskJSON("86c0abc12", "Analiza wymagań", "open")})
	pr := newFakeProductive(t)
	pr.readFailStatus = 500
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	cfg.MaxRetries = 0

	res := runSync(t, cfg, RunOptions{})

	if res.Aborted != "fetch_productive" {
		t.Fatalf("aborted = %q", res.Aborted)
	}
	if len(pr.writes()) != 0 {
		t.Fatal("nothing may be written to Productive after a failed read")
	}
	if d := res.DeliverIT; d.SyncedProjects != 1 || d.Created != 1 || len(dit.posts()) != 1 {
		t.Fatalf("the fanout must still run: %+v", d)
	}
}

// A list whose read already failed in this run is reported, not hammered again.
func TestFanoutDoesNotReReadAClickUpListThatFailedInThisRun(t *testing.T) {
	cu := newFakeClickUp(t, nil)
	cu.failWith, cu.failBody = 500, `{"err":"boom"}`
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	cfg.MaxRetries = 0

	res := runSync(t, cfg, RunOptions{})

	if res.Aborted != "fetch_clickup" {
		t.Fatalf("aborted = %q", res.Aborted)
	}
	if got := len(cu.recorded()); got != 1 {
		t.Fatalf("ClickUp saw %d requests, want 1 (no second read)", got)
	}
	p := projectResult(t, res, cuMainProjectID)
	if p.Status != "failed" || res.DeliverIT.FailedProjects != 1 || !hasSubstring(res.DeliverIT.Errors, "not re-read") {
		t.Fatalf("project = %+v errors = %v", p, res.DeliverIT.Errors)
	}
	if len(dit.posts()) != 0 {
		t.Fatal("a project whose source could not be read must not be sent")
	}
}

func TestFanoutDryRunOnlyCounts(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{
		clickUpTaskJSON("86c0abc12", "Analiza wymagań", "open"),
		clickUpTaskJSON("86c0abc13", "Integracja z API płatności", "open"),
	})
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{DryRun: true})

	if len(dit.posts()) != 0 || len(pr.writes()) != 0 {
		t.Fatalf("a dry run wrote: deliverit %d, productive %d", len(dit.posts()), len(pr.writes()))
	}
	if got := dit.recorded(); len(got) != 1 || got[0].Path != "/api/integrations/projects" {
		t.Fatalf("a dry run still reads the project list: %+v", got)
	}
	d := res.DeliverIT
	if d.Tasks != 2 || d.Batches != 1 || d.Created != 0 || d.SyncedProjects != 0 {
		t.Fatalf("deliverit = %+v", d)
	}
	if p := projectResult(t, res, cuMainProjectID); p.Status != "planned" || p.Tasks != 2 || len(p.Plan) != 0 {
		t.Fatalf("dry run without explain = counts only: %+v", p)
	}
}

// A wrongly linked list creates tasks in the wrong project, and the app cannot undo
// that. The dry run therefore shows each DeliverIT project next to the NAME of the
// source list, so a human can check the pairing before the first real run.
func TestFanoutDryRunShowsTheSourceListName(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.listNames = map[string]string{"900501332334": "Portal — sprint 12"}
	pr := newFakeProductive(t)
	pr.otherLists = map[string][]*fakeProductiveTask{"4815162": {}}
	pr.taskListNames = map[string]string{"4815162": "Backlog wdrożenia"}
	dit := newFakeDeliverIT(t, cuMainProject, prProject, cuOtherProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	cfg.MaxRetries = 0

	res := runSync(t, cfg, RunOptions{DryRun: true})

	if got := projectResult(t, res, cuMainProjectID).ListName; got != "Portal — sprint 12" {
		t.Fatalf("clickup list name = %q", got)
	}
	if got := projectResult(t, res, prProjectID).ListName; got != "Backlog wdrożenia" {
		t.Fatalf("productive list name = %q", got)
	}
	// An unreadable name is a warning, never a reason to skip the project.
	other := projectResult(t, res, cuOtherProjectID)
	if other.ListName != "?" || other.Status != "planned" || !hasSubstring(res.DeliverIT.Warnings, "list name") {
		t.Fatalf("other = %+v warnings = %v", other, res.DeliverIT.Warnings)
	}

	// A real run does not pay for the extra reads.
	cu2 := newFakeClickUp(t, clickUpTasksFor(1))
	pr2 := newFakeProductive(t)
	dit2 := newFakeDeliverIT(t, cuMainProject)
	res = runSync(t, withDeliverIT(testConfig(cu2.baseURL(), pr2.baseURL()), dit2), RunOptions{})
	for _, c := range cu2.recorded() {
		if !strings.Contains(c.Path, "/task?") {
			t.Fatalf("a real run read %s", c.Path)
		}
	}
	if got := projectResult(t, res, cuMainProjectID).ListName; got != "" {
		t.Fatalf("list name outside a dry run = %q", got)
	}
}

func TestFanoutExplainAttachesTheBatchPlanWithoutTheKey(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": {
		clickUpTaskJSON("86c0abc12", "Analiza wymagań", "open"),
		clickUpTaskJSON("86c0abc13", "Integracja z API płatności", "open"),
	}}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuOtherProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{DryRun: true, Explain: true})

	p := projectResult(t, res, cuOtherProjectID)
	if len(p.Plan) != 1 || p.Plan[0].Tasks != 2 {
		t.Fatalf("plan = %+v", p.Plan)
	}
	assertSameJSON(t, string(p.Plan[0].Body), ditSyncRequestJSON)
	all := mustJSON(t, res)
	if strings.Contains(all, testDeliverITKey) {
		t.Fatal("the result JSON contains the API key")
	}
	if !strings.Contains(all, `"api_key_prefix":"dit_Ab3dE5gH…"`) {
		t.Fatalf("the result should name the key by its prefix only:\n%.600s", all)
	}
}

// A DeliverIT failure is contained to its project and never touches the Productive
// stage.
func TestFanoutBatchFailureIsContainedToItsProject(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(2))
	cu.otherLists = map[string][]map[string]any{"901234567": {clickUpTaskJSON("a1", "A1", "open")}}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject, cuOtherProject)
	dit.onSync = func(_ int, projectID string, req ditSyncRequest, w http.ResponseWriter, _ *http.Request) {
		if projectID == cuMainProjectID {
			writeProblem(w, http.StatusConflict, ditMismatchJSON)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"renamed":0,"unchanged":0,"skipped":[]}`)
	}
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	if res.Created != 2 || res.Failed != 0 || res.Aborted != "" {
		t.Fatalf("Productive stage: created=%d failed=%d aborted=%q", res.Created, res.Failed, res.Aborted)
	}
	d := res.DeliverIT
	if d.FailedProjects != 1 || d.SyncedProjects != 1 || !hasSubstring(d.Errors, "PROJECT_TASK_LIST_MISMATCH") ||
		!hasSubstring(d.Errors, "Portal klienta") {
		t.Fatalf("deliverit = %+v", d)
	}
}

// A bug (panic) in the fanout must not kill the process: stage 1 has already
// written, and a dead process would lose the whole summary and leave the DO lease
// hanging. Here a nil ClickUp client makes reading another list panic.
func TestFanoutPanicBecomesAnErrorOfTheStage(t *testing.T) {
	dit := newFakeDeliverIT(t, cuOtherProject, prProject)
	cfg := withDeliverIT(testConfig("http://x/", "http://y/"), dit)

	res := runDeliverIT(context.Background(), cfg, RunOptions{}, nil, nil, clickUpRead{}, testLogger())

	if !res.failed() || !hasSubstring(res.Errors, "internal error") || len(dit.posts()) != 0 {
		t.Fatalf("deliverit = %+v, posts = %d", res, len(dit.posts()))
	}
	if p := res.ProjectResults; len(p) != 1 || p[0].ID != cuOtherProjectID || p[0].Status != "failed" ||
		p[0].Reason != "internal_error" || res.FailedProjects != 1 {
		t.Fatalf("project results = %+v, failed = %d", p, res.FailedProjects)
	}
	if res.API.Requests != 1 {
		t.Fatalf("api stats survive the panic: %+v", res.API)
	}
}

// Batches accepted before a failing one are already in DeliverIT's database, so
// they count; the remaining batches of that project are not sent.
func TestFanoutCountsBatchesAcceptedBeforeAFailure(t *testing.T) {
	var tasks []map[string]any
	for i := range 450 {
		tasks = append(tasks, clickUpTaskJSON("t"+strconv.Itoa(i), "Zadanie "+strconv.Itoa(i), "open"))
	}
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": tasks}
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuOtherProject)
	dit.onSync = func(n int, _ string, req ditSyncRequest, w http.ResponseWriter, _ *http.Request) {
		if n == 2 {
			writeProblem(w, http.StatusConflict, ditMismatchJSON)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":`+strconv.Itoa(len(req.Tasks))+`,"renamed":0,"unchanged":0,"skipped":[]}`)
	}
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	if got := len(dit.posts()); got != 2 {
		t.Fatalf("posts = %d, want 2 (the third batch is not sent after the second fails)", got)
	}
	d := res.DeliverIT
	if d.Created != 200 || d.FailedProjects != 1 || projectResult(t, res, cuOtherProjectID).Created != 200 {
		t.Fatalf("deliverit = %+v", d)
	}
	if !hasSubstring(d.Errors, "batch 2 of 3") {
		t.Fatalf("errors = %v", d.Errors)
	}
}

// A rejected key makes every further request pointless: the stage stops.
func TestFanoutRejectedKeyStopsTheStage(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	cu.otherLists = map[string][]map[string]any{"901234567": {clickUpTaskJSON("a1", "A1", "open")}}
	pr := newFakeProductive(t)
	pr.otherLists = map[string][]*fakeProductiveTask{"4815162": {}}
	dit := newFakeDeliverIT(t, cuOtherProject, prProject)
	dit.onSync = func(_ int, _ string, _ ditSyncRequest, w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusUnauthorized, ditAPIKeyInvalidJSON)
	}
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)

	res := runSync(t, cfg, RunOptions{})

	if len(dit.posts()) != 1 {
		t.Fatalf("after a rejected key no further batch may be sent: %d", len(dit.posts()))
	}
	for _, c := range pr.recorded() {
		if strings.Contains(c.Path, "4815162") {
			t.Fatal("the next project's source must not even be read")
		}
	}
	d := res.DeliverIT
	if !hasSubstring(d.Errors, "API_KEY_INVALID") || !hasSubstring(d.Errors, "/integracje") {
		t.Fatalf("errors = %v", d.Errors)
	}
	if p := projectResult(t, res, prProjectID); p.Status != "skipped" || p.Reason != "stage_stopped" {
		t.Fatalf("remaining project = %+v", p)
	}
}

func TestFanoutRejectedKeyOnTheProjectList(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	cfg.DeliverITAPIKey = "dit_revokedKeyXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"

	res := runSync(t, cfg, RunOptions{})

	if len(dit.posts()) != 0 || !hasSubstring(res.DeliverIT.Errors, "API_KEY_INVALID") {
		t.Fatalf("deliverit = %+v", res.DeliverIT)
	}
	if hasSubstring(res.DeliverIT.Errors, cfg.DeliverITAPIKey) {
		t.Fatal("the key leaked into an error")
	}
}

func TestFanoutIsOffWithoutABaseURL(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := testConfig(cu.baseURL(), pr.baseURL())
	cfg.DeliverITAPIKey = testDeliverITKey

	res := runSync(t, cfg, RunOptions{})

	d := res.DeliverIT
	if d.Enabled || d.DisabledReason == "" || len(d.Warnings) != 0 || len(d.Errors) != 0 || len(dit.recorded()) != 0 {
		t.Fatalf("deliverit = %+v, requests = %d", d, len(dit.recorded()))
	}
}

func TestFanoutWithoutAKeyIsOffWithAWarning(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	cfg.DeliverITAPIKey = ""

	res := runSync(t, cfg, RunOptions{})

	d := res.DeliverIT
	if d.Enabled || !hasSubstring(d.Warnings, "DELIVERIT_API_KEY") || len(d.Errors) != 0 || len(dit.recorded()) != 0 {
		t.Fatalf("deliverit = %+v", d)
	}
	if res.Created != 1 {
		t.Fatalf("the Productive stage must run regardless: created=%d", res.Created)
	}
}

func TestFanoutWithAMalformedKeyIsAnError(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	cfg.DeliverITAPIKey = "Bearer " + testDeliverITKey // a classic copy-paste slip

	res := runSync(t, cfg, RunOptions{})

	d := res.DeliverIT
	if d.Enabled || !hasSubstring(d.Errors, "malformed") || len(dit.recorded()) != 0 {
		t.Fatalf("deliverit = %+v", d)
	}
	if strings.Contains(mustJSON(t, res), testDeliverITKey) {
		t.Fatal("the malformed key leaked into the result")
	}
}

func TestFanoutIsSkippedWhenTooLittleTimeIsLeft(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res := Run(ctx, cfg, RunOptions{}, testLogger())

	if res.Created != 1 {
		t.Fatalf("Productive stage: created=%d errors=%v", res.Created, res.Errors)
	}
	d := res.DeliverIT
	if !d.Enabled || !hasSubstring(d.Warnings, "SYNC_TIMEOUT") || len(d.Errors) != 0 || len(dit.recorded()) != 0 {
		t.Fatalf("deliverit = %+v, requests = %d", d, len(dit.recorded()))
	}
}

// Nothing the fanout produces — logs at debug level, the result JSON — may carry
// the key, even when a misbehaving proxy echoes the request headers back.
func TestFanoutNeverLeaksTheKey(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(1))
	pr := newFakeProductive(t)
	dit := newFakeDeliverIT(t, cuMainProject)
	dit.onSync = func(_ int, _ string, _ ditSyncRequest, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<pre>"+r.Header.Get("Authorization")+"</pre>")
	}
	cfg := withDeliverIT(testConfig(cu.baseURL(), pr.baseURL()), dit)
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	res := Run(context.Background(), cfg, RunOptions{Explain: true}, log)

	if len(res.DeliverIT.Errors) == 0 {
		t.Fatalf("the echoed 400 must be an error: %+v", res.DeliverIT)
	}
	if strings.Contains(logs.String(), testDeliverITKey) {
		t.Fatalf("the key is in the logs:\n%s", logs.String())
	}
	if strings.Contains(mustJSON(t, res), testDeliverITKey) {
		t.Fatal("the key is in the result")
	}
	if !strings.Contains(logs.String(), "dit_Ab3dE5gH…") {
		t.Fatalf("the log should identify the key by its prefix:\n%s", logs.String())
	}
}

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// --- Plan() : pure, no I/O ---

func planFor(t *testing.T, clickUp []Task, snap productiveSnapshot, cfg Config) plan {
	t.Helper()
	if snap.ByClickUpID == nil {
		snap.ByClickUpID = map[string]existingTask{}
	}
	if snap.Conflicted == nil {
		snap.Conflicted = map[string][]string{}
	}
	return Plan(clickUp, snap, cfg)
}

func TestPlanCreateUpdateSkip(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")

	snap := productiveSnapshot{
		ByClickUpID: map[string]existingTask{
			"cu-same": {Task: Task{ClickUpID: "cu-same", Title: "Same"}, ProductiveID: "9001"},
			"cu-diff": {Task: Task{ClickUpID: "cu-diff", Title: "Old"}, ProductiveID: "9002"},
		},
		Conflicted: map[string][]string{},
	}
	clickUp := []Task{
		{ClickUpID: "cu-same", Title: "Same"},
		{ClickUpID: "cu-diff", Title: "New"},
		{ClickUpID: "cu-new", Title: "Fresh"},
	}

	p := planFor(t, clickUp, snap, cfg)
	if p.Creates != 1 || p.Updates != 1 || p.Skips != 1 {
		t.Fatalf("create=%d update=%d skip=%d", p.Creates, p.Updates, p.Skips)
	}
	if p.Reasons["new"] != 1 || p.Reasons["title"] != 1 {
		t.Fatalf("reasons = %v", p.Reasons)
	}
}

// Tasks without a ClickUp id are not part of the sync (parity).
func TestPlanDropsTasksWithoutID(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	p := planFor(t, []Task{{Title: "no id"}, {ClickUpID: "cu-1", Title: "ok"}}, productiveSnapshot{}, cfg)
	if len(p.Actions) != 1 || p.Actions[0].Task.ClickUpID != "cu-1" {
		t.Fatalf("actions = %+v", p.Actions)
	}
}

func TestPlanDedupeKeepsFirst(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	p := planFor(t, []Task{
		{ClickUpID: "cu-1", Title: "first"},
		{ClickUpID: "cu-1", Title: "second"},
	}, productiveSnapshot{}, cfg)

	if p.DedupeDropped != 1 || len(p.Actions) != 1 {
		t.Fatalf("dropped=%d actions=%d", p.DedupeDropped, len(p.Actions))
	}
	if p.Actions[0].Task.Title != "first" {
		t.Fatalf("kept %q, want the first occurrence", p.Actions[0].Task.Title)
	}
}

func TestPlanOrdersParentsBeforeChildren(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	// This test is about ORDERING only; the depth cap is covered separately by
	// TestPlanFlattensTooDeepSubtasks. Raise it so the two concerns stay independent.
	cfg.MaxSubtaskDepth = 10

	// Deliberately inverted input: grandchild, child, root.
	clickUp := []Task{
		{ClickUpID: "cu-3", Title: "grandchild", ClickUpParentID: "cu-2"},
		{ClickUpID: "cu-2", Title: "child", ClickUpParentID: "cu-1"},
		{ClickUpID: "cu-1", Title: "root"},
	}
	p := planFor(t, clickUp, productiveSnapshot{}, cfg)

	var order []string
	for _, a := range p.Actions {
		order = append(order, a.Task.ClickUpID)
	}
	if strings.Join(order, ",") != "cu-1,cu-2,cu-3" {
		t.Fatalf("order = %v, want root, child, grandchild", order)
	}
	if p.Actions[1].ParentClickUpID != "cu-1" || p.Actions[2].ParentClickUpID != "cu-2" {
		t.Fatalf("pending parents not linked: %+v", p.Actions)
	}
}

func TestPlanParentAlreadyInProductive(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	snap := productiveSnapshot{
		ByClickUpID: map[string]existingTask{
			"cu-1": {Task: Task{ClickUpID: "cu-1", Title: "root"}, ProductiveID: "9001"},
		},
		Conflicted: map[string][]string{},
	}
	p := planFor(t, []Task{
		{ClickUpID: "cu-1", Title: "root"},
		{ClickUpID: "cu-2", Title: "child", ClickUpParentID: "cu-1"},
	}, snap, cfg)

	child := p.Actions[len(p.Actions)-1]
	if child.ParentProductiveID != "9001" || child.ParentClickUpID != "" {
		t.Fatalf("child parent = %+v", child)
	}
}

func TestPlanUnresolvableParentCreatesFlatWithWarning(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	p := planFor(t, []Task{
		{ClickUpID: "cu-2", Title: "orphan", ClickUpParentID: "cu-outside"},
	}, productiveSnapshot{}, cfg)

	a := p.Actions[0]
	if a.FlatParent != "cu-outside" || a.ParentProductiveID != "" || a.ParentClickUpID != "" {
		t.Fatalf("action = %+v", a)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "outside the synced set") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
}

// A cycle saturates the depth guard and lands in the flat bucket, which is where
// .NET's queue-with-progress-flag leaves it too. It must not hang.
func TestPlanHandlesParentCycle(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	p := planFor(t, []Task{
		{ClickUpID: "cu-a", Title: "a", ClickUpParentID: "cu-b"},
		{ClickUpID: "cu-b", Title: "b", ClickUpParentID: "cu-a"},
	}, productiveSnapshot{}, cfg)

	if len(p.Actions) != 2 {
		t.Fatalf("actions = %d", len(p.Actions))
	}
	// Whichever way the cycle is broken, exactly one can reference the other.
	pending := 0
	for _, a := range p.Actions {
		if a.ParentClickUpID != "" {
			pending++
		}
	}
	if pending > 1 {
		t.Fatalf("a cycle cannot have both members waiting on each other: %+v", p.Actions)
	}
}

// Several Productive tasks claiming one ClickUp id: writing to either would make
// the drift permanent, so the id is skipped entirely.
func TestPlanSkipsConflictedIDs(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	snap := productiveSnapshot{
		ByClickUpID: map[string]existingTask{},
		Conflicted:  map[string][]string{"cu-1": {"9001", "9002"}},
	}
	p := planFor(t, []Task{{ClickUpID: "cu-1", Title: "x"}}, snap, cfg)

	if p.Conflicts != 1 || p.Creates != 0 || p.Updates != 0 {
		t.Fatalf("create=%d update=%d conflict=%d", p.Creates, p.Updates, p.Conflicts)
	}
	if p.Actions[0].Kind != actionConflict {
		t.Fatalf("kind = %v", p.Actions[0].Kind)
	}
}

func TestPlanReportsReparentNeeded(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	snap := productiveSnapshot{
		ByClickUpID: map[string]existingTask{
			"cu-1": {Task: Task{ClickUpID: "cu-1", Title: "root"}, ProductiveID: "9001"},
			// Child exists in Productive but flat: created before its parent was syncable.
			"cu-2": {Task: Task{ClickUpID: "cu-2", Title: "child"}, ProductiveID: "9002", ParentTaskID: ""},
		},
		Conflicted: map[string][]string{},
	}
	clickUp := []Task{
		{ClickUpID: "cu-1", Title: "root"},
		{ClickUpID: "cu-2", Title: "child", ClickUpParentID: "cu-1"},
	}

	p := planFor(t, clickUp, snap, cfg)
	if p.Updates != 0 {
		t.Fatalf("reparenting alone must not trigger a write by default; updates=%d", p.Updates)
	}
	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w, "reparent_needed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a reparent_needed warning, got %v", p.Warnings)
	}

	cfg.AllowReparent = true
	p = planFor(t, clickUp, snap, cfg)
	if p.Updates != 1 || p.Reasons["reparent"] != 1 {
		t.Fatalf("ALLOW_REPARENT should produce one update: %+v", p.Reasons)
	}
}

// Productive rejects nesting past one level with
// 422 "invalid level of subtasks". Flattening the too-deep task keeps it in
// Productive, and its own children then start a fresh level, so as much of the
// tree as Productive allows survives.
func TestPlanFlattensTooDeepSubtasks(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	cfg.MaxSubtaskDepth = 1

	p := planFor(t, []Task{
		{ClickUpID: "cu-1", Title: "root"},
		{ClickUpID: "cu-2", Title: "child", ClickUpParentID: "cu-1"},
		{ClickUpID: "cu-3", Title: "grandchild", ClickUpParentID: "cu-2"},
		{ClickUpID: "cu-4", Title: "great-grandchild", ClickUpParentID: "cu-3"},
	}, productiveSnapshot{}, cfg)

	byID := map[string]action{}
	for _, a := range p.Actions {
		byID[a.Task.ClickUpID] = a
	}

	if got := byID["cu-2"]; got.ParentClickUpID != "cu-1" || got.FlatParent != "" {
		t.Fatalf("level 1 must keep its parent: %+v", got)
	}
	if got := byID["cu-3"]; got.FlatParent != "cu-2" || got.ParentClickUpID != "" || got.ParentProductiveID != "" {
		t.Fatalf("level 2 must be flattened: %+v", got)
	}
	// The flattened task became a root, so its own child fits at level 1 again.
	if got := byID["cu-4"]; got.ParentClickUpID != "cu-3" || got.FlatParent != "" {
		t.Fatalf("child of a flattened task should re-chain: %+v", got)
	}

	found := false
	for _, w := range p.Warnings {
		if strings.Contains(w, "subtask_too_deep") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a subtask_too_deep warning, got %v", p.Warnings)
	}
}

func TestPlanMaxSubtaskDepthZeroDisablesNesting(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	cfg.MaxSubtaskDepth = 0

	p := planFor(t, []Task{
		{ClickUpID: "cu-1", Title: "root"},
		{ClickUpID: "cu-2", Title: "child", ClickUpParentID: "cu-1"},
	}, productiveSnapshot{}, cfg)

	for _, a := range p.Actions {
		if a.ParentClickUpID != "" || a.ParentProductiveID != "" {
			t.Fatalf("no task may keep a parent: %+v", a)
		}
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	snap := productiveSnapshot{ByClickUpID: map[string]existingTask{}, Conflicted: map[string][]string{}}
	for i := range 40 {
		id := "cu-" + strconv.Itoa(i)
		snap.ByClickUpID[id] = existingTask{
			Task:         Task{ClickUpID: id, Title: "old " + strconv.Itoa(i)},
			ProductiveID: strconv.Itoa(9000 + i),
		}
	}
	clickUp := make([]Task, 0, 40)
	for i := range 40 {
		clickUp = append(clickUp, Task{ClickUpID: "cu-" + strconv.Itoa(i), Title: "new " + strconv.Itoa(i)})
	}

	first := mustJSON(t, summarize(Plan(clickUp, snap, cfg), snap, cfg, true))
	for range 5 {
		if got := mustJSON(t, summarize(Plan(clickUp, snap, cfg), snap, cfg, true)); got != first {
			t.Fatal("Plan output varies between runs — map iteration order has leaked into the write order")
		}
	}
}

// --- Run() : end to end against the fakes ---

func runSync(t *testing.T, cfg Config, opts RunOptions) *Result {
	t.Helper()
	return Run(context.Background(), cfg, opts, testLogger())
}

func clickUpTasksFor(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := range n {
		out = append(out, clickUpTaskJSON("cu-"+strconv.Itoa(i), "Task "+strconv.Itoa(i), "open"))
	}
	return out
}

// THE acceptance test. A converged system must issue zero writes, and this single
// assertion covers all three eternal-churn bugs, the pointer-equality trap and the
// nil-versus-empty traps at once.
func TestRunConvergesToZeroWrites(t *testing.T) {
	tasks := []map[string]any{
		// Untagged — churn #1 would rewrite this one on every run.
		clickUpTaskJSON("cu-1", "Untagged", "open"),
		// Tagged, deliberately out of order so the sort is exercised.
		func() map[string]any {
			m := clickUpTaskJSON("cu-2", "Tagged", "open")
			m["tags"] = []any{map[string]any{"name": "urgent"}, map[string]any{"name": "backend"}}
			return m
		}(),
		// No estimate — churn #2 would rewrite this one forever.
		clickUpTaskJSON("cu-3", "No estimate", "open"),
		// With an estimate.
		func() map[string]any {
			m := clickUpTaskJSON("cu-4", "With estimate", "open")
			m["time_estimate"] = 5_400_000
			return m
		}(),
		// Sub-minute estimate: 0 minutes — churn #3.
		func() map[string]any {
			m := clickUpTaskJSON("cu-5", "Tiny estimate", "open")
			m["time_estimate"] = 30_000
			return m
		}(),
		// Done status.
		clickUpTaskJSON("cu-6", "Released", "wydane"),
		// Subtask.
		func() map[string]any {
			m := clickUpTaskJSON("cu-7", "Child", "open")
			m["parent"] = "cu-1"
			return m
		}(),
	}

	cu := newFakeClickUp(t, tasks)
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())

	first := runSync(t, cfg, RunOptions{})
	if first.Aborted != "" || first.Failed != 0 {
		t.Fatalf("first run: aborted=%q failed=%d errors=%v", first.Aborted, first.Failed, first.Errors)
	}
	if first.Created != len(tasks) {
		t.Fatalf("created %d, want %d", first.Created, len(tasks))
	}

	second := runSync(t, cfg, RunOptions{})
	if second.Created != 0 || second.Updated != 0 {
		t.Fatalf("NOT CONVERGED: created=%d updated=%d reasons=%v\nactions:\n%s",
			second.Created, second.Updated, second.Reasons, mustJSON(t, second.Actions))
	}
	if second.Skipped != len(tasks) {
		t.Fatalf("skipped %d, want %d", second.Skipped, len(tasks))
	}

	third := runSync(t, cfg, RunOptions{})
	if third.Created != 0 || third.Updated != 0 {
		t.Fatalf("third run drifted: created=%d updated=%d reasons=%v", third.Created, third.Updated, third.Reasons)
	}
}

// End to end against a fake that enforces Productive's real 140-character limit.
// The second assertion is the important one: truncating at mapping time is what
// makes the comparison converge. Truncating only when building the body would leave
// Productive holding the short title and ClickUp the long one — a PATCH every run,
// forever.
func TestRunLongTitleIsTruncatedAndConverges(t *testing.T) {
	longTitle := "Przygotować " + strings.Repeat("bardzo długi opis zadania ", 20)
	if len([]rune(longTitle)) <= 140 {
		t.Fatalf("test setup: title is only %d runes", len([]rune(longTitle)))
	}

	cu := newFakeClickUp(t, []map[string]any{clickUpTaskJSON("cu-1", longTitle, "open")})
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())

	first := runSync(t, cfg, RunOptions{})
	if first.Created != 1 || first.Failed != 0 {
		t.Fatalf("created=%d failed=%d errors=%v", first.Created, first.Failed, first.Errors)
	}
	stored := pr.findByClickUpID("cu-1")
	if len(stored) != 1 {
		t.Fatalf("stored %d tasks", len(stored))
	}
	if n := len([]rune(stored[0].Title)); n != 140 {
		t.Fatalf("stored title is %d runes, want exactly 140", n)
	}

	second := runSync(t, cfg, RunOptions{})
	if second.Created != 0 || second.Updated != 0 {
		t.Fatalf("NOT CONVERGED on a truncated title: created=%d updated=%d reasons=%v",
			second.Created, second.Updated, second.Reasons)
	}
}

// Three ClickUp levels against a Productive that only accepts one. The plan
// flattens level 2 up front, so no write is even attempted that Productive would
// reject — and nothing is lost.
func TestRunDeepSubtasksAreFlattenedNotFailed(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{
		clickUpTaskJSON("cu-1", "root", "open"),
		func() map[string]any {
			m := clickUpTaskJSON("cu-2", "child", "open")
			m["parent"] = "cu-1"
			return m
		}(),
		func() map[string]any {
			m := clickUpTaskJSON("cu-3", "grandchild", "open")
			m["parent"] = "cu-2"
			return m
		}(),
	})
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())

	res := runSync(t, cfg, RunOptions{})
	if res.Created != 3 || res.Failed != 0 {
		t.Fatalf("created=%d failed=%d errors=%v", res.Created, res.Failed, res.Errors)
	}

	root := pr.findByClickUpID("cu-1")[0]
	child := pr.findByClickUpID("cu-2")[0]
	grandchild := pr.findByClickUpID("cu-3")[0]

	if child.ParentTaskID != root.ID {
		t.Fatalf("child parent = %q, want %q", child.ParentTaskID, root.ID)
	}
	if grandchild.ParentTaskID != "" {
		t.Fatalf("grandchild should be flat, parent = %q", grandchild.ParentTaskID)
	}

	if second := runSync(t, cfg, RunOptions{}); second.Created != 0 || second.Updated != 0 {
		t.Fatalf("not converged: created=%d updated=%d", second.Created, second.Updated)
	}
}

// Safety net for MaxSubtaskDepth configured higher than Productive really allows.
// Retrying is safe here and only here: a 422 is a definitive rejection, so nothing
// was created and no duplicate is possible.
func TestExecuteRetriesCreateFlatOnSubtaskLevelRejection(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{
		clickUpTaskJSON("cu-1", "root", "open"),
		func() map[string]any {
			m := clickUpTaskJSON("cu-2", "child", "open")
			m["parent"] = "cu-1"
			return m
		}(),
		func() map[string]any {
			m := clickUpTaskJSON("cu-3", "grandchild", "open")
			m["parent"] = "cu-2"
			return m
		}(),
	})
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())
	cfg.MaxSubtaskDepth = 5 // deliberately wrong: the plan will attempt level 2

	res := runSync(t, cfg, RunOptions{})
	if res.Created != 3 || res.Failed != 0 {
		t.Fatalf("created=%d failed=%d errors=%v", res.Created, res.Failed, res.Errors)
	}
	if got := pr.findByClickUpID("cu-3"); len(got) != 1 || got[0].ParentTaskID != "" {
		t.Fatalf("grandchild should exist and be flat: %+v", got)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "retrying as a flat task") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the degradation must be reported, warnings = %v", res.Warnings)
	}
}

func TestRunSubtaskGetsParentCreatedInSameRun(t *testing.T) {
	tasks := []map[string]any{
		func() map[string]any { // listed BEFORE its parent on purpose
			m := clickUpTaskJSON("cu-child", "Child", "open")
			m["parent"] = "cu-root"
			return m
		}(),
		clickUpTaskJSON("cu-root", "Root", "open"),
	}
	cu := newFakeClickUp(t, tasks)
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())

	res := runSync(t, cfg, RunOptions{})
	if res.Created != 2 {
		t.Fatalf("created %d, want 2 (errors: %v)", res.Created, res.Errors)
	}

	root := pr.findByClickUpID("cu-root")
	child := pr.findByClickUpID("cu-child")
	if len(root) != 1 || len(child) != 1 {
		t.Fatalf("root=%d child=%d", len(root), len(child))
	}
	if child[0].ParentTaskID != root[0].ID {
		t.Fatalf("child parent = %q, want %q", child[0].ParentTaskID, root[0].ID)
	}
}

// The merge, proven through a fake that reproduces Productive's destructive
// whole-hash replacement.
func TestRunMergePreservesOtherCustomFields(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{clickUpTaskJSON("cu-1", "New title", "open")})
	pr := newFakeProductive(t)
	pr.seed(&fakeProductiveTask{
		ID:    "9001",
		Title: "Old title",
		CustomFields: map[string]json.RawMessage{
			"242457": json.RawMessage(`"cu-1"`),
			"242565": json.RawMessage(`""`),
			"999":    json.RawMessage(`"set by a human in Productive"`),
			"1000":   json.RawMessage(`42`),
		},
		WorkflowStatusID: "161082",
	})

	cfg := testConfig(cu.baseURL(), pr.baseURL())
	res := runSync(t, cfg, RunOptions{})
	if res.Updated != 1 {
		t.Fatalf("updated=%d reasons=%v errors=%v", res.Updated, res.Reasons, res.Errors)
	}

	after := pr.task("9001")
	if rawToString(after.CustomFields["999"]) != "set by a human in Productive" {
		t.Fatalf("custom field 999 was destroyed: %v", after.CustomFields)
	}
	if string(after.CustomFields["1000"]) != "42" {
		t.Fatalf("numeric custom field mangled: %s", after.CustomFields["1000"])
	}
	if after.Title != "New title" {
		t.Fatalf("title not updated: %q", after.Title)
	}

	// And with the merge off, the .NET behaviour is reproduced exactly.
	cu2 := newFakeClickUp(t, []map[string]any{clickUpTaskJSON("cu-1", "Newer title", "open")})
	pr2 := newFakeProductive(t)
	pr2.seed(&fakeProductiveTask{
		ID:    "9001",
		Title: "Old title",
		CustomFields: map[string]json.RawMessage{
			"242457": json.RawMessage(`"cu-1"`),
			"242565": json.RawMessage(`""`),
			"999":    json.RawMessage(`"about to be lost"`),
		},
		WorkflowStatusID: "161082",
	})
	cfg2 := testConfig(cu2.baseURL(), pr2.baseURL())
	cfg2.MergeCustomFields = false
	if res := runSync(t, cfg2, RunOptions{}); res.Updated != 1 {
		t.Fatalf("updated=%d errors=%v", res.Updated, res.Errors)
	}
	if _, still := pr2.task("9001").CustomFields["999"]; still {
		t.Fatal("with the merge off, the other custom field should be gone — the fake is not reproducing Productive")
	}
}

// A Productive read that fails without aborting looks exactly like an empty
// Productive, and would POST a duplicate of the entire ClickUp list.
func TestRunSoftReadFailureWritesNothing(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(50))
	pr := newFakeProductive(t)
	pr.readFailStatus = 500
	pr.readFailBody = `<html>500</html>`

	cfg := testConfig(cu.baseURL(), pr.baseURL())
	cfg.MaxRetries = 0

	res := runSync(t, cfg, RunOptions{})
	if res.Aborted != "fetch_productive" {
		t.Fatalf("aborted = %q, want fetch_productive", res.Aborted)
	}
	if res.Created != 0 || res.Updated != 0 {
		t.Fatalf("wrote %d creates and %d updates after a failed read", res.Created, res.Updated)
	}
	if w := pr.writes(); len(w) != 0 {
		t.Fatalf("Productive saw writes:\n%s", fmtCalls(w))
	}
}

// A POST that lands server-side but whose response is lost must NOT be retried:
// the ClickUp id is written atomically with the task, so the next run finds it and
// PATCHes instead of creating a twin.
func TestRunAmbiguousCreateIsNotRetriedAndSelfHeals(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{clickUpTaskJSON("cu-1", "Only task", "open")})
	pr := newFakeProductive(t)
	pr.postThenHangup = true

	cfg := testConfig(cu.baseURL(), pr.baseURL())
	res := runSync(t, cfg, RunOptions{})

	if res.Created != 0 || res.Failed != 1 {
		t.Fatalf("created=%d failed=%d", res.Created, res.Failed)
	}
	if res.Ambiguous != 1 {
		t.Fatalf("ambiguous = %d; a lost POST response must be reported as ambiguous", res.Ambiguous)
	}
	posts := 0
	for _, c := range pr.writes() {
		if c.Method == http.MethodPost {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("POST issued %d times; retrying an ambiguous create duplicates the task", posts)
	}
	if got := pr.findByClickUpID("cu-1"); len(got) != 1 {
		t.Fatalf("Productive holds %d tasks for cu-1 after the first run", len(got))
	}

	// Second run: the task is discoverable by its ClickUp custom field.
	pr.postThenHangup = false
	res = runSync(t, cfg, RunOptions{})
	if res.Created != 0 {
		t.Fatalf("second run created %d tasks — the self-healing path is broken", res.Created)
	}
	if got := pr.findByClickUpID("cu-1"); len(got) != 1 {
		t.Fatalf("DUPLICATE: %d tasks claim cu-1", len(got))
	}
}

func TestRunDuplicateClickUpIDIsNeverWritten(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{clickUpTaskJSON("cu-1", "Changed title", "open")})
	pr := newFakeProductive(t)
	pr.seed(seededTask("9001", "cu-1", "Old", ""))
	pr.seed(seededTask("9002", "cu-1", "Old twin", ""))

	cfg := testConfig(cu.baseURL(), pr.baseURL())
	res := runSync(t, cfg, RunOptions{})

	if res.Created != 0 || res.Updated != 0 {
		t.Fatalf("created=%d updated=%d; neither twin may be written", res.Created, res.Updated)
	}
	if res.Planned.Conflict != 1 {
		t.Fatalf("conflict not planned: %+v", res.Planned)
	}
	if w := pr.writes(); len(w) != 0 {
		t.Fatalf("writes issued:\n%s", fmtCalls(w))
	}
}

func TestRunDryRunIssuesNoWritesButRendersBodies(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(3))
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())

	res := runSync(t, cfg, RunOptions{DryRun: true})
	if !res.DryRun || res.Created != 0 {
		t.Fatalf("dry run created %d", res.Created)
	}
	if res.Planned.Create != 3 {
		t.Fatalf("planned = %+v", res.Planned)
	}
	if w := pr.writes(); len(w) != 0 {
		t.Fatalf("dry run issued writes:\n%s", fmtCalls(w))
	}
	if len(res.Actions) != 3 || len(res.Actions[0].Body) == 0 {
		t.Fatalf("dry run must render the exact bodies it would send: %s", mustJSON(t, res.Actions))
	}
	if pr.count() != 0 {
		t.Fatalf("Productive mutated during a dry run")
	}
}

func TestRunCircuitBreakers(t *testing.T) {
	t.Run("max creates", func(t *testing.T) {
		cu := newFakeClickUp(t, clickUpTasksFor(10))
		pr := newFakeProductive(t)
		cfg := testConfig(cu.baseURL(), pr.baseURL())
		cfg.MaxCreates = 3

		res := runSync(t, cfg, RunOptions{})
		if res.Aborted != "max_creates_exceeded" {
			t.Fatalf("aborted = %q", res.Aborted)
		}
		if len(pr.writes()) != 0 {
			t.Fatal("the breaker must trip BEFORE any write")
		}
		if len(res.Actions) == 0 {
			t.Fatal("the plan must be attached so the operator can see what it wanted to do")
		}
	})

	t.Run("overridable per request", func(t *testing.T) {
		cu := newFakeClickUp(t, clickUpTasksFor(10))
		pr := newFakeProductive(t)
		cfg := testConfig(cu.baseURL(), pr.baseURL())
		cfg.MaxCreates = 3

		ten := 10
		res := runSync(t, cfg, RunOptions{MaxCreates: &ten})
		if res.Aborted != "" || res.Created != 10 {
			t.Fatalf("aborted=%q created=%d", res.Aborted, res.Created)
		}
	})

	t.Run("max writes", func(t *testing.T) {
		cu := newFakeClickUp(t, []map[string]any{
			clickUpTaskJSON("cu-1", "changed a", "open"),
			clickUpTaskJSON("cu-2", "changed b", "open"),
		})
		pr := newFakeProductive(t)
		pr.seed(seededTask("9001", "cu-1", "old a", ""))
		pr.seed(seededTask("9002", "cu-2", "old b", ""))

		cfg := testConfig(cu.baseURL(), pr.baseURL())
		cfg.MaxWrites = 1

		res := runSync(t, cfg, RunOptions{})
		if res.Aborted != "max_writes_exceeded" {
			t.Fatalf("aborted = %q", res.Aborted)
		}
		if len(pr.writes()) != 0 {
			t.Fatal("nothing may be written once the breaker trips")
		}
	})
}

// Two independent runs from identical starting state must produce byte-identical
// request sequences. This is what catches map-iteration order leaking into the
// write order, which would otherwise bake non-reproducible parenting into
// Productive (PATCH never fixes parent_task).
func TestRunWriteSequenceIsDeterministic(t *testing.T) {
	build := func(t *testing.T) []recordedCall {
		tasks := []map[string]any{}
		for i := range 12 {
			m := clickUpTaskJSON("cu-"+strconv.Itoa(i), "Task "+strconv.Itoa(i), "open")
			if i%3 == 1 {
				m["parent"] = "cu-" + strconv.Itoa(i-1)
			}
			if i%4 == 0 {
				m["tags"] = []any{map[string]any{"name": "z"}, map[string]any{"name": "a"}}
			}
			tasks = append(tasks, m)
		}
		cu := newFakeClickUp(t, tasks)
		pr := newFakeProductive(t)
		cfg := testConfig(cu.baseURL(), pr.baseURL())

		res := runSync(t, cfg, RunOptions{})
		if res.Aborted != "" || res.Failed != 0 {
			t.Fatalf("aborted=%q failed=%d errors=%v", res.Aborted, res.Failed, res.Errors)
		}
		return pr.writes()
	}

	a, b := build(t), build(t)
	if len(a) != len(b) {
		t.Fatalf("write counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Method != b[i].Method || a[i].Path != b[i].Path {
			t.Fatalf("call %d differs: %s %s vs %s %s", i, a[i].Method, a[i].Path, b[i].Method, b[i].Path)
		}
		// Bodies carry generated parent ids, which differ between fakes; compare
		// everything else.
		sa, sb := stripParentIDs(a[i].Body), stripParentIDs(b[i].Body)
		if sa != sb {
			t.Fatalf("call %d body differs:\n%s\n%s", i, sa, sb)
		}
	}
}

func stripParentIDs(body string) string {
	idx := strings.Index(body, `"parent_task"`)
	if idx < 0 {
		return body
	}
	return body[:idx]
}

func TestRunClickUpClientIsAlwaysReadOnly(t *testing.T) {
	cu := newFakeClickUp(t, clickUpTasksFor(2))
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())

	if res := runSync(t, cfg, RunOptions{}); res.Created != 2 {
		t.Fatalf("created=%d errors=%v", res.Created, res.Errors)
	}
	for _, c := range cu.recorded() {
		if c.Method != http.MethodGet {
			t.Fatalf("this integration is one-way; ClickUp saw %s %s", c.Method, c.Path)
		}
	}
}

func TestRunReportsStatsAndDiagnostics(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{
		clickUpTaskJSON("cu-1", "a", "open"),
		clickUpTaskJSON("cu-2", "b", "wydane"),
	})
	pr := newFakeProductive(t)
	cfg := testConfig(cu.baseURL(), pr.baseURL())

	res := runSync(t, cfg, RunOptions{DryRun: true})

	if res.ClickUp.StatusTypes["open|custom"] != 1 || res.ClickUp.StatusTypes["wydane|custom"] != 1 {
		t.Fatalf("status type histogram missing: %v", res.ClickUp.StatusTypes)
	}
	if res.Productive.WorkflowStatusNames["161082"] != "Open" {
		t.Fatalf("workflow status names missing: %v", res.Productive.WorkflowStatusNames)
	}
	if res.ProductiveAPI.Requests == 0 || res.ClickUpAPI.Requests == 0 {
		t.Fatalf("api stats not reported: %+v %+v", res.ProductiveAPI, res.ClickUpAPI)
	}
	if res.RunID == "" || res.DurationMs < 0 {
		t.Fatalf("run metadata missing: %+v", res)
	}
}

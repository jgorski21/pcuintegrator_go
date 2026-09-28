package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
)

// --- read DTOs ---

type productiveTasksPage struct {
	Data     []productiveTaskData `json:"data"`
	Included []productiveIncluded `json:"included"`
	Meta     productiveMeta       `json:"meta"`
}

// productiveMeta is documented as always present on collection responses.
type productiveMeta struct {
	CurrentPage flexInt `json:"current_page"`
	TotalPages  flexInt `json:"total_pages"`
	TotalCount  flexInt `json:"total_count"`
	PageSize    flexInt `json:"page_size"`
	MaxPageSize flexInt `json:"max_page_size"`
}

type productiveTaskData struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Title           string  `json:"title"`
		InitialEstimate flexInt `json:"initial_estimate"` // minutes
		// A whole-hash attribute, and values are polymorphic (string / number /
		// array of strings depending on the field's data type). RawMessage is
		// mandatory rather than stylistic: the merge writes these values back, and
		// map[string]any would corrupt large numeric ids through float64.
		CustomFields map[string]json.RawMessage `json:"custom_fields"`
	} `json:"attributes"`
	Relationships struct {
		WorkflowStatus productiveRel `json:"workflow_status"`
		ParentTask     productiveRel `json:"parent_task"`
	} `json:"relationships"`
}

type productiveRel struct {
	Data *productiveRef `json:"data"`
}

type productiveRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type productiveIncluded struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Name string `json:"name"`
	} `json:"attributes"`
}

// existingTask is a Productive task that already carries a ClickUp id.
type existingTask struct {
	Task
	ProductiveID     string
	WorkflowStatusID string
	ParentTaskID     string                     // "" when top-level
	RawCustomFields  map[string]json.RawMessage // FULL hash, needed for the merge
}

type productiveFetchStats struct {
	Tasks       int `json:"tasks"`
	Pages       int `json:"pages"`
	TotalCount  int `json:"total_count"`
	WithoutKey  int `json:"without_clickup_id"`
	ExtraFields int `json:"tasks_with_extra_custom_fields"`
	// WorkflowStatusNames answers, in the dry-run output, whether the configured
	// Done status is literally named "Closed". If it is not, the Productive side of
	// the status mapping never reports Done and every completed task is rewritten
	// on every run.
	WorkflowStatusNames map[string]string `json:"workflow_status_names,omitempty"`
	// ExtraCustomFieldIDs is the evidence for whether the custom_fields merge is a
	// no-op on this data set or is actively preventing data loss.
	ExtraCustomFieldIDs map[string]int `json:"extra_custom_field_ids,omitempty"`
}

type productiveSnapshot struct {
	ByClickUpID map[string]existingTask
	// Conflicted maps a ClickUp id to the several Productive tasks claiming it.
	// Those ids are excluded from writing entirely — picking one twin silently
	// (which a plain Go map assignment would do) turns a one-off duplicate into
	// permanent invisible drift, and .NET's ToDictionary crash-loops on it.
	Conflicted map[string][]string
	Stats      productiveFetchStats
	Warnings   []string
}

// fetchProductive pages the task list and indexes it by ClickUp id.
//
// `page[number]` is 1-based and `page[size]` maxes out at the 200 used here. The
// loop is driven by meta.total_pages, so it does not pay for the trailing empty
// request that .NET makes, and it asserts the collected count against
// meta.total_count: coming up short means a task slid across a page boundary while
// we were reading, which would make it look new and get it POSTed as a duplicate.
func fetchProductive(ctx context.Context, c *apiClient, cfg Config) (productiveSnapshot, error) {
	snap := productiveSnapshot{
		ByClickUpID: map[string]existingTask{},
		Conflicted:  map[string][]string{},
		Stats: productiveFetchStats{
			WorkflowStatusNames: map[string]string{},
			ExtraCustomFieldIDs: map[string]int{},
		},
	}

	pages, err := fetchProductivePages(ctx, c, cfg.ProductiveTaskListID, "workflow_status,assignee", cfg.MaxPages)
	snap.Stats.Pages, snap.Stats.TotalCount = pages.Pages, pages.TotalCount
	if err != nil {
		// HARD ABORT, and this is the single most important line in the file.
		// Continuing with a partial or empty snapshot after a 401/429/500/HTML
		// error page means every ClickUp task looks new, which POSTs a
		// duplicate of the entire list — each one stamped with the ClickUp id,
		// so it cannot be told apart from a legitimate task afterwards.
		return snap, err
	}
	statusNames, collected := pages.StatusNames, pages.Data

	for id, name := range statusNames {
		snap.Stats.WorkflowStatusNames[id] = name
	}

	for _, d := range collected {
		snap.Stats.Tasks++

		clickUpID := rawToString(d.Attributes.CustomFields[cfg.CFClickUpID])
		if clickUpID == "" {
			// Parity: tasks without a ClickUp id are simply not part of the sync.
			snap.Stats.WithoutKey++
			continue
		}

		wsID := ""
		if d.Relationships.WorkflowStatus.Data != nil {
			wsID = d.Relationships.WorkflowStatus.Data.ID
		}
		status := StatusOpen
		if name, ok := statusNames[wsID]; ok {
			if cfg.ProductiveDoneNames[name] {
				status = StatusDone
			}
		} else if wsID != "" {
			// .NET throws KeyNotFoundException here, which kills the run.
			snap.Warnings = append(snap.Warnings, fmt.Sprintf(
				"productive %s: workflow_status %s missing from `included`; assuming open",
				d.ID, wsID))
		}

		parentID := ""
		if d.Relationships.ParentTask.Data != nil {
			parentID = d.Relationships.ParentTask.Data.ID
		}

		var estimate *int
		if d.Attributes.InitialEstimate.Set {
			estimate = intPtr(int(d.Attributes.InitialEstimate.V))
		}

		for k, v := range d.Attributes.CustomFields {
			if k == cfg.CFClickUpID || k == cfg.CFClickUpTags {
				continue
			}
			if isJSONNull(v) {
				continue
			}
			snap.Stats.ExtraCustomFieldIDs[k]++
		}
		if hasExtraCustomFields(d.Attributes.CustomFields, cfg) {
			snap.Stats.ExtraFields++
		}

		et := existingTask{
			Task: Task{
				ClickUpID:       clickUpID,
				Title:           d.Attributes.Title,
				Status:          status,
				Tags:            parseTagsField(rawToString(d.Attributes.CustomFields[cfg.CFClickUpTags])),
				InitialEstimate: estimate,
			},
			ProductiveID:     d.ID,
			WorkflowStatusID: wsID,
			ParentTaskID:     parentID,
			RawCustomFields:  d.Attributes.CustomFields,
		}

		if prev, dup := snap.ByClickUpID[clickUpID]; dup {
			ids := snap.Conflicted[clickUpID]
			if len(ids) == 0 {
				ids = append(ids, prev.ProductiveID)
			}
			ids = append(ids, et.ProductiveID)
			sort.Strings(ids)
			snap.Conflicted[clickUpID] = ids
			snap.Warnings = append(snap.Warnings, fmt.Sprintf(
				"productive: clickup id %s claimed by %d tasks (%v); excluded from writing",
				clickUpID, len(ids), ids))
			continue
		}
		snap.ByClickUpID[clickUpID] = et
	}

	if len(snap.Stats.ExtraCustomFieldIDs) == 0 {
		snap.Stats.ExtraCustomFieldIDs = nil
	}
	return snap, nil
}

// productivePages is one task list, read in full.
type productivePages struct {
	Data        []productiveTaskData
	StatusNames map[string]string // workflow_status id -> name, from `included`
	Pages       int
	TotalCount  int
}

// fetchProductivePages pages one task list. Shared by the sync (which needs
// include=workflow_status,assignee) and the DeliverIT fanout (id + title only,
// include=""). Any non-2xx and any short read against meta.total_count is an error:
// the caller must never mistake a partial read for the whole list.
func fetchProductivePages(ctx context.Context, c *apiClient, taskListID, include string, maxPages int) (productivePages, error) {
	out := productivePages{StatusNames: map[string]string{}, Data: make([]productiveTaskData, 0, 256)}
	seenProductiveID := map[string]bool{}
	totalPages := 0

	includeParam := ""
	if include != "" {
		includeParam = "&include=" + include
	}

	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("tasks?filter[task_list_id]=%s%s&page[number]=%d&page[size]=200",
			taskListID, includeParam, page)

		var body productiveTasksPage
		if err := c.getJSON(ctx, path, &body); err != nil {
			return out, fmt.Errorf("productive: %w", err)
		}
		out.Pages++

		// A page may legitimately carry no `included` (.NET dereferences it
		// unconditionally and NREs).
		for _, inc := range body.Included {
			if inc.Type == "workflow_statuses" {
				out.StatusNames[inc.ID] = inc.Attributes.Name
			}
		}

		for _, d := range body.Data {
			if seenProductiveID[d.ID] {
				continue // same task seen twice due to ordering drift
			}
			seenProductiveID[d.ID] = true
			out.Data = append(out.Data, d)
		}

		if page == 1 && body.Meta.TotalPages.Set {
			totalPages = int(body.Meta.TotalPages.V)
			out.TotalCount = int(body.Meta.TotalCount.V)
		}

		if totalPages > 0 {
			if page >= totalPages {
				break
			}
			continue
		}
		// No usable meta: fall back to .NET's terminate-on-empty-page behaviour.
		if len(body.Data) == 0 {
			break
		}
	}

	if out.TotalCount > 0 && len(out.Data) < out.TotalCount {
		return out, fmt.Errorf(
			"productive: read %d of %d tasks (meta.total_count) — the list changed while paging; "+
				"aborting rather than treating missing tasks as new",
			len(out.Data), out.TotalCount)
	}
	return out, nil
}

func hasExtraCustomFields(cf map[string]json.RawMessage, cfg Config) bool {
	for k, v := range cf {
		if k == cfg.CFClickUpID || k == cfg.CFClickUpTags || isJSONNull(v) {
			continue
		}
		return true
	}
	return false
}

// --- write DTOs ---
//
// Structs rather than maps so field order — and therefore the request body — is
// byte-deterministic, which is what makes golden tests possible. Map keys inside
// custom_fields are sorted by encoding/json, so those are deterministic too.

type writeEnvelope struct {
	Data writeResource `json:"data"`
}

type writeResource struct {
	// No `id`: .NET never sends one (its Id property lacks a JsonPropertyName and
	// is null on write), and this shape is the one proven in production. JSON:API
	// would prefer it on PATCH; deviating buys nothing and risks everything.
	Type          string     `json:"type"`
	Attributes    writeAttrs `json:"attributes"`
	Relationships writeRels  `json:"relationships"`
}

type writeAttrs struct {
	Title string `json:"title"`
	// RawMessage, not *int: distinguishes omit (nil) from explicit null from a
	// number. Omitted when there is no estimate, matching WhenWritingNull.
	InitialEstimate json.RawMessage            `json:"initial_estimate,omitempty"`
	CustomFields    map[string]json.RawMessage `json:"custom_fields"`
	// remaining_time / start_date / due_date are DELIBERATELY ABSENT. .NET emits
	// all three as explicit nulls in every POST and PATCH (only InitialEstimate
	// carries WhenWritingNull), so every update it makes clears the start and due
	// dates of the Productive task. Not reproduced.
}

type writeRels struct {
	Project        *writeRel `json:"project,omitempty"`
	TaskList       *writeRel `json:"task_list,omitempty"`
	WorkflowStatus *writeRel `json:"workflow_status,omitempty"`
	ParentTask     *writeRel `json:"parent_task,omitempty"`
}

type writeRel struct {
	Data productiveRef `json:"data"`
}

func ref(typ, id string) *writeRel { return &writeRel{Data: productiveRef{Type: typ, ID: id}} }

// buildBody produces the request body for both create and update.
//
//	existing == nil -> create. parent_task set when parentProductiveID != "".
//	existing != nil -> update. parent_task omitted (parenting is deliberately
//	                   outside change detection) unless AllowReparent is on, and
//	                   custom_fields is the existing hash with the two managed
//	                   keys overwritten.
func buildBody(t Task, parentProductiveID string, existing *existingTask, cfg Config) ([]byte, error) {
	attrs := writeAttrs{
		Title:        t.Title,
		CustomFields: map[string]json.RawMessage{},
	}

	switch {
	case t.InitialEstimate != nil:
		attrs.InitialEstimate = json.RawMessage(strconv.Itoa(*t.InitialEstimate))
	case existing != nil && wantsEstimateClear(t.InitialEstimate, existing.InitialEstimate, cfg.EstimateMode):
		attrs.InitialEstimate = json.RawMessage("null")
	}

	// Productive treats custom_fields as ONE attribute: a PATCH carrying two keys
	// means "set the whole hash to these two keys" and everything else on the task
	// is lost. The list fetch already returned the full hash, so merging costs no
	// extra request. Keys whose value is JSON null are dropped — several field
	// types are read-only or reject their own read representation, and echoing one
	// back would 422 every PATCH.
	if existing != nil && cfg.MergeCustomFields {
		for k, v := range existing.RawCustomFields {
			if k == cfg.CFClickUpID || k == cfg.CFClickUpTags || isJSONNull(v) {
				continue
			}
			attrs.CustomFields[k] = append(json.RawMessage(nil), v...)
		}
	}
	idRaw, err := json.Marshal(t.ClickUpID)
	if err != nil {
		return nil, err
	}
	tagsRaw, err := json.Marshal(formatTagsField(t.Tags))
	if err != nil {
		return nil, err
	}
	attrs.CustomFields[cfg.CFClickUpID] = idRaw
	attrs.CustomFields[cfg.CFClickUpTags] = tagsRaw

	rels := writeRels{
		Project:        ref("projects", cfg.ProductiveProjectID),
		TaskList:       ref("task_lists", cfg.ProductiveTaskListID),
		WorkflowStatus: ref("workflow_statuses", cfg.statusIDFor(t.Status)),
	}
	switch {
	case existing == nil && parentProductiveID != "":
		rels.ParentTask = ref("tasks", parentProductiveID)
	case existing != nil && cfg.AllowReparent && parentProductiveID != "" && existing.ParentTaskID != parentProductiveID:
		rels.ParentTask = ref("tasks", parentProductiveID)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Keep "<", ">", "&" literal so bodies are comparable to what .NET sends.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(writeEnvelope{Data: writeResource{
		Type:          "tasks",
		Attributes:    attrs,
		Relationships: rels,
	}}); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// createTask POSTs and returns the new Productive id.
//
// The returned id matters beyond bookkeeping: subtasks created later in the same
// run need it for parent_task. A create whose response is lost is NOT retried
// (see retryable) — the ClickUp id is written atomically with the task, so the
// next run finds it and PATCHes instead of duplicating.
func createTask(ctx context.Context, c *apiClient, body []byte) (string, error) {
	status, resp, err := c.do(ctx, http.MethodPost, "tasks", body)
	if err != nil {
		return "", err
	}
	if status < 200 || status > 299 {
		return "", &httpError{Method: http.MethodPost, Path: "tasks", Status: status, Body: resp}
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return "", fmt.Errorf("POST tasks: created but response undecodable: %w", err)
	}
	if env.Data.ID == "" {
		return "", fmt.Errorf("POST tasks: created but response carried no data.id")
	}
	return env.Data.ID, nil
}

func updateTask(ctx context.Context, c *apiClient, productiveID string, body []byte) error {
	path := "tasks/" + productiveID
	status, resp, err := c.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return &httpError{Method: http.MethodPatch, Path: path, Status: status, Body: resp}
	}
	return nil
}

func productiveHeader(cfg Config) http.Header {
	h := http.Header{}
	h.Set("X-Organization-Id", cfg.ProductiveOrgID)
	h.Set("X-Auth-Token", cfg.ProductiveToken)
	h.Set("Accept", "application/vnd.api+json")
	return h
}

// --- json helpers ---

func isJSONNull(raw json.RawMessage) bool {
	v := bytes.TrimSpace(raw)
	return len(v) == 0 || bytes.Equal(v, []byte("null"))
}

// rawToString reads a custom field value that should be textual but may arrive as
// a number.
func rawToString(raw json.RawMessage) string {
	v := bytes.TrimSpace(raw)
	if isJSONNull(v) {
		return ""
	}
	if v[0] == '"' {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return ""
		}
		return s
	}
	return string(v)
}

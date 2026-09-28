package main

// The DeliverIT fanout: the second stage of every run, after ClickUp -> Productive.
//
//	GET /api/integrations/projects            which projects are linked to which source list
//	  -> per project: read that list's tasks from ClickUp (1) or Productive (2)
//	  -> filter, trim, dedupe, cut into batches of 200
//	  -> POST /api/integrations/projects/{id}/tasks/sync
//
// It is INDEPENDENT of the first stage: it runs whatever that stage's outcome —
// including an aborted Productive read, since it only needs the source's tasks —
// and it never aborts it. Its failures make the run a 207, never a 422.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"
)

const (
	// deliverITMinBudget is what must be left of SYNC_TIMEOUT to start the stage:
	// one full request timeout, which also covers a cold DeliverIT container.
	deliverITMinBudget = deliverITRequestTimeout
	// deliverITProjectBudget is what must be left to start one more project.
	deliverITProjectBudget = 15 * time.Second
)

type deliverITBatchPlan struct {
	Tasks int             `json:"tasks"`
	Body  json.RawMessage `json:"body"` // the exact request body; the key is a header, never in here
}

type deliverITProjectResult struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ClientName string `json:"client_name,omitempty"`
	Source     string `json:"source"`
	ListID     string `json:"list_id"`
	// ListName is the source list's own name, read in a dry run only: a human
	// checks the project <-> list pairing before tasks land in the wrong project
	// (which DeliverIT cannot undo). "?" = could not be read.
	ListName string `json:"list_name,omitempty"`
	// synced | planned (dry run) | skipped | failed
	Status    string               `json:"status"`
	Reason    string               `json:"reason,omitempty"`
	Tasks     int                  `json:"tasks"`
	Batches   int                  `json:"batches"`
	Created   int                  `json:"created"`
	Renamed   int                  `json:"renamed"`
	Unchanged int                  `json:"unchanged"`
	Skipped   int                  `json:"skipped"`
	Plan      []deliverITBatchPlan `json:"plan,omitempty"` // explain only
}

// deliverITResult is the "deliverit" section of GET /sync and /status.
type deliverITResult struct {
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode,omitempty"` // proxied | direct
	DisabledReason string `json:"disabled_reason,omitempty"`
	APIKeyPrefix   string `json:"api_key_prefix,omitempty"` // dit_XXXXXXXX… — never the key

	Projects        int `json:"projects"` // linked projects DeliverIT returned
	SyncedProjects  int `json:"synced_projects"`
	SkippedProjects int `json:"skipped_projects"`
	FailedProjects  int `json:"failed_projects"`

	Tasks   int `json:"tasks"` // prepared for sending (dry run: would be sent)
	Batches int `json:"batches"`

	Created   int `json:"created"`
	Renamed   int `json:"renamed"`
	Unchanged int `json:"unchanged"`
	// Skipped counts tasks that did not land: filtered here (a server rule would
	// have rejected the whole batch) or skipped by the server. Fixed in the source.
	Skipped        int            `json:"skipped"`
	SkippedReasons map[string]int `json:"skipped_reasons,omitempty"`

	ProjectResults []deliverITProjectResult `json:"project_results,omitempty"`
	Warnings       []string                 `json:"warnings,omitempty"`
	Errors         []string                 `json:"errors,omitempty"`
	API            clientStats              `json:"api"`
}

func (d *deliverITResult) failed() bool { return len(d.Errors) > 0 }

// clickUpRead is what the ClickUp -> Productive stage learned about CLICKUP_LIST_ID,
// so the fanout does not read the same list twice in one run.
type clickUpRead struct {
	attempted bool
	tasks     []Task
	err       error
}

type deliverITStage struct {
	cfg        Config
	opts       RunOptions
	setup      deliverITSetup
	client     *deliverITClient
	clickUp    *apiClient // shared with the first stage: one rate budget per token
	productive *apiClient // shared with the first stage: Productive's limits are org-wide
	read       clickUpRead
	log        *slog.Logger
	res        *deliverITResult
}

// runDeliverIT returns a NAMED result so the deferred API stats land in what the
// caller receives.
func runDeliverIT(ctx context.Context, cfg Config, opts RunOptions, cu, pr *apiClient, read clickUpRead, log *slog.Logger) (res deliverITResult) {
	setup := cfg.deliverITSetup()
	res = deliverITResult{Enabled: setup.Enabled, DisabledReason: setup.DisabledReason, APIKeyPrefix: setup.KeyLabel}
	if setup.Base != nil {
		res.Mode = "direct"
		if setup.Proxied {
			res.Mode = "proxied"
		}
	}
	s := &deliverITStage{cfg: cfg, opts: opts, setup: setup, clickUp: cu, productive: pr, read: read,
		log: log.With("stage", "deliverit"), res: &res}
	// Registered first, so it runs last: a bug in this stage must not cost the run
	// its summary. Stage 1 has already written; an unrecovered panic in this
	// goroutine would kill the process — no response, no /status, and the DO lease
	// left to expire on its own.
	defer s.recoverPanic()

	if setup.Warning != "" {
		s.warn(setup.Warning)
	}
	if setup.Error != "" {
		s.fail(setup.Error)
	}
	if !setup.Enabled {
		s.log.Info("deliverit fanout disabled", "reason", setup.DisabledReason)
		return res
	}

	if isShuttingDown() {
		s.warn("SIGTERM received; DeliverIT fanout skipped — the next run sends everything")
		return res
	}
	if left, ok := timeLeft(ctx); ok && left < deliverITMinBudget {
		s.warn(fmt.Sprintf("DeliverIT fanout skipped: %s left of SYNC_TIMEOUT, needs at least %s — the next run sends everything",
			left.Round(time.Second), deliverITMinBudget))
		return res
	}

	s.client = newDeliverITClient(setup, cfg, opts.DryRun, log)
	defer func() { res.API = s.client.Stats() }()
	s.log.Info("deliverit fanout starting", "mode", res.Mode, "base_url", setup.Base.String(),
		"api_key_prefix", setup.KeyLabel)

	projects, err := s.client.listLinkedProjects(ctx)
	if err != nil {
		s.fail("list linked projects: " + err.Error() + authHint(err))
		return res
	}
	res.Projects = len(projects)

	for i, p := range projects {
		if reason := s.stopReason(ctx); reason != "" {
			s.skipRemaining(projects[i:], reason)
			s.warn(fmt.Sprintf("DeliverIT fanout stopped (%s): %d project(s) not sent — the next run continues",
				reason, len(projects)-i))
			break
		}
		if stop := s.syncProject(ctx, p); stop {
			s.skipRemaining(projects[i+1:], "stage_stopped")
			break
		}
	}

	s.log.Info("deliverit fanout complete", "projects", res.Projects, "synced", res.SyncedProjects,
		"skipped", res.SkippedProjects, "failed", res.FailedProjects, "created", res.Created,
		"renamed", res.Renamed, "unchanged", res.Unchanged, "skipped_tasks", res.Skipped)
	return res
}

// recoverPanic turns a panic into an error of this stage (run -> 207). The project
// being processed was already recorded by syncProject's own defer, with no status.
func (s *deliverITStage) recoverPanic() {
	r := recover()
	if r == nil {
		return
	}
	for i := range s.res.ProjectResults {
		if pr := &s.res.ProjectResults[i]; pr.Status == "" {
			pr.Status, pr.Reason = "failed", "internal_error"
			s.res.FailedProjects++
		}
	}
	s.log.Error("deliverit fanout panicked", "stack", string(debug.Stack()))
	s.fail(fmt.Sprintf("DeliverIT fanout aborted by an internal error (a bug in pcuintegrator): %v — "+
		"the ClickUp -> Productive stage is unaffected", r))
}

func (s *deliverITStage) stopReason(ctx context.Context) string {
	switch {
	case isShuttingDown():
		return "sigterm"
	case ctx.Err() != nil:
		return "deadline"
	}
	if left, ok := timeLeft(ctx); ok && left < deliverITProjectBudget {
		return "deadline"
	}
	return ""
}

func (s *deliverITStage) skipRemaining(projects []ditLinkedProject, reason string) {
	for _, p := range projects {
		pr := newProjectResult(p)
		pr.Status, pr.Reason = "skipped", reason
		s.res.SkippedProjects++
		s.res.ProjectResults = append(s.res.ProjectResults, pr)
	}
}

func newProjectResult(p ditLinkedProject) deliverITProjectResult {
	return deliverITProjectResult{
		ID: p.ID, Name: p.Name, ClientName: p.ClientName,
		Source: p.TaskList.Source.String(), ListID: p.TaskList.ExternalID,
	}
}

// syncProject handles one project; stop=true means the key was rejected and no
// further request can succeed.
func (s *deliverITStage) syncProject(ctx context.Context, p ditLinkedProject) (stop bool) {
	pr := newProjectResult(p)
	label := fmt.Sprintf("project %q (%s, %s list %s)", p.Name, p.ID, pr.Source, p.TaskList.ExternalID)
	log := s.log.With("project", p.Name, "project_id", p.ID, "client", p.ClientName,
		"source", pr.Source, "list", p.TaskList.ExternalID)
	defer func() { s.res.ProjectResults = append(s.res.ProjectResults, pr) }()

	fetched, skipReason, skipDetail, err := s.sourceTasks(ctx, p)
	switch {
	case skipReason != "":
		pr.Status, pr.Reason = "skipped", skipReason
		s.res.SkippedProjects++
		s.warn(label + " skipped: " + skipDetail)
		return false
	case err != nil:
		pr.Status, pr.Reason = "failed", "source_read_failed"
		s.res.FailedProjects++
		s.fail(label + ": reading the source failed: " + err.Error())
		return false
	}

	tasks := s.prepare(label, fetched, &pr)
	batches := chunkDeliverITTasks(tasks, deliverITBatchSize)
	pr.Tasks, pr.Batches = len(tasks), len(batches)
	s.res.Tasks += pr.Tasks
	s.res.Batches += pr.Batches

	if s.opts.Explain {
		for _, batch := range batches {
			body, err := marshalDeliverITRequest(ditSyncRequest{TaskList: p.TaskList, Tasks: batch})
			if err == nil {
				pr.Plan = append(pr.Plan, deliverITBatchPlan{Tasks: len(batch), Body: body})
			}
		}
	}

	if s.opts.DryRun {
		pr.Status = "planned"
		name, err := s.sourceListName(ctx, p)
		if err != nil {
			name = "?"
			s.warn(fmt.Sprintf("%s: could not read the source list name (%v); check the link by hand", label, err))
		}
		pr.ListName = name
		log.Info("deliverit dry run: nothing sent", "list_name", name, "tasks", pr.Tasks, "batches", pr.Batches)
		return false
	}

	for i, batch := range batches {
		if reason := s.stopReason(ctx); reason != "" && i > 0 {
			pr.Status, pr.Reason = "failed", reason
			s.res.FailedProjects++
			s.fail(fmt.Sprintf("%s: stopped (%s) after %d of %d batches — the next run continues", label, reason, i, len(batches)))
			return false
		}
		result, err := s.client.syncTasks(ctx, p.ID, ditSyncRequest{TaskList: p.TaskList, Tasks: batch})
		if err != nil {
			pr.Status, pr.Reason = "failed", "batch_rejected"
			s.res.FailedProjects++
			// Batches are idempotent: whatever landed before this one stays, and the
			// next run completes the rest.
			s.fail(fmt.Sprintf("%s: batch %d of %d: %s; remaining batches of this project skipped%s",
				label, i+1, len(batches), err.Error(), authHint(err)))
			log.Error("deliverit batch rejected", "batch", i+1, "err", s.mask(err.Error()))
			return isDeliverITAuthError(err)
		}
		// Counted per accepted batch, into the stage totals too: if a later batch
		// fails, these changes are already in DeliverIT's database.
		pr.Created += result.Created
		pr.Renamed += result.Renamed
		pr.Unchanged += result.Unchanged
		s.res.Created += result.Created
		s.res.Renamed += result.Renamed
		s.res.Unchanged += result.Unchanged
		for _, skipped := range result.Skipped {
			pr.Skipped++
			s.skipTask(skipped.Reason.String())
			// Fixed by a human in the source (rename), so it is reported on EVERY run.
			s.warn(fmt.Sprintf("%s: DeliverIT skipped task %s %q: %s", label, skipped.ExternalID, skipped.Name, skipped.Reason))
		}
	}

	pr.Status = "synced"
	s.res.SyncedProjects++
	log.Info("deliverit project synced", "tasks", pr.Tasks, "batches", pr.Batches, "created", pr.Created,
		"renamed", pr.Renamed, "unchanged", pr.Unchanged, "skipped", pr.Skipped)
	return false
}

// sourceTasks reads the linked list. A skip (unknown source, no token) is a
// warning; a read failure is an error of this project only.
func (s *deliverITStage) sourceTasks(ctx context.Context, p ditLinkedProject) (tasks []ditSyncTask, skipReason, skipDetail string, err error) {
	listID := p.TaskList.ExternalID
	switch p.TaskList.Source {
	case taskSourceClickUp:
		if s.cfg.ClickUpToken == "" {
			return nil, "missing_source_token", "CLICKUP_TOKEN is not set", nil
		}
		if !validSourceListID(listID) {
			return nil, "", "", fmt.Errorf("malformed ClickUp list id %q", listID)
		}
		tasks, err := s.clickUpTasks(ctx, listID)
		return tasks, "", "", err

	case taskSourceProductive:
		if s.cfg.ProductiveToken == "" || s.cfg.ProductiveOrgID == "" {
			return nil, "missing_source_token", "PRODUCTIVE_TOKEN / PRODUCTIVE_ORG_ID is not set", nil
		}
		if !validSourceListID(listID) {
			return nil, "", "", fmt.Errorf("malformed Productive task list id %q", listID)
		}
		pages, err := fetchProductivePages(ctx, s.productive, listID, "", s.cfg.MaxPages)
		if err != nil {
			return nil, "", "", err
		}
		out := make([]ditSyncTask, 0, len(pages.Data))
		for _, d := range pages.Data {
			out = append(out, ditSyncTask{ExternalID: d.ID, Name: d.Attributes.Title})
		}
		return out, "", "", nil

	default:
		return nil, "unknown_source", fmt.Sprintf("task source %s is not supported by this pcuintegrator build (update it)",
			p.TaskList.Source), nil
	}
}

// clickUpTasks reuses CLICKUP_LIST_ID's tasks from the first stage; any other list
// is read with the SAME filters (subtasks=true, include_closed per
// CLICKUP_INCLUDE_CLOSED), so a list yields the same set whichever way it is read.
func (s *deliverITStage) clickUpTasks(ctx context.Context, listID string) ([]ditSyncTask, error) {
	var tasks []Task
	if listID == s.cfg.ClickUpListID && s.read.attempted {
		if s.read.err != nil {
			// Its retries are already spent; a second read would burn the rate
			// budget and the deadline for the same answer.
			return nil, fmt.Errorf("list %s failed to read earlier in this run; not re-read: %v", listID, s.read.err)
		}
		tasks = s.read.tasks
	} else {
		cfg := s.cfg
		cfg.ClickUpListID = listID
		fetched, _, _, err := fetchClickUp(ctx, s.clickUp, cfg)
		if err != nil {
			return nil, err
		}
		tasks = fetched
	}
	out := make([]ditSyncTask, 0, len(tasks))
	for _, t := range tasks {
		// SourceTitle, not Title: Title is cut to Productive's 140 characters.
		out = append(out, ditSyncTask{ExternalID: t.ClickUpID, Name: t.SourceTitle})
	}
	return out, nil
}

// sourceListName reads the list's display name (ClickUp GET list/{id}, Productive
// GET task_lists/{id}). The id was validated by sourceTasks.
func (s *deliverITStage) sourceListName(ctx context.Context, p ditLinkedProject) (string, error) {
	listID := p.TaskList.ExternalID
	switch p.TaskList.Source {
	case taskSourceClickUp:
		var body struct {
			Name string `json:"name"`
		}
		err := s.clickUp.getJSON(ctx, "list/"+listID, &body)
		return body.Name, err
	case taskSourceProductive:
		var body struct {
			Data struct {
				Attributes struct {
					Name string `json:"name"`
				} `json:"attributes"`
			} `json:"data"`
		}
		err := s.productive.getJSON(ctx, "task_lists/"+listID, &body)
		return body.Data.Attributes.Name, err
	}
	return "", fmt.Errorf("unsupported source %s", p.TaskList.Source)
}

// prepare drops what the server's validator would reject — one such task would
// cost the WHOLE batch a 400 — removes NUL (a 500 for the whole batch), trims and
// caps names, and dedupes. Every drop is a warning: it is fixed in the source.
func (s *deliverITStage) prepare(label string, fetched []ditSyncTask, pr *deliverITProjectResult) []ditSyncTask {
	valid := make([]ditSyncTask, 0, len(fetched))
	for _, t := range fetched {
		// NUL is removed silently, like surrounding whitespace; only a name that
		// nothing is left of is worth a warning — and it must say why it is empty.
		hadNUL := strings.Contains(t.Name, "\x00")
		t.Name = normalizeDeliverITTaskName(t.Name)
		switch {
		case !validDeliverITTaskExternalID(t.ExternalID):
			pr.Skipped++
			s.skipTask("invalid_external_id")
			s.warn(fmt.Sprintf("%s: task %q skipped: id breaks DeliverIT's rule (1-100 of A-Z a-z 0-9 - _)", label, t.ExternalID))
		case t.Name == "":
			pr.Skipped++
			s.skipTask("empty_name")
			detail := "empty name in the source"
			if hadNUL {
				detail += " after removing NUL characters"
			}
			s.warn(fmt.Sprintf("%s: task %s skipped: %s", label, t.ExternalID, detail))
		default:
			valid = append(valid, t)
		}
	}
	out, duplicates := dedupeDeliverITTasks(valid)
	if duplicates > 0 {
		// The server rejects a batch with a repeated externalId (400); a list that
		// changes while being paged returns the same task twice.
		s.warn(fmt.Sprintf("%s: %d duplicate task id(s) in the source; kept the last read of each", label, duplicates))
	}
	return out
}

// dedupeDeliverITTasks keeps the LAST occurrence (the freshest read) at the
// position of the first.
func dedupeDeliverITTasks(tasks []ditSyncTask) ([]ditSyncTask, int) {
	index := make(map[string]int, len(tasks))
	out := make([]ditSyncTask, 0, len(tasks))
	duplicates := 0
	for _, t := range tasks {
		if i, seen := index[t.ExternalID]; seen {
			out[i] = t
			duplicates++
			continue
		}
		index[t.ExternalID] = len(out)
		out = append(out, t)
	}
	return out, duplicates
}

// chunkDeliverITTasks never returns zero batches: an empty list is sent as ONE
// empty batch, which marks the project as synced in DeliverIT.
func chunkDeliverITTasks(tasks []ditSyncTask, size int) [][]ditSyncTask {
	if len(tasks) == 0 {
		return [][]ditSyncTask{{}}
	}
	batches := make([][]ditSyncTask, 0, (len(tasks)+size-1)/size)
	for start := 0; start < len(tasks); start += size {
		batches = append(batches, tasks[start:min(start+size, len(tasks))])
	}
	return batches
}

// validSourceListID mirrors DeliverIT's TaskListExternalId: \A[1-9][0-9]{0,19}\z.
// The id goes into a URL path (ClickUp) or query (Productive), where "/", "?", "&"
// or ".." would change the request; escaping alone does not protect that.
func validSourceListID(id string) bool {
	if len(id) == 0 || len(id) > 20 || id[0] == '0' {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

func (s *deliverITStage) skipTask(reason string) {
	s.res.Skipped++
	if s.res.SkippedReasons == nil {
		s.res.SkippedReasons = map[string]int{}
	}
	s.res.SkippedReasons[reason]++
}

func (s *deliverITStage) warn(msg string) {
	msg = s.mask(msg)
	s.res.Warnings = append(s.res.Warnings, msg)
	s.log.Warn(msg)
}

func (s *deliverITStage) fail(msg string) {
	msg = s.mask(msg)
	s.res.Errors = append(s.res.Errors, msg)
	s.log.Error(msg)
}

// mask is defence in depth on top of the client's own masking: nothing that
// reaches the result or the log may carry the key.
func (s *deliverITStage) mask(msg string) string {
	if key := s.cfg.DeliverITAPIKey; key != "" {
		msg = strings.ReplaceAll(msg, key, deliverITKeyLabel(key))
	}
	return msg
}

func authHint(err error) string {
	if !isDeliverITAuthError(err) {
		return ""
	}
	return " — DeliverIT rejected the API key: generate a new one in DeliverIT → /integracje and " +
		"`npx wrangler secret put DELIVERIT_API_KEY`, then revoke the old one"
}

func timeLeft(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline), true
}

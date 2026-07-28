package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

type actionKind int

const (
	actionCreate actionKind = iota
	actionUpdate
	actionSkip
	actionConflict // several Productive tasks claim this ClickUp id — never written
)

func (k actionKind) String() string {
	switch k {
	case actionCreate:
		return "create"
	case actionUpdate:
		return "update"
	case actionConflict:
		return "conflict"
	default:
		return "skip"
	}
}

type action struct {
	Task         Task
	Kind         actionKind
	ProductiveID string   // "" for create
	Reasons      []string // "new" | "title" | "status" | "tags" | "estimate" | "reparent"

	// Parent resolution is split because a parent created in THIS run has no
	// Productive id at plan time. At most one of these is set.
	ParentProductiveID string // parent already lives in Productive
	ParentClickUpID    string // parent is created earlier in this same run
	FlatParent         string // parent unresolvable at all -> create flat + warn
}

type plan struct {
	Actions       []action
	Warnings      []string
	Reasons       map[string]int
	Creates       int
	Updates       int
	Skips         int
	Conflicts     int
	DedupeDropped int
}

// Plan is PURE: no I/O, no clock, no randomness, no map-order dependence. All the
// reconcile semantics live here, which is what makes the dry-run, the circuit
// breakers and the golden tests possible.
func Plan(clickUp []Task, snap productiveSnapshot, cfg Config) plan {
	p := plan{Reasons: map[string]int{}}

	withKey := make([]Task, 0, len(clickUp))
	for _, t := range clickUp {
		if t.ClickUpID == "" {
			continue // parity: tasks without an id are not synced
		}
		withKey = append(withKey, t)
	}

	deduped, dropped := dedupeByID(withKey)
	p.DedupeDropped = dropped
	if dropped > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"clickup: %d duplicate task id(s) in the fetched pages; kept the first of each", dropped))
	}

	ordered := orderParentsFirst(deduped)

	// Which ClickUp ids this run will have a Productive id for, and in what order.
	inRun := make(map[string]int, len(ordered))
	for i, t := range ordered {
		inRun[t.ClickUpID] = i
	}

	for i, t := range ordered {
		if ids, bad := snap.Conflicted[t.ClickUpID]; bad {
			p.Actions = append(p.Actions, action{Task: t, Kind: actionConflict})
			p.Conflicts++
			p.Reasons["conflict"]++
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"clickup %s: %d Productive tasks claim this id (%v); skipped entirely — resolve by hand",
				t.ClickUpID, len(ids), ids))
			continue
		}

		a := action{Task: t}
		resolveParent(&a, t, i, inRun, snap, &p)

		existing, found := snap.ByClickUpID[t.ClickUpID]
		if !found {
			a.Kind = actionCreate
			a.Reasons = []string{"new"}
			p.Creates++
			p.Reasons["new"]++
			p.Actions = append(p.Actions, a)
			continue
		}

		a.ProductiveID = existing.ProductiveID
		reasons := diffReasons(t, existing.Task, cfg.EstimateMode)

		// A child first created flat (parent outside the synced set) stays flat
		// forever, because PATCH deliberately omits parent_task. Always report it;
		// only act on it when explicitly allowed, since re-parenting would also
		// move tasks that people moved on purpose.
		wantParent := a.ParentProductiveID
		if wantParent != "" && existing.ParentTaskID != wantParent {
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"clickup %s (productive %s): parent should be %s but is %q — reparent_needed",
				t.ClickUpID, existing.ProductiveID, wantParent, existing.ParentTaskID))
			if cfg.AllowReparent {
				reasons = append(reasons, "reparent")
			}
		}

		if len(reasons) == 0 {
			a.Kind = actionSkip
			p.Skips++
			p.Actions = append(p.Actions, a)
			continue
		}

		a.Kind = actionUpdate
		a.Reasons = reasons
		p.Updates++
		for _, r := range reasons {
			p.Reasons[r]++
		}
		p.Actions = append(p.Actions, a)
	}

	return p
}

// resolveParent picks exactly one of the three parent fields (or none).
func resolveParent(a *action, t Task, idx int, inRun map[string]int, snap productiveSnapshot, p *plan) {
	parent := t.ClickUpParentID
	if parent == "" {
		return
	}
	if _, bad := snap.Conflicted[parent]; bad {
		a.FlatParent = parent
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"clickup %s: parent %s is in conflict; creating flat", t.ClickUpID, parent))
		return
	}
	if pe, ok := snap.ByClickUpID[parent]; ok {
		a.ParentProductiveID = pe.ProductiveID
		return
	}
	if pidx, ok := inRun[parent]; ok && pidx < idx {
		a.ParentClickUpID = parent
		return
	}
	a.FlatParent = parent
	p.Warnings = append(p.Warnings, fmt.Sprintf(
		"clickup %s: parent %s is outside the synced set; creating flat", t.ClickUpID, parent))
}

// dedupeByID keeps the first occurrence, matching GroupBy(id).Select(g => g.First()).
func dedupeByID(tasks []Task) ([]Task, int) {
	seen := make(map[string]bool, len(tasks))
	out := make([]Task, 0, len(tasks))
	dropped := 0
	for _, t := range tasks {
		if seen[t.ClickUpID] {
			dropped++
			continue
		}
		seen[t.ClickUpID] = true
		out = append(out, t)
	}
	return out, dropped
}

// orderParentsFirst sorts by depth in the subtask tree so a parent is always
// written before its children.
//
// This replaces .NET's queue + deferred-list + progress-flag loop. The two agree
// for acyclic input; a cycle saturates the guard at 64 and lands in the "create
// flat" bucket, which is also where the .NET progress loop leaves it.
//
// Two Go-specific requirements: the sort must be STABLE (Go's sort.Slice is not),
// and depths must be precomputed into a map rather than recomputed inside the
// comparator, which would otherwise be both slow and inconsistent.
func orderParentsFirst(tasks []Task) []Task {
	byID := make(map[string]Task, len(tasks))
	for _, t := range tasks {
		byID[t.ClickUpID] = t
	}

	depth := make(map[string]int, len(tasks))
	for _, t := range tasks {
		d := 0
		cur := t
		for guard := 0; cur.ClickUpParentID != "" && guard < 64; guard++ {
			parent, ok := byID[cur.ClickUpParentID]
			if !ok {
				break
			}
			d++
			cur = parent
		}
		depth[t.ClickUpID] = d
	}

	out := make([]Task, len(tasks))
	copy(out, tasks)
	sort.SliceStable(out, func(i, j int) bool {
		return depth[out[i].ClickUpID] < depth[out[j].ClickUpID]
	})
	return out
}

// --- result ---

type actionSummary struct {
	ClickUpID    string          `json:"clickup_id"`
	Kind         string          `json:"kind"`
	ProductiveID string          `json:"productive_id,omitempty"`
	Reasons      []string        `json:"reasons,omitempty"`
	Parent       string          `json:"parent,omitempty"`
	Body         json.RawMessage `json:"body,omitempty"`
}

type plannedCounts struct {
	Create   int `json:"create"`
	Update   int `json:"update"`
	Skip     int `json:"skip"`
	Conflict int `json:"conflict"`
}

type Result struct {
	RunID      string    `json:"run_id"`
	DryRun     bool      `json:"dry_run"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	DurationMs int64     `json:"duration_ms"`
	Phase      string    `json:"phase"`

	ClickUp    clickUpFetchStats    `json:"clickup"`
	Productive productiveFetchStats `json:"productive"`

	Planned plannedCounts `json:"planned"`

	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
	Ambiguous int `json:"ambiguous"` // writes whose outcome is unknown; next run reconciles

	// Reasons is the histogram that makes eternal PATCH churn visible instead of
	// letting it hide behind an "updated: 412" that looks like normal traffic.
	Reasons map[string]int `json:"reasons,omitempty"`

	Actions  []actionSummary `json:"actions,omitempty"` // dry-run / explain only
	Warnings []string        `json:"warnings,omitempty"`
	Errors   []string        `json:"errors,omitempty"`
	Aborted  string          `json:"aborted,omitempty"`

	ClickUpAPI    clientStats `json:"clickup_api"`
	ProductiveAPI clientStats `json:"productive_api"`
}

type RunOptions struct {
	RunID      string // caller-supplied so /status can name the in-flight run
	DryRun     bool
	Explain    bool
	MaxCreates *int // per-request override for the deliberate first bulk import
	MaxWrites  *int
}

func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// Run is the whole pipeline: read both sides, plan, check the breakers, write.
func Run(ctx context.Context, cfg Config, opts RunOptions, log *slog.Logger) *Result {
	runID := opts.RunID
	if runID == "" {
		runID = newRunID()
	}
	res := &Result{
		RunID:     runID,
		DryRun:    opts.DryRun,
		StartedAt: time.Now().UTC(),
		Phase:     "starting",
		Reasons:   map[string]int{},
	}
	log = log.With("run_id", res.RunID, "dry_run", opts.DryRun)

	// The ClickUp client is ALWAYS read-only: this integration is one-way by
	// definition, so a write to ClickUp is a bug, not a configuration choice.
	cu := newAPIClient("clickup", cfg.ClickUpBaseURL, clickUpHeader(cfg), cfg.ClickUpRPS, cfg.ClickUpRPS, cfg, true, log)
	pr := newAPIClient("productive", cfg.ProductiveBaseURL, productiveHeader(cfg),
		cfg.ProductiveRPS, cfg.ProductiveBurstRPS, cfg, opts.DryRun, log)

	defer func() {
		res.FinishedAt = time.Now().UTC()
		res.DurationMs = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
		res.ClickUpAPI = cu.Stats()
		res.ProductiveAPI = pr.Stats()
	}()

	res.Phase = "fetch_productive"
	log.Info("fetching productive")
	snap, err := fetchProductive(ctx, pr, cfg)
	res.Productive = snap.Stats
	res.Warnings = append(res.Warnings, snap.Warnings...)
	if err != nil {
		res.Phase, res.Aborted = "failed", "fetch_productive"
		res.Errors = append(res.Errors, err.Error())
		log.Error("productive fetch failed; nothing written", "err", err)
		return res
	}
	log.Info("productive fetched", "tasks", snap.Stats.Tasks, "keyed", len(snap.ByClickUpID))

	res.Phase = "fetch_clickup"
	clickUpTasks, cuStats, cuWarnings, err := fetchClickUp(ctx, cu, cfg)
	res.ClickUp = cuStats
	res.Warnings = append(res.Warnings, cuWarnings...)
	if err != nil {
		res.Phase, res.Aborted = "failed", "fetch_clickup"
		res.Errors = append(res.Errors, err.Error())
		log.Error("clickup fetch failed; nothing written", "err", err)
		return res
	}
	log.Info("clickup fetched", "tasks", cuStats.Tasks, "subtasks", cuStats.Subtasks)

	res.Phase = "plan"
	p := Plan(clickUpTasks, snap, cfg)
	res.Planned = plannedCounts{Create: p.Creates, Update: p.Updates, Skip: p.Skips, Conflict: p.Conflicts}
	res.Reasons = p.Reasons
	res.Warnings = append(res.Warnings, p.Warnings...)
	res.Skipped = p.Skips
	log.Info("planned", "create", p.Creates, "update", p.Updates, "skip", p.Skips,
		"conflict", p.Conflicts, "reasons", p.Reasons)

	maxCreates, maxWrites := cfg.MaxCreates, cfg.MaxWrites
	if opts.MaxCreates != nil {
		maxCreates = *opts.MaxCreates
	}
	if opts.MaxWrites != nil {
		maxWrites = *opts.MaxWrites
	}

	// Circuit breakers. This runs unattended behind a 4-hourly trigger; a mapping
	// regression or a partially-read Productive snapshot must not be allowed to
	// rewrite the whole list before anyone notices.
	if p.Creates > maxCreates {
		res.Phase, res.Aborted = "aborted", "max_creates_exceeded"
		res.Errors = append(res.Errors, fmt.Sprintf(
			"planned %d creates > MAX_CREATES=%d; nothing written. Re-run with ?max_creates=%d if this is intended",
			p.Creates, maxCreates, p.Creates))
		res.Actions = summarize(p, snap, cfg, true)
		log.Error("create breaker tripped", "planned", p.Creates, "limit", maxCreates)
		return res
	}
	if p.Creates+p.Updates > maxWrites {
		res.Phase, res.Aborted = "aborted", "max_writes_exceeded"
		res.Errors = append(res.Errors, fmt.Sprintf(
			"planned %d writes > MAX_WRITES=%d; nothing written. Check the `reasons` histogram before raising it",
			p.Creates+p.Updates, maxWrites))
		res.Actions = summarize(p, snap, cfg, true)
		log.Error("write breaker tripped", "planned", p.Creates+p.Updates, "limit", maxWrites)
		return res
	}

	if opts.DryRun {
		res.Phase = "done"
		res.Actions = summarize(p, snap, cfg, true)
		log.Info("dry run complete; no writes issued")
		return res
	}

	res.Phase = "execute"
	Execute(ctx, pr, p, snap, cfg, res, log)
	if opts.Explain {
		res.Actions = summarize(p, snap, cfg, true)
	}
	if res.Aborted == "" {
		res.Phase = "done"
	}
	log.Info("run complete", "created", res.Created, "updated", res.Updated,
		"skipped", res.Skipped, "failed", res.Failed, "ambiguous", res.Ambiguous)
	return res
}

// Execute walks the plan in order, resolving parents created earlier in this run.
func Execute(ctx context.Context, pr *apiClient, p plan, snap productiveSnapshot, cfg Config, res *Result, log *slog.Logger) {
	// Seeded with everything already in Productive, extended after each successful
	// create so subtasks can reference their parent.
	idMap := make(map[string]string, len(snap.ByClickUpID))
	for k, v := range snap.ByClickUpID {
		idMap[k] = v.ProductiveID
	}

	for _, a := range p.Actions {
		if err := ctx.Err(); err != nil {
			res.Aborted = "deadline"
			res.Errors = append(res.Errors, fmt.Sprintf("stopped after %d writes: %v", res.Created+res.Updated, err))
			log.Warn("deadline reached mid-run; remaining work left for the next run",
				"created", res.Created, "updated", res.Updated)
			return
		}
		if isShuttingDown() {
			res.Aborted = "sigterm"
			res.Errors = append(res.Errors, fmt.Sprintf("SIGTERM after %d writes", res.Created+res.Updated))
			log.Warn("SIGTERM mid-run; stopping cleanly", "created", res.Created, "updated", res.Updated)
			return
		}

		switch a.Kind {
		case actionSkip, actionConflict:
			continue

		case actionCreate:
			parent := a.ParentProductiveID
			if parent == "" && a.ParentClickUpID != "" {
				if pid, ok := idMap[a.ParentClickUpID]; ok {
					parent = pid
				} else {
					res.Warnings = append(res.Warnings, fmt.Sprintf(
						"clickup %s: parent %s was not created in this run; creating flat",
						a.Task.ClickUpID, a.ParentClickUpID))
				}
			}
			body, err := buildBody(a.Task, parent, nil, cfg)
			if err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("clickup %s: build body: %v", a.Task.ClickUpID, err))
				continue
			}
			newID, err := createTask(ctx, pr, body)
			if err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("clickup %s: create: %v", a.Task.ClickUpID, err))
				// Not retried on an ambiguous failure: the task may exist. Report it
				// so the count is honest, and let the next run find it by its
				// ClickUp custom field and PATCH instead of duplicating.
				if isAmbiguousWriteFailure(err) {
					res.Ambiguous++
				}
				// Ensure children degrade to flat rather than referencing a task
				// that may not exist.
				delete(idMap, a.Task.ClickUpID)
				if errors.Is(err, errRateLimited) {
					res.Aborted = "rate_limited"
					return
				}
				continue
			}
			idMap[a.Task.ClickUpID] = newID
			res.Created++
			log.Info("created", "clickup_id", a.Task.ClickUpID, "productive_id", newID, "parent", parent)

		case actionUpdate:
			existing, ok := snap.ByClickUpID[a.Task.ClickUpID]
			if !ok {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf(
					"clickup %s: planned update but no existing task (internal inconsistency)", a.Task.ClickUpID))
				continue
			}
			parent := a.ParentProductiveID
			if parent == "" && a.ParentClickUpID != "" {
				parent = idMap[a.ParentClickUpID]
			}
			body, err := buildBody(a.Task, parent, &existing, cfg)
			if err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("clickup %s: build body: %v", a.Task.ClickUpID, err))
				continue
			}
			if err := updateTask(ctx, pr, existing.ProductiveID, body); err != nil {
				res.Failed++
				res.Errors = append(res.Errors, fmt.Sprintf("clickup %s: update: %v", a.Task.ClickUpID, err))
				if errors.Is(err, errRateLimited) {
					res.Aborted = "rate_limited"
					return
				}
				continue
			}
			res.Updated++
			log.Info("updated", "clickup_id", a.Task.ClickUpID,
				"productive_id", existing.ProductiveID, "reasons", a.Reasons)
		}
	}
}

// isAmbiguousWriteFailure reports whether a create may have landed server-side.
// A definite rejection (4xx that is not 429) did not create anything.
func isAmbiguousWriteFailure(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.Status >= 500
	}
	return !errors.Is(err, errReadOnly)
}

// summarize renders the plan for the dry-run / explain output, including the exact
// request body that would be sent.
func summarize(p plan, snap productiveSnapshot, cfg Config, withBody bool) []actionSummary {
	out := make([]actionSummary, 0, len(p.Actions))
	for _, a := range p.Actions {
		s := actionSummary{
			ClickUpID:    a.Task.ClickUpID,
			Kind:         a.Kind.String(),
			ProductiveID: a.ProductiveID,
			Reasons:      a.Reasons,
		}
		switch {
		case a.ParentProductiveID != "":
			s.Parent = a.ParentProductiveID
		case a.ParentClickUpID != "":
			s.Parent = "pending:clickup:" + a.ParentClickUpID
		case a.FlatParent != "":
			s.Parent = "unresolved:clickup:" + a.FlatParent
		}

		if withBody && (a.Kind == actionCreate || a.Kind == actionUpdate) {
			var existing *existingTask
			if a.Kind == actionUpdate {
				if e, ok := snap.ByClickUpID[a.Task.ClickUpID]; ok {
					existing = &e
				}
			}
			if body, err := buildBody(a.Task, a.ParentProductiveID, existing, cfg); err == nil {
				s.Body = body
			}
		}
		out = append(out, s)
	}
	return out
}

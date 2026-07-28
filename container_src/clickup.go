package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// --- DTOs ---
//
// Deliberately narrow: only the fields the sync reads. ClickUp pages carry the
// full type_config.options array for every custom field on every task, which is
// the bulk of the payload; projecting into these structs and discarding the rest
// keeps peak memory well under the container's limit.

type clickUpTasksPage struct {
	Tasks []clickUpTask `json:"tasks"`
	// Present in ClickUp's own schema/example for this endpoint but not in a
	// `required` array, hence a pointer. Kept for diagnostics only — the fetch
	// loop terminates on an empty page, matching .NET.
	LastPage *bool `json:"last_page"`
}

type clickUpTask struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Status       clickUpStatus        `json:"status"`
	Parent       *string              `json:"parent"`
	TimeEstimate flexInt              `json:"time_estimate"` // milliseconds
	Tags         []clickUpTag         `json:"tags"`
	CustomFields []clickUpCustomField `json:"custom_fields"`
}

type clickUpStatus struct {
	Name string `json:"status"`
	Type string `json:"type"` // open | custom | closed | done
}

type clickUpTag struct {
	Name string `json:"name"`
}

type clickUpCustomField struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	TypeConfig struct {
		Options []clickUpDropdownOption `json:"options"`
	} `json:"type_config"`
	// Polymorphic across the 18 field types and untyped in the reference schema.
	Value json.RawMessage `json:"value"`
}

type clickUpDropdownOption struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	OrderIndex flexInt `json:"orderindex"` // appears as both 0 and "0" in official examples
}

// clickUpFetchStats doubles as the diagnostic the cutover needs: the
// name x type histogram answers whether "wydane" / "gotowe do wydania" /
// "zawieszone" are *closed*-type statuses, in which case they are invisible to
// this endpoint (include_closed defaults to false) and the whole Done branch of
// the mapping has never fired in production.
type clickUpFetchStats struct {
	Tasks       int            `json:"tasks"`
	Pages       int            `json:"pages"`
	Subtasks    int            `json:"subtasks"`
	StatusTypes map[string]int `json:"status_types,omitempty"` // "name|type" => count
}

// fetchClickUp pages through the list. `page` is 0-based and the page size is
// fixed at 100 by ClickUp; there is no size parameter.
//
// Termination is on an empty `tasks` array, matching .NET. That costs one extra
// request per run versus breaking on `last_page`, and keeping it preserves parity
// exactly; MaxPages is a pure safety guard against a pathological non-terminating
// response, not a behaviour change.
func fetchClickUp(ctx context.Context, c *apiClient, cfg Config) ([]Task, clickUpFetchStats, []string, error) {
	stats := clickUpFetchStats{StatusTypes: map[string]int{}}
	var (
		tasks    []Task
		warnings []string
	)

	for page := 0; page < cfg.MaxPages; page++ {
		path := fmt.Sprintf("list/%s/task?page=%d&subtasks=true", cfg.ClickUpListID, page)
		if cfg.IncludeClosed {
			path += "&include_closed=true"
		}

		var body clickUpTasksPage
		if err := c.getJSON(ctx, path, &body); err != nil {
			// Hard abort. A read that fails without aborting looks identical to
			// "ClickUp is empty", which would silently sync nothing — less
			// dangerous than the Productive equivalent, but still a lie in the
			// summary.
			return nil, stats, warnings, fmt.Errorf("clickup: %w", err)
		}
		stats.Pages++

		if len(body.Tasks) == 0 {
			return tasks, stats, warnings, nil
		}

		for _, raw := range body.Tasks {
			t, warns := toTask(raw, cfg)
			warnings = append(warnings, warns...)
			tasks = append(tasks, t)
			stats.Tasks++
			if t.ClickUpParentID != "" {
				stats.Subtasks++
			}
			stats.StatusTypes[raw.Status.Name+"|"+raw.Status.Type]++
		}
	}

	return tasks, stats, warnings, fmt.Errorf("clickup: exceeded MAX_PAGES=%d, refusing to keep paging", cfg.MaxPages)
}

// toTask mirrors ClickUpTask.ToUniversalTask().
func toTask(raw clickUpTask, cfg Config) (Task, []string) {
	status := StatusOpen
	if cfg.ClickUpDoneStatuses[raw.Status.Name] {
		status = StatusDone
	}

	tags := make([]string, 0, len(raw.Tags))
	for _, tg := range raw.Tags {
		tags = append(tags, tg.Name)
	}

	estimate, warnings := resolveEstimate(raw, cfg)

	parent := ""
	if raw.Parent != nil {
		parent = strings.TrimSpace(*raw.Parent)
	}

	return Task{
		ClickUpID: raw.ID,
		// Truncated here, not in buildBody: Productive stores the truncated title,
		// so comparing the full ClickUp title against it would differ on every run.
		Title:           truncateTitle(strings.TrimSpace(raw.Name), cfg.TitleMaxRunes),
		Status:          status,
		Tags:            normalizeTags(tags),
		InitialEstimate: estimate,
		ClickUpParentID: parent,
	}, warnings
}

// resolveEstimate mirrors MappingExtensions.ResolveInitialEstimateMinutes:
// time_estimate (ms -> minutes, integer truncation) wins; otherwise the SUM of the
// selected labels across the "Szybka wycena" drop-downs (API + FRONT).
func resolveEstimate(raw clickUpTask, cfg Config) (*int, []string) {
	if raw.TimeEstimate.Set && raw.TimeEstimate.V > 0 {
		return intPtr(int(raw.TimeEstimate.V / 60_000)), nil
	}

	var (
		warnings []string
		sum      int
		found    bool
	)
	for _, f := range raw.CustomFields {
		if !slices.Contains(cfg.EstimateFieldIDs, f.ID) || f.Type != "drop_down" {
			continue
		}
		opt := resolveSelectedOption(f)
		if opt == nil {
			continue
		}
		minutes, ok := cfg.EstimateLabelMinutes[opt.Name]
		if !ok {
			// Parity: .NET silently skips an unmapped label (the table is missing
			// "12h - 16h" and "32h - 40h"). Surfacing it as a warning is the only
			// change — the value still does not contribute.
			warnings = append(warnings, fmt.Sprintf(
				"clickup %s: unmapped estimate label %q on field %s", raw.ID, opt.Name, f.ID))
			continue
		}
		sum += minutes
		found = true
	}
	if !found {
		return nil, warnings
	}
	return intPtr(sum), warnings
}

// resolveSelectedOption mirrors MappingExtensions.ResolveSelectedOption exactly:
// a numeric `value` indexes options[].orderindex; a string `value` matches
// options[].id first and falls back to being parsed as an orderindex.
//
// Note the asymmetry ClickUp documents: writing a drop-down takes the option UUID,
// but reading one back returns the orderindex integer. Matching on id alone would
// resolve nothing.
func resolveSelectedOption(f clickUpCustomField) *clickUpDropdownOption {
	opts := f.TypeConfig.Options
	if len(opts) == 0 {
		return nil
	}
	v := bytes.TrimSpace(f.Value)
	if len(v) == 0 || bytes.Equal(v, []byte("null")) {
		return nil
	}

	if v[0] == '"' {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil
		}
		if s = strings.TrimSpace(s); s == "" {
			return nil
		}
		for i := range opts {
			if opts[i].ID == s {
				return &opts[i]
			}
		}
		n, err := parseLooseInt(s)
		if err != nil {
			return nil
		}
		return optionByOrderIndex(opts, n)
	}

	if v[0] == '[' || v[0] == '{' || v[0] == 't' || v[0] == 'f' {
		return nil
	}
	n, err := parseLooseInt(string(v))
	if err != nil {
		return nil
	}
	return optionByOrderIndex(opts, n)
}

func optionByOrderIndex(opts []clickUpDropdownOption, idx int64) *clickUpDropdownOption {
	for i := range opts {
		if opts[i].OrderIndex.Set && opts[i].OrderIndex.V == idx {
			return &opts[i]
		}
	}
	return nil
}

func clickUpHeader(cfg Config) http.Header {
	h := http.Header{}
	// ClickUp takes the raw pk_… token; no "Bearer" prefix.
	h.Set("Authorization", cfg.ClickUpToken)
	h.Set("Accept", "application/json")
	return h
}

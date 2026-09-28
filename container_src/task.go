package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type Status int

const (
	StatusOpen Status = iota
	StatusDone
)

func (s Status) String() string {
	if s == StatusDone {
		return "done"
	}
	return "open"
}

func (s Status) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// Task is the normalized shape both systems map into — the Go equivalent of
// UniversalTask. ClickUpParentID is deliberately EXCLUDED from equalForSync,
// exactly as the .NET record excludes it from Equals/GetHashCode: parenting is
// only ever set on create, never on update.
type Task struct {
	ClickUpID       string   `json:"clickup_id"`
	Title           string   `json:"title"`
	Status          Status   `json:"status"`
	Tags            []string `json:"tags"`              // normalized: trimmed, non-empty, deduped, SORTED
	InitialEstimate *int     `json:"initial_estimate"`  // minutes; nil == none
	ClickUpParentID string   `json:"clickup_parent_id"` // "" == root

	// SourceTitle is the trimmed ClickUp name BEFORE the cut to Productive's 140
	// characters; the DeliverIT fanout sends it (DeliverIT keeps 300). Like the
	// parent, it is outside diffReasons, so it can never cause a PATCH.
	SourceTitle string `json:"-"`
}

// equalForSync decides whether a PATCH is needed. It must be a free function:
//
//   - `a == b` on a struct containing *int compares POINTERS, so it would report
//     every task as changed and PATCH the entire list on every run.
//   - reflect.DeepEqual would follow the pointer correctly but also pull in
//     ClickUpParentID (breaking parity) and treat a nil slice as != an empty one.
func equalForSync(clickUp, existing Task, mode EstimateMode) bool {
	return len(diffReasons(clickUp, existing, mode)) == 0
}

// diffReasons returns why the two differ, in a fixed order so output is
// deterministic and the run summary's histogram is stable. Empty == equal.
func diffReasons(clickUp, existing Task, mode EstimateMode) []string {
	var out []string
	// Ordinal string comparison, matching StringComparison.Ordinal in .NET.
	if clickUp.ClickUpID != existing.ClickUpID {
		out = append(out, "clickup_id")
	}
	if clickUp.Title != existing.Title {
		out = append(out, "title")
	}
	if clickUp.Status != existing.Status {
		out = append(out, "status")
	}
	if !tagsEqual(clickUp.Tags, existing.Tags) {
		out = append(out, "tags")
	}
	if !estimatesEqual(clickUp.InitialEstimate, existing.InitialEstimate, mode) {
		out = append(out, "estimate")
	}
	return out
}

// tagsEqual is set equality. Both sides go through normalizeTags (sorted, deduped,
// no empties), so this degenerates to a slice compare — and nil compares equal to
// an empty slice, which a naive DeepEqual would get wrong.
func tagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return slices.Equal(a, b)
}

// normalizeTags trims, drops empties, dedupes and SORTS.
//
// Sorting is a deliberate deviation: .NET joins a HashSet in arbitrary order, so
// the string stored in Productive today has nondeterministic ordering, which makes
// byte-comparable request bodies (and therefore golden tests) impossible. The cost
// is one one-time PATCH per task with more than one tag.
func normalizeTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}

// parseTagsField reads the ", "-joined Productive custom field back into tags.
//
// CHURN FIX #1: .NET does `clickUpTags?.Split(", ").ToHashSet()`, and in .NET
// "".Split(", ") returns [""] — a set holding one empty string. The ClickUp side
// produces an empty set, SetEquals is false, and every untagged task is therefore
// PATCHed on every single run, forever. Dropping empty segments fixes it, and
// changes no data: the tasks simply stop being rewritten.
func parseTagsField(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return normalizeTags(strings.Split(v, ","))
}

// formatTagsField is deterministic because normalizeTags sorted the input.
func formatTagsField(tags []string) string { return strings.Join(tags, ", ") }

// truncateTitle cuts the title down to what Productive accepts.
//
// Counting is by RUNE, not byte: Productive's limit is stated in characters, and
// Polish titles are full of multibyte characters — a byte-based cut would both
// truncate too early and risk splitting a character in half.
//
// Applied at mapping time so both sides of the comparison see the same string.
// If it were applied only when building the body, Productive would hold the
// truncated title, the ClickUp side would hold the full one, and the diff would
// never resolve — a PATCH on every run, forever.
func truncateTitle(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	// Leave room for the ellipsis so the result is exactly maxRunes and it is
	// obvious to a human that the title was cut.
	cut := strings.TrimRight(string(runes[:maxRunes-1]), " \t\n")
	return cut + "…"
}

// normEstimate collapses nil and 0 to the same thing.
//
// CHURN FIX #3: ClickUp's resolver returns ms/60000 with integer truncation, so a
// 30-second estimate yields 0 minutes. Productive stores that as 0 or null
// depending on how it got there, and `int? == int?` across those two is false =>
// PATCH forever.
func normEstimate(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// estimatesEqual compares the ClickUp value against the Productive one.
//
// CHURN FIX #2: the write body omits `initial_estimate` when it is nil (parity
// with JsonIgnoreCondition.WhenWritingNull), so a "ClickUp has none, Productive
// has 480" diff can never be resolved by writing — the PATCH omits the field,
// Productive is unchanged, and the next run sees the same diff. Forever.
//
// EstimateIgnoreNil (default) declares that case equal: no writes, no churn, and
// estimates people typed into Productive by hand survive. EstimateClearNil instead
// sends an explicit null, which converges but destroys those manual values.
func estimatesEqual(clickUp, existing *int, mode EstimateMode) bool {
	cu, ex := normEstimate(clickUp), normEstimate(existing)
	if mode == EstimateIgnoreNil && cu == 0 && ex != 0 {
		return true
	}
	return cu == ex
}

// wantsEstimateClear reports whether buildBody must emit an explicit null.
func wantsEstimateClear(clickUp, existing *int, mode EstimateMode) bool {
	return mode == EstimateClearNil && normEstimate(clickUp) == 0 && normEstimate(existing) != 0
}

// flexInt decodes an integer that the ClickUp API delivers inconsistently.
//
// Their own docs show `orderindex` as both "0" and 0, and `time_estimate` is
// declared variously string|null, integer and number|null across the spec. A plain
// *int64 field panics the whole page decode on the shape it did not expect — which
// in .NET silently kills the run and retries 4 hours later.
type flexInt struct {
	V   int64
	Set bool
}

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = flexInt{}
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s = strings.TrimSpace(s); s == "" {
			*f = flexInt{}
			return nil
		}
		n, err := parseLooseInt(s)
		if err != nil {
			return fmt.Errorf("flexInt: %q: %w", s, err)
		}
		*f = flexInt{V: n, Set: true}
		return nil
	}
	if b[0] == 't' || b[0] == 'f' {
		// Not an integer in any reading; treat as absent rather than failing the page.
		*f = flexInt{}
		return nil
	}
	n, err := parseLooseInt(string(b))
	if err != nil {
		return fmt.Errorf("flexInt: %s: %w", b, err)
	}
	*f = flexInt{V: n, Set: true}
	return nil
}

// parseLooseInt accepts "123" and "123.0" / 1.23e6 — JSON numbers have no integer
// type and ClickUp has been observed emitting both.
func parseLooseInt(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(fl), nil
}

func intPtr(v int) *int { return &v }

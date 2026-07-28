package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// szybkaWycena builds a "Szybka wycena" drop_down field. `value` is inserted
// verbatim so tests can use every shape ClickUp has been observed to emit.
func szybkaWycena(id, value string, orderIndexAsString bool) map[string]any {
	mk := func(optID, name string, idx int) map[string]any {
		o := map[string]any{"id": optID, "name": name, "color": "#000"}
		if orderIndexAsString {
			o["orderindex"] = string(rune('0' + idx))
		} else {
			o["orderindex"] = idx
		}
		return o
	}
	f := map[string]any{
		"id":   id,
		"name": "Szybka wycena",
		"type": "drop_down",
		"type_config": map[string]any{
			"options": []any{
				mk("o0", "< 30m", 0),
				mk("o1", "1h - 2h", 1),
				mk("o2", "3h - 12h", 2),
				mk("o3", "16h - 32h", 3),
			},
		},
	}
	if value != "" {
		f["value"] = json.RawMessage(value)
	}
	return f
}

func decodeClickUpTask(t *testing.T, m map[string]any) clickUpTask {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out clickUpTask
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// ClickUp writes a drop-down by option UUID but reads it back as an orderindex
// integer. Resolving by id alone would find nothing.
func TestResolveSelectedOption(t *testing.T) {
	const apiField = "33afbaec-1fae-49be-9e9f-35eb789ef911"

	tests := []struct {
		name      string
		value     string
		asString  bool
		wantLabel string
	}{
		{"numeric orderindex", `1`, false, "1h - 2h"},
		{"numeric orderindex as string", `"1"`, false, "1h - 2h"},
		{"option uuid", `"o2"`, false, "3h - 12h"},
		{"orderindex zero", `0`, false, "< 30m"},
		{"options carry string orderindex", `2`, true, "3h - 12h"},
		{"null", `null`, false, ""},
		{"empty string", `""`, false, ""},
		{"absent", ``, false, ""},
		{"unknown uuid", `"nope"`, false, ""},
		{"unknown orderindex", `99`, false, ""},
		{"array value", `[]`, false, ""},
		{"object value", `{}`, false, ""},
		{"bool value", `true`, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := decodeClickUpTask(t, map[string]any{
				"id":            "cu-1",
				"name":          "x",
				"status":        map[string]any{"status": "open", "type": "open"},
				"custom_fields": []any{szybkaWycena(apiField, tc.value, tc.asString)},
			})
			opt := resolveSelectedOption(task.CustomFields[0])
			got := ""
			if opt != nil {
				got = opt.Name
			}
			if got != tc.wantLabel {
				t.Fatalf("resolved %q, want %q", got, tc.wantLabel)
			}
		})
	}
}

func TestResolveSelectedOptionNoOptions(t *testing.T) {
	task := decodeClickUpTask(t, map[string]any{
		"id":     "cu-1",
		"name":   "x",
		"status": map[string]any{"status": "open", "type": "open"},
		"custom_fields": []any{map[string]any{
			"id": "f1", "type": "drop_down", "value": 1,
			"type_config": map[string]any{"options": []any{}},
		}},
	})
	if opt := resolveSelectedOption(task.CustomFields[0]); opt != nil {
		t.Fatalf("expected nil, got %+v", opt)
	}
}

func TestResolveEstimate(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	apiField, frontField := cfg.EstimateFieldIDs[0], cfg.EstimateFieldIDs[1]

	tests := []struct {
		name         string
		task         map[string]any
		want         *int
		wantWarnings int
	}{
		{
			name: "time_estimate wins over drop-downs",
			task: map[string]any{
				"id": "a", "name": "a", "status": map[string]any{"status": "open"},
				"time_estimate": 5_400_000, // 90 min
				"custom_fields": []any{szybkaWycena(apiField, `2`, false)},
			},
			want: intPtr(90),
		},
		{
			name: "time_estimate as string",
			task: map[string]any{
				"id": "a", "name": "a", "status": map[string]any{"status": "open"},
				"time_estimate": "5400000",
			},
			want: intPtr(90),
		},
		{
			// ms/60000 truncates: 30 seconds becomes 0 minutes. Kept for parity.
			name: "sub-minute time_estimate truncates to zero",
			task: map[string]any{
				"id": "a", "name": "a", "status": map[string]any{"status": "open"},
				"time_estimate": 30_000,
			},
			want: intPtr(0),
		},
		{
			name: "zero time_estimate falls through to drop-downs",
			task: map[string]any{
				"id": "a", "name": "a", "status": map[string]any{"status": "open"},
				"time_estimate": 0,
				"custom_fields": []any{szybkaWycena(apiField, `1`, false)},
			},
			want: intPtr(90),
		},
		{
			name: "both drop-downs are SUMMED",
			task: map[string]any{
				"id": "a", "name": "a", "status": map[string]any{"status": "open"},
				"custom_fields": []any{
					szybkaWycena(apiField, `1`, false),   // 1h - 2h  = 90
					szybkaWycena(frontField, `2`, false), // 3h - 12h = 600
				},
			},
			want: intPtr(690),
		},
		{
			name: "no estimate anywhere",
			task: map[string]any{
				"id": "a", "name": "a", "status": map[string]any{"status": "open"},
			},
			want: nil,
		},
		{
			name: "unrelated drop-down field is ignored",
			task: map[string]any{
				"id": "a", "name": "a", "status": map[string]any{"status": "open"},
				"custom_fields": []any{szybkaWycena("some-other-field", `1`, false)},
			},
			want: nil,
		},
		{
			name: "orderindex 3 resolves to the 16h - 32h label",
			task: map[string]any{
				"id": "a", "name": "a", "status": map[string]any{"status": "open"},
				"custom_fields": []any{szybkaWycena(apiField, `3`, false)},
			},
			want: intPtr(1500),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := resolveEstimate(decodeClickUpTask(t, tc.task), cfg)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("want nil, got %d", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("want %d, got nil", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("want %d, got %d", *tc.want, *got)
			}
			if len(warns) != tc.wantWarnings {
				t.Fatalf("warnings = %v, want %d", warns, tc.wantWarnings)
			}
		})
	}
}

// The label table is missing "12h - 16h" and "32h - 40h" upstream; parity keeps
// them missing, and the only change is that the omission becomes visible.
func TestResolveEstimateUnmappedLabelWarns(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	cfg.EstimateLabelMinutes = map[string]int{"< 30m": 20} // "1h - 2h" now unmapped

	got, warns := resolveEstimate(decodeClickUpTask(t, map[string]any{
		"id": "a", "name": "a", "status": map[string]any{"status": "open"},
		"custom_fields": []any{szybkaWycena(cfg.EstimateFieldIDs[0], `1`, false)},
	}), cfg)

	if got != nil {
		t.Fatalf("unmapped label must not contribute, got %d", *got)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "unmapped estimate label") {
		t.Fatalf("expected one unmapped-label warning, got %v", warns)
	}
}

func TestToTask(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")

	raw := decodeClickUpTask(t, map[string]any{
		"id":     "cu-1",
		"name":   "   Trim me   ",
		"status": map[string]any{"status": "wydane", "type": "custom"},
		"parent": "cu-parent",
		"tags":   []any{map[string]any{"name": "urgent"}, map[string]any{"name": "backend"}},
	})

	got, _ := toTask(raw, cfg)
	if got.Title != "Trim me" {
		t.Fatalf("title = %q", got.Title)
	}
	if got.Status != StatusDone {
		t.Fatalf("status = %v, want done (wydane is a configured done status)", got.Status)
	}
	if got.ClickUpParentID != "cu-parent" {
		t.Fatalf("parent = %q", got.ClickUpParentID)
	}
	if !tagsEqual(got.Tags, []string{"backend", "urgent"}) {
		t.Fatalf("tags = %#v", got.Tags)
	}
	if got.InitialEstimate != nil {
		t.Fatalf("estimate = %d, want nil", *got.InitialEstimate)
	}
}

// Parity: the status match is exact and ordinal, so a differently-cased ClickUp
// status maps to Open just as it does today.
func TestToTaskStatusMatchIsExact(t *testing.T) {
	cfg := testConfig("http://x/", "http://y/")
	raw := decodeClickUpTask(t, map[string]any{
		"id": "cu-1", "name": "x",
		"status": map[string]any{"status": "Wydane", "type": "custom"},
	})
	if got, _ := toTask(raw, cfg); got.Status != StatusOpen {
		t.Fatal("expected Open: the configured done statuses are lowercase and the match is exact")
	}
}

func TestFetchClickUpPaginates(t *testing.T) {
	tasks := make([]map[string]any, 0, 150)
	for i := range 150 {
		tasks = append(tasks, clickUpTaskJSON("cu-"+strconv.Itoa(i), "task "+strconv.Itoa(i), "open"))
	}
	cu := newFakeClickUp(t, tasks)
	cfg := testConfig(cu.baseURL(), "http://y/")
	client := newAPIClient("clickup", cfg.ClickUpBaseURL, clickUpHeader(cfg), cfg.ClickUpRPS, cfg.ClickUpRPS, cfg, true, testLogger())

	got, stats, warns, err := fetchClickUp(context.Background(), client, cfg)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 150 || stats.Tasks != 150 {
		t.Fatalf("got %d tasks (stats %d), want 150", len(got), stats.Tasks)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	// Parity with .NET: pages 0, 1 and then one empty page to terminate.
	if stats.Pages != 3 {
		t.Fatalf("pages = %d, want 3 (terminates on an empty page, as .NET does)", stats.Pages)
	}
	calls := cu.recorded()
	if len(calls) != 3 || !strings.Contains(calls[0].Path, "page=0") || !strings.Contains(calls[0].Path, "subtasks=true") {
		t.Fatalf("unexpected calls:\n%s", fmtCalls(calls))
	}
	if strings.Contains(calls[0].Path, "include_closed") {
		t.Fatal("include_closed must not be sent unless configured — flipping it would POST every historical closed task")
	}
}

func TestFetchClickUpIncludeClosedIsOptIn(t *testing.T) {
	cu := newFakeClickUp(t, nil)
	cfg := testConfig(cu.baseURL(), "http://y/")
	cfg.IncludeClosed = true
	client := newAPIClient("clickup", cfg.ClickUpBaseURL, clickUpHeader(cfg), cfg.ClickUpRPS, cfg.ClickUpRPS, cfg, true, testLogger())

	if _, _, _, err := fetchClickUp(context.Background(), client, cfg); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(cu.recorded()[0].Path, "include_closed=true") {
		t.Fatalf("include_closed not sent: %s", cu.recorded()[0].Path)
	}
}

func TestFetchClickUpAbortsOnHTTPError(t *testing.T) {
	cu := newFakeClickUp(t, []map[string]any{clickUpTaskJSON("cu-1", "x", "open")})
	cu.failWith = 401
	cu.failBody = `{"err":"Token invalid","ECODE":"OAUTH_025"}`

	cfg := testConfig(cu.baseURL(), "http://y/")
	cfg.MaxRetries = 0
	client := newAPIClient("clickup", cfg.ClickUpBaseURL, clickUpHeader(cfg), cfg.ClickUpRPS, cfg.ClickUpRPS, cfg, true, testLogger())

	tasks, _, _, err := fetchClickUp(context.Background(), client, cfg)
	if err == nil {
		t.Fatal("a 401 must abort the fetch, not be decoded into an empty task list")
	}
	if len(tasks) != 0 {
		t.Fatalf("no tasks may be returned alongside an error, got %d", len(tasks))
	}
}

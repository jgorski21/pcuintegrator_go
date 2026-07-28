package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty strings only", []string{"", "  "}, nil},
		{"trim and sort", []string{" urgent ", "backend"}, []string{"backend", "urgent"}},
		{"dedupe", []string{"a", "a", "b"}, []string{"a", "b"}},
		{"drops empties among real tags", []string{"a", "", "b"}, []string{"a", "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeTags(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

// CHURN FIX #1. .NET's "".Split(", ") yields [""] — one empty string — while the
// ClickUp side yields an empty set, so SetEquals is false and every untagged task
// is PATCHed on every run forever. An empty field must read back as no tags.
func TestParseTagsField_EmptyIsNoTags(t *testing.T) {
	for _, in := range []string{"", " ", ","} {
		if got := parseTagsField(in); got != nil {
			t.Fatalf("parseTagsField(%q) = %#v, want nil", in, got)
		}
	}
	if got := parseTagsField("backend, urgent"); !reflect.DeepEqual(got, []string{"backend", "urgent"}) {
		t.Fatalf("round trip failed: %#v", got)
	}
	// Whatever order .NET happened to write, we read it back normalized.
	if got := parseTagsField("urgent, backend"); !reflect.DeepEqual(got, []string{"backend", "urgent"}) {
		t.Fatalf("unsorted input not normalized: %#v", got)
	}
}

func TestTagsRoundTripIsStable(t *testing.T) {
	tags := normalizeTags([]string{"zeta", "alpha", "alpha", " mid "})
	field := formatTagsField(tags)
	if field != "alpha, mid, zeta" {
		t.Fatalf("formatTagsField = %q", field)
	}
	if !tagsEqual(tags, parseTagsField(field)) {
		t.Fatalf("round trip not stable: %#v vs %#v", tags, parseTagsField(field))
	}
}

func TestEstimatesEqual(t *testing.T) {
	tests := []struct {
		name           string
		clickUp        *int
		existing       *int
		mode           EstimateMode
		want           bool
		wantClearWrite bool
	}{
		// CHURN FIX #3: ms/60000 truncates, so a 30-second ClickUp estimate is 0
		// minutes; nil and 0 must be the same thing.
		{"nil vs zero", nil, intPtr(0), EstimateIgnoreNil, true, false},
		{"zero vs nil", intPtr(0), nil, EstimateIgnoreNil, true, false},
		{"nil vs nil", nil, nil, EstimateIgnoreNil, true, false},
		{"same value", intPtr(90), intPtr(90), EstimateIgnoreNil, true, false},
		{"different value", intPtr(90), intPtr(600), EstimateIgnoreNil, false, false},
		{"clickup set, productive nil", intPtr(90), nil, EstimateIgnoreNil, false, false},

		// CHURN FIX #2: the write omits initial_estimate when nil, so this diff can
		// never be resolved by writing. Ignore mode declares it equal.
		{"cleared in clickup, ignore mode", nil, intPtr(480), EstimateIgnoreNil, true, false},
		{"cleared in clickup, clear mode", nil, intPtr(480), EstimateClearNil, false, true},
		{"zero in clickup, clear mode", intPtr(0), intPtr(480), EstimateClearNil, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimatesEqual(tc.clickUp, tc.existing, tc.mode); got != tc.want {
				t.Fatalf("estimatesEqual = %v, want %v", got, tc.want)
			}
			if got := wantsEstimateClear(tc.clickUp, tc.existing, tc.mode); got != tc.wantClearWrite {
				t.Fatalf("wantsEstimateClear = %v, want %v", got, tc.wantClearWrite)
			}
		})
	}
}

func TestDiffReasons(t *testing.T) {
	base := Task{
		ClickUpID:       "cu-1",
		Title:           "Title",
		Status:          StatusOpen,
		Tags:            normalizeTags([]string{"a", "b"}),
		InitialEstimate: intPtr(90),
	}

	tests := []struct {
		name   string
		mutate func(*Task)
		want   []string
	}{
		{"identical", func(*Task) {}, nil},
		{"title", func(t *Task) { t.Title = "Other" }, []string{"title"}},
		{"status", func(t *Task) { t.Status = StatusDone }, []string{"status"}},
		{"tags", func(t *Task) { t.Tags = normalizeTags([]string{"a"}) }, []string{"tags"}},
		{"estimate", func(t *Task) { t.InitialEstimate = intPtr(600) }, []string{"estimate"}},
		{"several, fixed order", func(t *Task) {
			t.Title = "Other"
			t.Tags = nil
		}, []string{"title", "tags"}},

		// Parenting is deliberately outside change detection, exactly as the .NET
		// record excludes ClickUpParentId from Equals.
		{"parent ignored", func(t *Task) { t.ClickUpParentID = "cu-99" }, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clickUp := base
			clickUp.Tags = append([]string(nil), base.Tags...)
			tc.mutate(&clickUp)

			got := diffReasons(clickUp, base, EstimateIgnoreNil)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diffReasons = %#v, want %#v", got, tc.want)
			}
			if equalForSync(clickUp, base, EstimateIgnoreNil) != (len(tc.want) == 0) {
				t.Fatalf("equalForSync disagrees with diffReasons")
			}
		})
	}
}

// Equality must compare the *pointed-to* estimate, not the pointer. Comparing
// pointers would report every task as changed and PATCH the whole list on every
// run — the single most expensive way to get this port wrong.
func TestEqualForSyncComparesPointeesNotPointers(t *testing.T) {
	a := Task{ClickUpID: "x", Title: "t", InitialEstimate: intPtr(90)}
	b := Task{ClickUpID: "x", Title: "t", InitialEstimate: intPtr(90)}
	if a.InitialEstimate == b.InitialEstimate {
		t.Fatal("test setup is wrong: the pointers must be distinct for this to prove anything")
	}
	if !equalForSync(a, b, EstimateIgnoreNil) {
		t.Fatal("equalForSync must compare the pointed-to values")
	}

	c := Task{ClickUpID: "x", Title: "t", InitialEstimate: intPtr(120)}
	if equalForSync(a, c, EstimateIgnoreNil) {
		t.Fatal("different estimates must not compare equal")
	}
}

// reflect.DeepEqual would be wrong in two directions here: it pulls in
// ClickUpParentID (breaking parity with .NET) and treats a nil slice as different
// from an empty one (churning every untagged task).
func TestEqualForSyncIsNotDeepEqual(t *testing.T) {
	if !tagsEqual(nil, []string{}) {
		t.Fatal("nil tags must equal empty tags")
	}
	a := Task{ClickUpID: "x", Title: "t", Tags: nil}
	b := Task{ClickUpID: "x", Title: "t", Tags: []string{}, ClickUpParentID: "cu-99"}
	if reflect.DeepEqual(a, b) {
		t.Fatal("test setup is wrong: DeepEqual should disagree here")
	}
	if !equalForSync(a, b, EstimateIgnoreNil) {
		t.Fatal("equalForSync must ignore parenting and nil-vs-empty tags")
	}
}

func TestFlexInt(t *testing.T) {
	tests := []struct {
		raw     string
		wantVal int64
		wantSet bool
		wantErr bool
	}{
		{`null`, 0, false, false},
		{`0`, 0, true, false},
		{`3600000`, 3600000, true, false},
		{`"3600000"`, 3600000, true, false},
		{`3.6e6`, 3600000, true, false},
		{`123.0`, 123, true, false},
		{`""`, 0, false, false},
		{`"  "`, 0, false, false},
		{`true`, 0, false, false},
		{`"abc"`, 0, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			var f flexInt
			err := json.Unmarshal([]byte(tc.raw), &f)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if f.V != tc.wantVal || f.Set != tc.wantSet {
				t.Fatalf("got {V:%d Set:%v} want {V:%d Set:%v}", f.V, f.Set, tc.wantVal, tc.wantSet)
			}
		})
	}
}

// orderindex arrives as both 0 and "0" in ClickUp's own documented examples, and
// a plain int field would fail the whole page decode on the other shape.
func TestFlexIntInsideStruct(t *testing.T) {
	for _, raw := range []string{
		`{"id":"a","name":"n","orderindex":0}`,
		`{"id":"a","name":"n","orderindex":"0"}`,
	} {
		var o clickUpDropdownOption
		if err := json.Unmarshal([]byte(raw), &o); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !o.OrderIndex.Set || o.OrderIndex.V != 0 {
			t.Fatalf("%s: orderindex not decoded: %+v", raw, o.OrderIndex)
		}
	}
}

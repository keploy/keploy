package matcher

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// orderRuns is how many times each row below repeats one comparison. Go
// walks a map in a new random order on every walk, so a list built from one
// without sorting comes out in another order within a few runs.
const orderRuns = 50

// orderDocs is a recorded and a live JSON body that differ by eight removed
// fields, eight retyped, eight changed in value and eight added, each written
// out of alphabetical order, with the names each list holds, sorted.
func orderDocs() (exp, act string, removed, retyped, changed, added []string) {
	names := func(prefix string) []string {
		// Not alphabetical: the bodies below name them in this order.
		out := make([]string, 0, 8)
		for _, n := range []int{7, 2, 5, 0, 6, 3, 1, 4} {
			out = append(out, fmt.Sprintf("%s_%d", prefix, n))
		}
		return out
	}
	sorted := func(prefix string) []string {
		out := make([]string, 0, 8)
		for n := 0; n < 8; n++ {
			out = append(out, fmt.Sprintf("%s_%d", prefix, n))
		}
		return out
	}
	var e, a []string
	for _, f := range names("gone") {
		e = append(e, fmt.Sprintf("%q:%q", f, "v"))
	}
	for _, f := range names("retyped") {
		e = append(e, fmt.Sprintf("%q:1", f))
		a = append(a, fmt.Sprintf("%q:%q", f, "1"))
	}
	for _, f := range names("changed") {
		e = append(e, fmt.Sprintf("%q:%q", f, "before"))
		a = append(a, fmt.Sprintf("%q:%q", f, "after"))
	}
	for _, f := range names("new") {
		a = append(a, fmt.Sprintf("%q:true", f))
	}
	exp = "{" + strings.Join(e, ",") + "}"
	act = "{" + strings.Join(a, ",") + "}"
	return exp, act, sorted("gone"), sorted("retyped"), sorted("changed"), sorted("new")
}

// TestFailureAssessmentReadsTheSameOnEveryRun: a test case's failure
// assessment, which the test report shows the user, names the removed and
// retyped fields in one order, sorted, on every run of the same failure, and
// lists every changed field sorted.
func TestFailureAssessmentReadsTheSameOnEveryRun(t *testing.T) {
	exp, act, removed, retyped, changed, added := orderDocs()
	want := &models.FailureAssessment{
		Category:      []models.FailureCategory{models.SchemaBroken},
		Risk:          models.High,
		AddedFields:   added,
		RemovedFields: removed,
		TypeChanges:   retyped,
		ValueChanges:  changed,
		Reasons: []string{
			"Removed fields: " + strings.Join(removed, ", "),
			"Type changes at: " + strings.Join(retyped, ", "),
		},
	}
	wantText, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for run := 0; run < orderRuns; run++ {
		got, err := ComputeFailureAssessmentJSON(exp, act, nil, false)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		gotText, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotText) != string(wantText) {
			t.Fatalf("run %d of the same failure reads otherwise:\n got %s\nwant %s", run, gotText, wantText)
		}
	}
}

// TestChangedFieldPathsAndFieldDiffsKeepOneOrder: the changed paths noise
// learning reads and the field diffs a mock mismatch reports come in one
// order on every run: each kind of change sorted, the kinds in the order
// each function gives them.
func TestChangedFieldPathsAndFieldDiffsKeepOneOrder(t *testing.T) {
	exp, act, removed, retyped, changed, added := orderDocs()

	wantPaths := append(append(append([]string(nil), changed...), retyped...), removed...)
	for run := 0; run < orderRuns; run++ {
		if got := ChangedJSONFieldPaths(exp, act, nil, nil, false, nil); !reflect.DeepEqual(got, wantPaths) {
			t.Fatalf("run %d: changed paths\n got %v\nwant %v", run, got, wantPaths)
		}
	}

	type diff struct {
		path string
		kind models.MockFieldDiffKind
	}
	var wantDiffs []diff
	for _, group := range []struct {
		paths []string
		kind  models.MockFieldDiffKind
	}{
		{changed, models.DiffKindValueChanged},
		{retyped, models.DiffKindTypeChanged},
		{removed, models.DiffKindMissingInLive},
		{added, models.DiffKindMissingInMock},
	} {
		for _, p := range group.paths {
			wantDiffs = append(wantDiffs, diff{"body." + p, group.kind})
		}
	}
	for run := 0; run < orderRuns; run++ {
		var got []diff
		for _, d := range JSONFieldDiffs(exp, act, nil, "body.", 0) {
			got = append(got, diff{d.Path, d.Kind})
		}
		if !reflect.DeepEqual(got, wantDiffs) {
			t.Fatalf("run %d: field diffs\n got %v\nwant %v", run, got, wantDiffs)
		}
	}
}

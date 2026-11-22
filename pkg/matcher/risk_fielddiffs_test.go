package matcher

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestJSONFieldDiffs_KindsValuesAndNoise(t *testing.T) {
	exp := `{"id":"a","ts":"1","gone":"g","typ":1,"same":"s"}`
	act := `{"id":"b","ts":"2","typ":"now-string","same":"s","added":true}`

	diffs := JSONFieldDiffs(exp, act, map[string][]string{"ts": {}}, "body.", 0)

	got := map[string]models.MockFieldDiff{}
	for _, d := range diffs {
		got[d.Path] = d
	}

	if d := got["body.id"]; d.Kind != models.DiffKindValueChanged || d.Expected != "a" || d.Actual != "b" {
		t.Errorf("body.id diff wrong: %+v", d)
	}
	if d := got["body.gone"]; d.Kind != models.DiffKindMissingInLive || d.Expected != "g" {
		t.Errorf("body.gone diff wrong: %+v", d)
	}
	if d := got["body.typ"]; d.Kind != models.DiffKindTypeChanged {
		t.Errorf("body.typ diff wrong: %+v", d)
	}
	if d := got["body.added"]; d.Kind != models.DiffKindMissingInMock || d.Actual != "true" {
		t.Errorf("body.added diff wrong: %+v", d)
	}
	if _, ok := got["body.ts"]; ok {
		t.Errorf("noised path body.ts must not be reported: %+v", diffs)
	}
	if _, ok := got["body.same"]; ok {
		t.Errorf("unchanged path body.same must not be reported: %+v", diffs)
	}
}

func TestJSONFieldDiffs_TruncatesValues(t *testing.T) {
	exp := `{"v":"aaaaaaaaaaaaaaaaaaaa"}`
	act := `{"v":"bbbbbbbbbbbbbbbbbbbb"}`
	diffs := JSONFieldDiffs(exp, act, nil, "body.", 5)
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if len(diffs[0].Expected) > 9 || len(diffs[0].Actual) > 9 { // 5 bytes + ellipsis rune
		t.Errorf("values not truncated: %+v", diffs[0])
	}
}

func TestJSONFieldDiffs_InvalidJSONReturnsNil(t *testing.T) {
	if d := JSONFieldDiffs("not-json", `{"a":1}`, nil, "body.", 0); d != nil {
		t.Errorf("expected nil for invalid JSON, got %+v", d)
	}
}

// TestJSONFieldDiffs_PatternIsJudgedOnBothSides pins that a pattern-guarded
// entry is held to the recorded and the live value alike: the recorded id 1
// under {"id": ["^1$"]} against a live 2 is a changed value, reported with both
// values, not a recorded id hidden and a live one added.
func TestJSONFieldDiffs_PatternIsJudgedOnBothSides(t *testing.T) {
	got := JSONFieldDiffs(`{"id":1,"n":"a"}`, `{"id":2,"n":"a"}`, map[string][]string{"id": {"^1$"}}, "body.", 0)
	if len(got) != 1 || got[0].Path != "body.id" || got[0].Kind != models.DiffKindValueChanged || got[0].Expected != "1" || got[0].Actual != "2" {
		t.Fatalf("got %+v, want one value change of body.id from 1 to 2", got)
	}
	// The other way round: the live value matches, the recorded one does not.
	got = JSONFieldDiffs(`{"id":2,"n":"a"}`, `{"id":1,"n":"a"}`, map[string][]string{"id": {"^1$"}}, "body.", 0)
	if len(got) != 1 || got[0].Path != "body.id" || got[0].Kind != models.DiffKindValueChanged || got[0].Expected != "2" || got[0].Actual != "1" {
		t.Fatalf("got %+v, want one value change of body.id from 2 to 1", got)
	}
	if got := ChangedJSONFieldPaths(`{"id":2}`, `{"id":1}`, map[string][]string{"id": {"^1$"}}, nil, true, nil); len(got) != 1 || got[0] != "id" {
		t.Fatalf("value changes only: got %v, want [id]", got)
	}
	if got := JSONFieldDiffs(`{"id":1}`, `{"id":2}`, map[string][]string{"id": {"^[12]$"}}, "body.", 0); len(got) != 0 {
		t.Fatalf("got %+v, want none: both values match the pattern", got)
	}
	assess, err := ComputeFailureAssessmentJSON(`{"id":1}`, `{"id":2}`, map[string][]string{"id": {"^1$"}}, false)
	if err != nil || assess == nil || len(assess.ValueChanges) != 1 || assess.ValueChanges[0] != "id" || len(assess.AddedFields) != 0 {
		t.Fatalf("assessment = %+v (err %v), want id as a value change and nothing added", assess, err)
	}
}

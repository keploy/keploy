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

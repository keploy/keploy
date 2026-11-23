package matcher

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

func TestJSONFieldDiffsWithTypeStrict_ReportsMissingAndTypeChangePreservingValues(t *testing.T) {
	for _, mode := range []struct {
		name string
		env  string
	}{
		{name: "proven_unset", env: ""},
		{name: "proven_1", env: "1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, mode.env)

			known := map[string][]string{"ts": {}, "meta": {}, "user_skip": {}}
			ts := map[string]struct{}{"ts": {}, "meta": {}}

			// Missing top-level learned field reports kind and recorded value.
			diffs := JSONFieldDiffsWithTypeStrict(`{"q":"ada","ts":1}`, `{"q":"ada"}`, known, ts, "body.", 0)
			if len(diffs) != 1 || diffs[0].Path != "body.ts" || diffs[0].Kind != models.DiffKindMissingInLive || diffs[0].Expected != "1" {
				t.Fatalf("missing learned field: got %+v, want body.ts missing_in_live expected=1", diffs)
			}

			// Type-changed learned field reports kind and typed values on both sides.
			diffs = JSONFieldDiffsWithTypeStrict(`{"q":"ada","ts":1}`, `{"q":"ada","ts":"1"}`, known, ts, "body.", 0)
			if len(diffs) != 1 || diffs[0].Path != "body.ts" || diffs[0].Kind != models.DiffKindTypeChanged || diffs[0].Expected != "number: 1" || diffs[0].Actual != "string: 1" {
				t.Fatalf("type-changed learned field: got %+v, want body.ts type_changed number: 1 -> string: 1", diffs)
			}

			// Same-type value change on learned field is still ignored.
			if diffs = JSONFieldDiffsWithTypeStrict(`{"q":"ada","ts":1}`, `{"q":"ada","ts":2}`, known, ts, "body.", 0); len(diffs) != 0 {
				t.Fatalf("same-type value drift on learned field must be ignored; got %+v", diffs)
			}

			// Learned value drift alongside a non-noise value change reports only the non-noise field.
			diffs = JSONFieldDiffsWithTypeStrict(`{"q":"ada","ts":1}`, `{"q":"bob","ts":2}`, known, ts, "body.", 0)
			if len(diffs) != 1 || diffs[0].Path != "body.q" || diffs[0].Kind != models.DiffKindValueChanged || diffs[0].Expected != "ada" || diffs[0].Actual != "bob" {
				t.Fatalf("expected only body.q value_changed; got %+v", diffs)
			}

			// Added child under a learned container reports missing_in_mock with actual value, ignoring same-type sibling drift.
			diffs = JSONFieldDiffsWithTypeStrict(`{"meta":{"a":1}}`, `{"meta":{"a":2,"b":3}}`, known, ts, "body.", 0)
			if len(diffs) != 1 || diffs[0].Path != "body.meta.b" || diffs[0].Kind != models.DiffKindMissingInMock || diffs[0].Actual != "3" {
				t.Fatalf("added child under learned container: got %+v, want body.meta.b missing_in_mock actual=3", diffs)
			}

			// User full-ignore (in known, not in typeStrict) stays ignored even on removal or type change.
			if diffs = JSONFieldDiffsWithTypeStrict(`{"q":"ada","user_skip":1}`, `{"q":"ada"}`, known, ts, "body.", 0); len(diffs) != 0 {
				t.Fatalf("user full-ignore missing field must stay ignored; got %+v", diffs)
			}
			if diffs = JSONFieldDiffsWithTypeStrict(`{"q":"ada","user_skip":1}`, `{"q":"ada","user_skip":"1"}`, known, ts, "body.", 0); len(diffs) != 0 {
				t.Fatalf("user full-ignore type change must stay ignored; got %+v", diffs)
			}

			// Truncation applies to strictValues as well.
			diffs = JSONFieldDiffsWithTypeStrict(`{"ts":"1234567890"}`, `{}`, known, ts, "body.", 5)
			if len(diffs) != 1 || diffs[0].Expected != "12345…" {
				t.Fatalf("expected truncated strict value, got %+v", diffs)
			}

			// Multiple type-strict fields missing or retyped in one call preserve all values.
			known2 := map[string][]string{"ts": {}, "nonce": {}}
			ts2 := map[string]struct{}{"ts": {}, "nonce": {}}
			diffs = JSONFieldDiffsWithTypeStrict(`{"q":"ada","ts":1,"nonce":"abc"}`, `{"q":"ada"}`, known2, ts2, "body.", 0)
			if len(diffs) != 2 ||
				diffs[0].Path != "body.nonce" || diffs[0].Kind != models.DiffKindMissingInLive || diffs[0].Expected != "abc" ||
				diffs[1].Path != "body.ts" || diffs[1].Kind != models.DiffKindMissingInLive || diffs[1].Expected != "1" {
				t.Fatalf("two missing type-strict fields must keep both values; got %+v", diffs)
			}
			diffs = JSONFieldDiffsWithTypeStrict(`{"q":"ada","ts":1,"nonce":"abc"}`, `{"q":"ada","ts":"1","nonce":42}`, known2, ts2, "body.", 0)
			if len(diffs) != 2 ||
				diffs[0].Path != "body.nonce" || diffs[0].Kind != models.DiffKindTypeChanged || diffs[0].Expected != "string: abc" || diffs[0].Actual != "number: 42" ||
				diffs[1].Path != "body.ts" || diffs[1].Kind != models.DiffKindTypeChanged || diffs[1].Expected != "number: 1" || diffs[1].Actual != "string: 1" {
				t.Fatalf("two retyped type-strict fields must keep all values; got %+v", diffs)
			}

			// Type-strict path under an id-key noise entry preserves both recorded and live values.
			u := testUUID(1)
			idKnown := map[string][]string{"{id}.ts": {}}
			idTS := map[string]struct{}{"{id}.ts": {}}
			diffs = JSONFieldDiffsWithTypeStrict(`{"`+u+`":{"ts":1}}`, `{"`+u+`":{"ts":"1"}}`, idKnown, idTS, "body.", 0)
			if len(diffs) != 1 || diffs[0].Path != "body."+u+".ts" || diffs[0].Kind != models.DiffKindTypeChanged ||
				diffs[0].Expected != "number: 1" || diffs[0].Actual != "string: 1" {
				t.Fatalf("id-key type-strict retyped field: got %+v", diffs)
			}

			// Scalar <-> container shape changes report one type_changed diff, never missing + added children.
			shapeCases := []struct {
				name                   string
				rec, live              string
				k                      map[string][]string
				s                      map[string]struct{}
				wantPath, wantE, wantA string
			}{
				{
					name:     "learned scalar to object",
					rec:      `{"q":"a","ts":1}`,
					live:     `{"q":"a","ts":{"s":1}}`,
					k:        known,
					s:        ts,
					wantPath: "body.ts",
					wantE:    "number: 1",
					wantA:    "object",
				},
				{
					name:     "learned scalar to empty object",
					rec:      `{"q":"a","ts":1}`,
					live:     `{"q":"a","ts":{}}`,
					k:        known,
					s:        ts,
					wantPath: "body.ts",
					wantE:    "number: 1",
					wantA:    "object",
				},
				{
					name:     "learned scalar to array",
					rec:      `{"q":"a","ts":1}`,
					live:     `{"q":"a","ts":[1,2]}`,
					k:        known,
					s:        ts,
					wantPath: "body.ts",
					wantE:    "number: 1",
					wantA:    "array",
				},
				{
					name:     "learned object to scalar",
					rec:      `{"q":"a","meta":{"s":1}}`,
					live:     `{"q":"a","meta":1}`,
					k:        known,
					s:        ts,
					wantPath: "body.meta",
					wantE:    "object",
					wantA:    "number: 1",
				},
				{
					name:     "learned array to scalar",
					rec:      `{"q":"a","ts":["x","y"]}`,
					live:     `{"q":"a","ts":"x"}`,
					k:        known,
					s:        ts,
					wantPath: "body.ts",
					wantE:    "array",
					wantA:    "string: x",
				},
				{
					name:     "learned empty array to scalar",
					rec:      `{"q":"a","ts":[]}`,
					live:     `{"q":"a","ts":"x"}`,
					k:        known,
					s:        ts,
					wantPath: "body.ts",
					wantE:    "array",
					wantA:    "string: x",
				},
				{
					name:     "non-noise scalar to object",
					rec:      `{"q":"a","v":1}`,
					live:     `{"q":"a","v":{"nested":{"leaf":2}}}`,
					k:        nil,
					s:        nil,
					wantPath: "body.v",
					wantE:    "number: 1",
					wantA:    "object",
				},
				{
					name:     "learned object to array (K1)",
					rec:      `{"q":"a","meta":{"ts":1}}`,
					live:     `{"q":"a","meta":[{"ts":1}]}`,
					k:        known,
					s:        ts,
					wantPath: "body.meta",
					wantE:    "object",
					wantA:    "array",
				},
				{
					name:     "learned array to object (K2)",
					rec:      `{"q":"a","ts":["x"]}`,
					live:     `{"q":"a","ts":{"a":"x"}}`,
					k:        known,
					s:        ts,
					wantPath: "body.ts",
					wantE:    "array",
					wantA:    "object",
				},
				{
					name:     "empty object to empty array",
					rec:      `{"q":"a","meta":{}}`,
					live:     `{"q":"a","meta":[]}`,
					k:        known,
					s:        ts,
					wantPath: "body.meta",
					wantE:    "object",
					wantA:    "array",
				},
			}
			for _, sc := range shapeCases {
				t.Run(sc.name, func(t *testing.T) {
					got := JSONFieldDiffsWithTypeStrict(sc.rec, sc.live, sc.k, sc.s, "body.", 0)
					if len(got) != 1 || got[0].Path != sc.wantPath || got[0].Kind != models.DiffKindTypeChanged ||
						got[0].Expected != sc.wantE || got[0].Actual != sc.wantA {
						t.Fatalf("%s: got %+v, want one type_changed %s (%q -> %q)", sc.name, got, sc.wantPath, sc.wantE, sc.wantA)
					}
				})
			}

			// Type diffs stay sorted by path when a scalar type change coexists with any container retype.
			for _, tc := range []struct {
				name, rec, live string
			}{
				{"scalar_to_container_with_scalar_retype", `{"a":1,"z":1}`, `{"a":{"x":1},"z":"1"}`},
				{"container_to_scalar_with_scalar_retype", `{"a":{"x":1},"z":1}`, `{"a":1,"z":"1"}`},
				{"container_to_container_with_scalar_retype", `{"a":{"x":1},"z":1}`, `{"a":[{"x":1}],"z":"1"}`},
			} {
				got := JSONFieldDiffsWithTypeStrict(tc.rec, tc.live, nil, nil, "body.", 0)
				if len(got) != 2 || got[0].Path != "body.a" || got[1].Path != "body.z" ||
					got[0].Kind != models.DiffKindTypeChanged || got[1].Kind != models.DiffKindTypeChanged {
					t.Fatalf("%s: want sorted [body.a, body.z] type_changed; got %+v", tc.name, got)
				}
			}

			// A sibling whose name merely starts with the retyped field's name is NOT dropped.
			diffs = JSONFieldDiffsWithTypeStrict(`{"ts":1}`, `{"ts":{"s":1},"ts2":2}`, known, ts, "body.", 0)
			if len(diffs) != 2 || diffs[0].Path != "body.ts" || diffs[0].Kind != models.DiffKindTypeChanged ||
				diffs[1].Path != "body.ts2" || diffs[1].Kind != models.DiffKindMissingInMock {
				t.Fatalf("sibling prefix path ts2 must not be dropped: got %+v", diffs)
			}
			diffs = JSONFieldDiffsWithTypeStrict(`{"meta":{"ts":1},"meta2":1}`, `{"meta":[{"ts":1}],"meta3":2}`, known, ts, "body.", 0)
			if len(diffs) != 3 ||
				diffs[0].Path != "body.meta" || diffs[0].Kind != models.DiffKindTypeChanged ||
				diffs[1].Path != "body.meta2" || diffs[1].Kind != models.DiffKindMissingInLive ||
				diffs[2].Path != "body.meta3" || diffs[2].Kind != models.DiffKindMissingInMock {
				t.Fatalf("sibling prefix paths meta2/meta3 must not be dropped on container retype: got %+v", diffs)
			}

			// Old JSONFieldDiffs keeps CountJSONFieldDiffs parity (no container-tracking drift).
			oldDiffs := JSONFieldDiffs(`{"q":"a","ts":1}`, `{"q":"a","ts":{"s":1}}`, nil, "body.", 0)
			if cnt := CountJSONFieldDiffs(CollectJSONFields(parseDoc(t, `{"q":"a","ts":1}`), nil), CollectJSONFields(parseDoc(t, `{"q":"a","ts":{"s":1}}`), nil)); cnt != len(oldDiffs) {
				t.Fatalf("JSONFieldDiffs len=%d != CountJSONFieldDiffs=%d", len(oldDiffs), cnt)
			}

			// Hot matching path (collectBoth) must not allocate report metadata.
			ni := buildNoiseIndex(known, nil)
			ni.markTypeStrict(ts)
			expM, actM := collectBoth(parseDoc(t, `{"meta":{"ts":1},"items":[1]}`), parseDoc(t, `{"meta":{"ts":2},"items":[1]}`), ni)
			if expM.report != nil || actM.report != nil {
				t.Fatalf("collectBoth must leave report nil on hot path; got exp=%v act=%v", expM.report, actM.report)
			}
		})
	}
}

func TestJSONFieldDiffsWithTypeStrictRedacted_RedactsBeforeTruncation(t *testing.T) {
	makePH := func(n int) string {
		const head = "OBFPH77"
		return head + strings.Repeat("q", n-len(head))
	}
	exactMatch := func(target string) func(string) bool {
		return func(raw string) bool { return raw == target }
	}

	// Cover all 6 scalar rendering sites with values longer than maxVal (and at
	// the exact prefix-overflow boundary where raw <= maxVal < len("string: "+raw)).
	const maxVal = 32
	for _, length := range []int{24, 25, 32, 33, 64} {
		ph := makePH(length)
		redact := exactMatch(ph)

		cases := []struct {
			name      string
			rec, live string
			wantKind  models.MockFieldDiffKind
			wantE     string
			wantA     string
		}{
			{
				name:     "value_changed_actual",
				rec:      `{"v":"ok"}`,
				live:     `{"v":"` + ph + `"}`,
				wantKind: models.DiffKindValueChanged,
				wantE:    "ok",
				wantA:    redactedDiffValue,
			},
			{
				name:     "value_changed_expected",
				rec:      `{"v":"` + ph + `"}`,
				live:     `{"v":"ok"}`,
				wantKind: models.DiffKindValueChanged,
				wantE:    redactedDiffValue,
				wantA:    "ok",
			},
			{
				name:     "type_changed_scalar_to_scalar_actual",
				rec:      `{"v":1}`,
				live:     `{"v":"` + ph + `"}`,
				wantKind: models.DiffKindTypeChanged,
				wantE:    "number: 1",
				wantA:    "string: " + redactedDiffValue,
			},
			{
				name:     "type_changed_scalar_to_scalar_expected",
				rec:      `{"v":"` + ph + `"}`,
				live:     `{"v":1}`,
				wantKind: models.DiffKindTypeChanged,
				wantE:    "string: " + redactedDiffValue,
				wantA:    "number: 1",
			},
			{
				name:     "type_changed_scalar_to_container",
				rec:      `{"v":"` + ph + `"}`,
				live:     `{"v":{"a":1}}`,
				wantKind: models.DiffKindTypeChanged,
				wantE:    "string: " + redactedDiffValue,
				wantA:    "object",
			},
			{
				name:     "type_changed_container_to_scalar",
				rec:      `{"v":{"s":"` + ph + `"}}`,
				live:     `{"v":"` + ph + `"}`,
				wantKind: models.DiffKindTypeChanged,
				wantE:    "object",
				wantA:    "string: " + redactedDiffValue,
			},
			{
				name:     "missing_in_live",
				rec:      `{"v":"` + ph + `"}`,
				live:     `{}`,
				wantKind: models.DiffKindMissingInLive,
				wantE:    redactedDiffValue,
				wantA:    "",
			},
			{
				name:     "missing_in_mock",
				rec:      `{}`,
				live:     `{"v":"` + ph + `"}`,
				wantKind: models.DiffKindMissingInMock,
				wantE:    "",
				wantA:    redactedDiffValue,
			},
		}

		for _, tc := range cases {
			got := JSONFieldDiffsWithTypeStrictRedacted(tc.rec, tc.live, nil, nil, "body.", maxVal, redact)
			if len(got) != 1 || got[0].Path != "body.v" || got[0].Kind != tc.wantKind ||
				got[0].Expected != tc.wantE || got[0].Actual != tc.wantA {
				t.Fatalf("len=%d %s: got %+v, want kind=%s E=%q A=%q", length, tc.name, got, tc.wantKind, tc.wantE, tc.wantA)
			}
		}
	}

	// Printable values exceeding maxVal are still truncated cleanly.
	longBenign := strings.Repeat("a", 50)
	got := JSONFieldDiffsWithTypeStrictRedacted(`{"v":{"s":1}}`, `{"v":"`+longBenign+`"}`, nil, nil, "body.", maxVal, func(string) bool { return false })
	if len(got) != 1 || got[0].Actual != "string: "+strings.Repeat("a", maxVal-len("string: "))+"…" {
		t.Fatalf("printable long value should truncate cleanly; got %+v", got)
	}
}

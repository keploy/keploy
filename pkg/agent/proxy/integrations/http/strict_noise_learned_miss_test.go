package http

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// TestStrictNoiseLearnedFieldMissNamesFieldAndKind pins that when a recording
// learned a body field as dynamic noise (Spec.ReqBodyNoise) and a live call
// omits it or changes its JSON type under strict noise enforcement, the miss
// report names the offending field and its kind rather than reporting "no
// field-level differences".
func TestStrictNoiseLearnedFieldMissNamesFieldAndKind(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json"}

	cases := []struct {
		name         string
		rec          string
		live         string
		learned      map[string][]string
		wantPath     string
		wantKind     models.MockFieldDiffKind
		wantExpected string // checked when KEPLOY_PROVEN is unset (strict_compare omits Expected on missing fields)
		wantActual   string // checked when KEPLOY_PROVEN is unset on type_changed
	}{
		{
			name:         "top-level number missing",
			rec:          `{"q":"ada","ts":1}`,
			live:         `{"q":"ada"}`,
			learned:      map[string][]string{"body.ts": {}},
			wantPath:     "body.ts",
			wantKind:     models.DiffKindMissingInLive,
			wantExpected: "1",
		},
		{
			name:         "top-level string missing",
			rec:          `{"q":"ada","ts":"2026-01-01"}`,
			live:         `{"q":"ada"}`,
			learned:      map[string][]string{"body.ts": {}},
			wantPath:     "body.ts",
			wantKind:     models.DiffKindMissingInLive,
			wantExpected: "2026-01-01",
		},
		{
			name:         "top-level bool missing",
			rec:          `{"q":"ada","flag":true}`,
			live:         `{"q":"ada"}`,
			learned:      map[string][]string{"body.flag": {}},
			wantPath:     "body.flag",
			wantKind:     models.DiffKindMissingInLive,
			wantExpected: "true",
		},
		{
			name:         "top-level null missing",
			rec:          `{"q":"ada","opt":null}`,
			live:         `{"q":"ada"}`,
			learned:      map[string][]string{"body.opt": {}},
			wantPath:     "body.opt",
			wantKind:     models.DiffKindMissingInLive,
			wantExpected: "null",
		},
		{
			name:         "nested number missing",
			rec:          `{"q":"ada","meta":{"ts":1,"v":"ok"}}`,
			live:         `{"q":"ada","meta":{"v":"ok"}}`,
			learned:      map[string][]string{"body.meta.ts": {}},
			wantPath:     "body.meta.ts",
			wantKind:     models.DiffKindMissingInLive,
			wantExpected: "1",
		},
		{
			name:         "nested mixed-case string missing",
			rec:          `{"q":"ada","meta":{"requestId":"r-1","v":"ok"}}`,
			live:         `{"q":"ada","meta":{"v":"ok"}}`,
			learned:      map[string][]string{"body.meta.requestId": {}},
			wantPath:     "body.meta.requestId",
			wantKind:     models.DiffKindMissingInLive,
			wantExpected: "r-1",
		},
		{
			name:         "array element field missing",
			rec:          `{"q":"ada","items":[{"ts":1,"k":"v"}]}`,
			live:         `{"q":"ada","items":[{"k":"v"}]}`,
			learned:      map[string][]string{"body.items[].ts": {}},
			wantPath:     "body.items[].ts",
			wantKind:     models.DiffKindMissingInLive,
			wantExpected: "1",
		},
		{
			name:         "top-level type changed",
			rec:          `{"q":"ada","ts":1}`,
			live:         `{"q":"ada","ts":"1"}`,
			learned:      map[string][]string{"body.ts": {}},
			wantPath:     "body.ts",
			wantKind:     models.DiffKindTypeChanged,
			wantExpected: "number: 1",
			wantActual:   "string: 1",
		},
		{
			name:         "nested type changed",
			rec:          `{"q":"ada","meta":{"ts":1,"v":"ok"}}`,
			live:         `{"q":"ada","meta":{"ts":true,"v":"ok"}}`,
			learned:      map[string][]string{"body.meta.ts": {}},
			wantPath:     "body.meta.ts",
			wantKind:     models.DiffKindTypeChanged,
			wantExpected: "number: 1",
			wantActual:   "boolean: true",
		},
		{
			name:         "learned scalar sent as object",
			rec:          `{"q":"a","ts":1}`,
			live:         `{"q":"a","ts":{"s":1}}`,
			learned:      map[string][]string{"body.ts": {}},
			wantPath:     "body.ts",
			wantKind:     models.DiffKindTypeChanged,
			wantExpected: "number: 1",
			wantActual:   "object",
		},
		{
			name:         "learned scalar sent as array",
			rec:          `{"q":"a","ts":1}`,
			live:         `{"q":"a","ts":[1,2]}`,
			learned:      map[string][]string{"body.ts": {}},
			wantPath:     "body.ts",
			wantKind:     models.DiffKindTypeChanged,
			wantExpected: "number: 1",
			wantActual:   "array",
		},
		{
			name:         "learned object sent as scalar",
			rec:          `{"q":"a","meta":{"s":1}}`,
			live:         `{"q":"a","meta":1}`,
			learned:      map[string][]string{"body.meta": {}},
			wantPath:     "body.meta",
			wantKind:     models.DiffKindTypeChanged,
			wantExpected: "object",
			wantActual:   "number: 1",
		},
		{
			name:         "learned array sent as scalar",
			rec:          `{"q":"a","tags":["x","y"]}`,
			live:         `{"q":"a","tags":"x"}`,
			learned:      map[string][]string{"body.tags": {}},
			wantPath:     "body.tags",
			wantKind:     models.DiffKindTypeChanged,
			wantExpected: "array",
			wantActual:   "string: x",
		},
		{
			name:         "learned object sent as array (K1)",
			rec:          `{"q":"a","meta":{"ts":1}}`,
			live:         `{"q":"a","meta":[{"ts":1}]}`,
			learned:      map[string][]string{"body.meta": {}},
			wantPath:     "body.meta",
			wantKind:     models.DiffKindTypeChanged,
			wantExpected: "object",
			wantActual:   "array",
		},
		{
			name:         "learned array sent as object (K2)",
			rec:          `{"q":"a","tags":["x"]}`,
			live:         `{"q":"a","tags":{"a":"x"}}`,
			learned:      map[string][]string{"body.tags": {}},
			wantPath:     "body.tags",
			wantKind:     models.DiffKindTypeChanged,
			wantExpected: "array",
			wantActual:   "object",
		},
	}

	for _, mode := range []struct {
		name string
		env  string
	}{
		{name: "proven_unset", env: ""},
		{name: "proven_1", env: "1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, mode.env)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					m := recorded("m-learned", "/search", jsonHdr, tc.rec)
					m.Spec.ReqBodyNoise = tc.learned
					db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

					ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, tc.live), db, nil, nil, nil, true, false, true, true, true)
					if err != nil {
						t.Fatalf("match error: %v", err)
					}
					if ok {
						t.Fatalf("expected miss for %s, but matched", tc.name)
					}

					req := makeJSONPostRequest("/search", tc.live)
					r := newHTTP().buildHTTPMismatchReport(context.Background(), req, []byte(tc.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
					if r == nil {
						t.Fatal("expected non-nil mismatch report")
					}
					if strings.Contains(r.Diff, "no field-level differences") {
						t.Fatalf("miss report falsely says no field-level differences: diff=%q fieldDiffs=%+v", r.Diff, r.FieldDiffs)
					}
					if tc.wantKind == models.DiffKindTypeChanged && strings.Contains(r.Diff, "absent in live call") {
						t.Fatalf("type-changed field falsely reported as absent in live call: diff=%q fieldDiffs=%+v", r.Diff, r.FieldDiffs)
					}
					if len(r.FieldDiffs) != 1 {
						t.Fatalf("expected exactly 1 field diff; got %+v (diff=%q)", r.FieldDiffs, r.Diff)
					}
					d := r.FieldDiffs[0]
					if d.Path != tc.wantPath || d.Kind != tc.wantKind {
						t.Fatalf("got diff %+v; want path=%q kind=%q (diff=%q)", d, tc.wantPath, tc.wantKind, r.Diff)
					}
					if mode.env == "" {
						if d.Expected != tc.wantExpected {
							t.Fatalf("got Expected=%q; want %q (diff=%+v)", d.Expected, tc.wantExpected, d)
						}
						if tc.wantKind == models.DiffKindTypeChanged && d.Actual != tc.wantActual {
							t.Fatalf("got Actual=%q; want %q (diff=%+v)", d.Actual, tc.wantActual, d)
						}
					}
					if !strings.Contains(r.Diff, tc.wantPath) {
						t.Fatalf("expected Diff to name %q; got %q", tc.wantPath, r.Diff)
					}
				})
			}
		})
	}
}

// TestStrictNoiseLearnedFieldValueDriftStillMatchesAndUserNoiseWins pins that
// making learned fields type-strict in mismatch reporting does not break normal
// same-type learned value drift, user full-ignore precedence (case-insensitive),
// user pattern-guarded precedence, or secret redaction on a missing learned field.
func TestStrictNoiseLearnedFieldValueDriftStillMatchesAndUserNoiseWins(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json"}

	for _, mode := range []struct {
		name string
		env  string
	}{
		{name: "proven_unset", env: ""},
		{name: "proven_1", env: "1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, mode.env)

			t.Run("same-type learned drift still matches", func(t *testing.T) {
				for _, tc := range []struct {
					name    string
					rec     string
					live    string
					learned map[string][]string
				}{
					{"top-level", `{"q":"ada","ts":1}`, `{"q":"ada","ts":2}`, map[string][]string{"body.ts": {}}},
					{"nested", `{"q":"ada","meta":{"ts":1,"v":"ok"}}`, `{"q":"ada","meta":{"ts":99,"v":"ok"}}`, map[string][]string{"body.meta.ts": {}}},
				} {
					m := recorded("m-ok", "/search", jsonHdr, tc.rec)
					m.Spec.ReqBodyNoise = tc.learned
					db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
					ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, tc.live), db, nil, nil, nil, true, false, true, true, true)
					if err != nil {
						t.Fatalf("%s: match error: %v", tc.name, err)
					}
					if !ok {
						t.Fatalf("%s: expected match for same-type learned drift; diag=%+v", tc.name, diag)
					}
				}
			})

			t.Run("learned value drift alongside non-noise drift names only non-noise field", func(t *testing.T) {
				m := recorded("m-sibling", "/search", jsonHdr, `{"q":"ada","ts":1}`)
				m.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

				live := `{"q":"bob","ts":2}`
				ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, live), db, nil, nil, nil, true, false, true, true, true)
				if err != nil || ok {
					t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
				}
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
				if len(r.FieldDiffs) != 1 || r.FieldDiffs[0].Path != "body.q" || r.FieldDiffs[0].Kind != models.DiffKindValueChanged {
					t.Fatalf("expected only body.q value_changed; got %+v", r.FieldDiffs)
				}
			})

			t.Run("user full-ignore wins over learned case twin", func(t *testing.T) {
				m := recorded("m-user", "/search", jsonHdr, `{"q":"ada","meta":{"userId":1}}`)
				m.Spec.ReqBodyNoise = map[string][]string{"body.meta.userId": {}}
				userBodyNoise := map[string][]string{"meta.userid": {}}
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

				// Missing meta.userId still matches when user configured meta.userid as full-ignore.
				ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, `{"q":"ada","meta":{}}`), db, nil, userBodyNoise, nil, true, false, true, true, true)
				if err != nil || !ok {
					t.Fatalf("expected match when user full-ignore covers missing learned field; ok=%v err=%v diag=%+v", ok, err, diag)
				}

				// When q also drifts, report names only body.q, not body.meta.userId.
				live := `{"q":"bob","meta":{}}`
				db2 := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
				ok, _, diag, err = newHTTP().match(context.Background(), sent("/search", jsonHdr, live), db2, nil, userBodyNoise, nil, true, false, true, true, true)
				if err != nil || ok {
					t.Fatalf("expected miss for q drift, got ok=%v err=%v", ok, err)
				}
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, userBodyNoise, diag)
				if len(r.FieldDiffs) != 1 || r.FieldDiffs[0].Path != "body.q" {
					t.Fatalf("expected only body.q diff when user full-ignore covers meta.userId; got %+v", r.FieldDiffs)
				}
			})

			t.Run("user pattern-guarded entry wins over learned child path", func(t *testing.T) {
				m := recorded("m-pat", "/search", jsonHdr, `{"user":{"id":"123"}}`)
				m.Spec.ReqBodyNoise = map[string][]string{"body.user.id": {}}
				userBodyNoise := map[string][]string{"id": {`^\d+$`}}
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

				live := `{"user":{"id":"bad"}}`
				ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, live), db, nil, userBodyNoise, nil, true, false, true, true, true)
				if err != nil || ok {
					t.Fatalf("expected miss when live value violates user pattern; ok=%v err=%v", ok, err)
				}
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, userBodyNoise, diag)
				if len(r.FieldDiffs) != 1 || r.FieldDiffs[0].Path != "body.user.id" || r.FieldDiffs[0].Kind != models.DiffKindValueChanged {
					t.Fatalf("expected body.user.id value_changed; got %+v (diff=%q)", r.FieldDiffs, r.Diff)
				}
			})

			t.Run("missing learned field with redacted recorded value never leaks secret", func(t *testing.T) {
				const secret = "SECRET_TOKEN_98765"
				m := recorded("m-redacted", "/search", jsonHdr, `{"q":"ada","ts":"`+secret+`"}`)
				m.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
				m.Noise = []string{`^` + secret + `$`}
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

				// Drift q as well so a redacted ts alone does not auto-match via noise-aware exact match.
				live := `{"q":"bob"}`
				ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, live), db, nil, nil, nil, true, false, true, true, true)
				if err != nil {
					t.Fatalf("match error: %v", err)
				}
				if ok {
					t.Fatal("expected miss when q differs")
				}
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
				if strings.Contains(r.Diff, secret) {
					t.Fatalf("secret leaked in Diff: %q", r.Diff)
				}
				var sawTS bool
				for _, d := range r.FieldDiffs {
					if strings.Contains(d.Expected, secret) || strings.Contains(d.Actual, secret) {
						t.Fatalf("secret leaked in FieldDiff %+v", d)
					}
					if d.Path == "body.ts" && d.Kind == models.DiffKindMissingInLive {
						sawTS = true
					}
				}
				if mode.env == "" && !sawTS {
					t.Fatalf("expected body.ts missing_in_live in FieldDiffs; got %+v", r.FieldDiffs)
				}
			})

			t.Run("pattern-guarded field inside learned container changing shape stays type_changed", func(t *testing.T) {
				for _, tc := range []struct {
					name, rec, live            string
					wantExpUnset, wantActUnset string
				}{
					{"scalar to object", `{"q":"a","meta":{"ts":3}}`, `{"q":"a","meta":{"ts":{"a":1}}}`, "number: 3", "object"},
					{"object to array", `{"q":"a","meta":{"ts":{"a":1}}}`, `{"q":"a","meta":{"ts":[1]}}`, "object", "array"},
				} {
					t.Run(tc.name, func(t *testing.T) {
						m := recorded("m-pat-shape", "/search", jsonHdr, tc.rec)
						m.Spec.ReqBodyNoise = map[string][]string{"body.meta": {}}
						userBodyNoise := map[string][]string{"ts": {`^[0-9]+$`}}
						db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

						ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, tc.live), db, nil, userBodyNoise, nil, true, false, true, true, true)
						if err != nil || ok {
							t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
						}
						r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", tc.live), []byte(tc.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, userBodyNoise, diag)
						if strings.Contains(r.Diff, "absent in live call") {
							t.Fatalf("miss falsely says live call lacks body.meta.ts: %q", r.Diff)
						}
						if len(r.FieldDiffs) != 1 || r.FieldDiffs[0].Path != "body.meta.ts" || r.FieldDiffs[0].Kind != models.DiffKindTypeChanged {
							t.Fatalf("want one body.meta.ts type_changed; got %+v (reasons %+v)", r.FieldDiffs, r.FieldReasons)
						}
						if mode.env == "" && (r.FieldDiffs[0].Expected != tc.wantExpUnset || r.FieldDiffs[0].Actual != tc.wantActUnset) {
							t.Fatalf("got (%q, %q); want (%q, %q)", r.FieldDiffs[0].Expected, r.FieldDiffs[0].Actual, tc.wantExpUnset, tc.wantActUnset)
						}
					})
				}
			})

			t.Run("pattern-guarded array field absent in one item stays missing_in_live", func(t *testing.T) {
				m := recorded("m-pat-arr", "/search", jsonHdr, `{"q":"a","items":[{"id":1},{"id":"a"}]}`)
				m.Spec.ReqBodyNoise = map[string][]string{"body.items[].id": {}}
				userBodyNoise := map[string][]string{"id": {`^\d+$`}}
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

				live := `{"q":"a","items":[{"id":1},{}]}`
				ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, live), db, nil, userBodyNoise, nil, true, false, true, true, true)
				if err != nil || ok {
					t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
				}
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, userBodyNoise, diag)
				var found bool
				for _, d := range r.FieldDiffs {
					if d.Path == "body.items[].id" {
						if d.Kind != models.DiffKindMissingInLive {
							t.Fatalf("body.items[].id got kind=%s (%q -> %q), want missing_in_live (reasons %+v)", d.Kind, d.Expected, d.Actual, r.FieldReasons)
						}
						found = true
					}
				}
				if !found {
					t.Fatalf("expected body.items[].id missing_in_live; got %+v", r.FieldDiffs)
				}
			})

			t.Run("type_changed exact obfuscator placeholder is redacted across length boundaries", func(t *testing.T) {
				makePH := func(n int) string {
					const head = "OBFPH77"
					return head + strings.Repeat("q", n-len(head))
				}
				const shortPH = "OBF_PLACEHOLDER_77"
				type redactCase struct {
					name, rec, live string
					learned         map[string][]string
					ph              string
					wantPath        string
					wantKind        models.MockFieldDiffKind
					wantActual      string
				}
				cases := []redactCase{
					{
						name:       "no learned noise: object with redacted child sent as placeholder",
						rec:        `{"q":"ada","meta":{"s":"` + shortPH + `"}}`,
						live:       `{"q":"bob","meta":"` + shortPH + `"}`,
						ph:         shortPH,
						wantPath:   "body.meta",
						wantKind:   models.DiffKindTypeChanged,
						wantActual: "string: " + valueRedacted,
					},
					{
						name:       "no learned noise: array with redacted child sent as placeholder",
						rec:        `{"q":"ada","meta":[{"s":"` + shortPH + `"}]}`,
						live:       `{"q":"bob","meta":"` + shortPH + `"}`,
						ph:         shortPH,
						wantPath:   "body.meta",
						wantKind:   models.DiffKindTypeChanged,
						wantActual: "string: " + valueRedacted,
					},
					{
						name:       "learned object with redacted child sent as placeholder",
						rec:        `{"q":"ada","meta":{"s":"` + shortPH + `"}}`,
						live:       `{"q":"bob","meta":"` + shortPH + `"}`,
						learned:    map[string][]string{"body.meta": {}},
						ph:         shortPH,
						wantPath:   "body.meta",
						wantKind:   models.DiffKindTypeChanged,
						wantActual: "string: " + valueRedacted,
					},
					{
						name:       "non-learned scalar retyped to placeholder (P16)",
						rec:        `{"q":"ada","x":1}`,
						live:       `{"q":"bob","x":"` + shortPH + `"}`,
						ph:         shortPH,
						wantPath:   "body.x",
						wantKind:   models.DiffKindTypeChanged,
						wantActual: "string: " + valueRedacted,
					},
				}
				// Boundary table around HTTP's 256-byte truncation cut (including the
				// 8-byte "string: " prefix boundary at 248/249 and past-256 at 257/300).
				for _, length := range []int{247, 248, 249, 255, 256, 257, 300} {
					ph := makePH(length)
					cases = append(cases,
						redactCase{
							name:       fmt.Sprintf("object parent %dB placeholder", length),
							rec:        `{"q":"ada","meta":{"s":"` + ph + `"}}`,
							live:       `{"q":"bob","meta":"` + ph + `"}`,
							ph:         ph,
							wantPath:   "body.meta",
							wantKind:   models.DiffKindTypeChanged,
							wantActual: "string: " + valueRedacted,
						},
						redactCase{
							name:       fmt.Sprintf("array parent %dB placeholder", length),
							rec:        `{"q":"ada","meta":[{"s":"` + ph + `"}]}`,
							live:       `{"q":"bob","meta":"` + ph + `"}`,
							ph:         ph,
							wantPath:   "body.meta",
							wantKind:   models.DiffKindTypeChanged,
							wantActual: "string: " + valueRedacted,
						},
						redactCase{
							name:       fmt.Sprintf("learned object parent %dB placeholder", length),
							rec:        `{"q":"ada","meta":{"s":"` + ph + `"}}`,
							live:       `{"q":"bob","meta":"` + ph + `"}`,
							learned:    map[string][]string{"body.meta": {}},
							ph:         ph,
							wantPath:   "body.meta",
							wantKind:   models.DiffKindTypeChanged,
							wantActual: "string: " + valueRedacted,
						},
						redactCase{
							name:       fmt.Sprintf("missing_in_mock %dB placeholder", length),
							rec:        `{"q":"ada"}`,
							live:       `{"q":"bob","extra":"` + ph + `"}`,
							ph:         ph,
							wantPath:   "body.extra",
							wantKind:   models.DiffKindMissingInMock,
							wantActual: valueRedacted,
						},
						redactCase{
							name:       fmt.Sprintf("value_changed %dB placeholder", length),
							rec:        `{"q":"ada","x":"ok"}`,
							live:       `{"q":"bob","x":"` + ph + `"}`,
							ph:         ph,
							wantPath:   "body.x",
							wantKind:   models.DiffKindValueChanged,
							wantActual: valueRedacted,
						},
					)
				}
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						m := recorded("m-redact-type", "/search", jsonHdr, tc.rec)
						m.Spec.ReqBodyNoise = tc.learned
						m.Noise = []string{"^" + regexp.QuoteMeta(tc.ph) + "$"}
						db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
						ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, tc.live), db, nil, nil, nil, true, false, true, true, true)
						if err != nil || ok {
							t.Fatalf("want miss, got ok=%v err=%v", ok, err)
						}
						r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", tc.live), []byte(tc.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
						needle := tc.ph
						if len(needle) > 40 {
							needle = needle[:40]
						}
						if strings.Contains(r.Diff, needle) || strings.Contains(r.NextSteps, needle) {
							t.Fatalf("placeholder leaked in report: diff=%q next=%q", r.Diff, r.NextSteps)
						}
						var sawTarget bool
						for _, d := range r.FieldDiffs {
							if strings.Contains(d.Expected, needle) || strings.Contains(d.Actual, needle) {
								t.Fatalf("placeholder leaked in FieldDiff %+v", d)
							}
							if d.Path == tc.wantPath && d.Kind == tc.wantKind {
								sawTarget = true
								if mode.env == "" && d.Actual != tc.wantActual {
									t.Fatalf("want redacted Actual %q, got %+v", tc.wantActual, d)
								}
							}
						}
						if !sawTarget {
							t.Fatalf("expected %s %s in %+v", tc.wantPath, tc.wantKind, r.FieldDiffs)
						}
					})
				}
			})

			t.Run("long JWT crossing 256B cut is redacted before truncation", func(t *testing.T) {
				const jwtNeedle = "eyJhbGciOiJI"
				type jwtCase struct {
					name, rec, live string
					wantPath        string
					wantKind        models.MockFieldDiffKind
					wantActual      string
				}
				var cases []jwtCase
				// Header+payload prefix is 24 bytes ("eyJhbGciOiJIUzI1NiJ9.eyJ"), so
				// payloadLen 232 (269B JWT), 260 (297B JWT), and 300 (337B JWT) all
				// cross the 256-byte cut before the ".signature" segment completes.
				for _, payloadLen := range []int{232, 260, 300} {
					jwt := "eyJhbGciOiJIUzI1NiJ9.eyJ" + strings.Repeat("a", payloadLen) + ".c2lnbmF0dXJl"
					total := len(jwt)
					cases = append(cases,
						jwtCase{
							name:       fmt.Sprintf("value_changed %dB JWT", total),
							rec:        `{"q":"ada","x":"ok"}`,
							live:       `{"q":"bob","x":"` + jwt + `"}`,
							wantPath:   "body.x",
							wantKind:   models.DiffKindValueChanged,
							wantActual: valueRedacted,
						},
						jwtCase{
							name:       fmt.Sprintf("retyped number %dB JWT", total),
							rec:        `{"q":"ada","x":1}`,
							live:       `{"q":"bob","x":"` + jwt + `"}`,
							wantPath:   "body.x",
							wantKind:   models.DiffKindTypeChanged,
							wantActual: "string: " + valueRedacted,
						},
						jwtCase{
							name:       fmt.Sprintf("missing_in_mock %dB JWT", total),
							rec:        `{"q":"ada"}`,
							live:       `{"q":"bob","extra":"` + jwt + `"}`,
							wantPath:   "body.extra",
							wantKind:   models.DiffKindMissingInMock,
							wantActual: valueRedacted,
						},
					)
				}
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						m := recorded("m-redact-jwt", "/search", jsonHdr, tc.rec)
						db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
						ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, tc.live), db, nil, nil, nil, true, false, true, true, true)
						if err != nil || ok {
							t.Fatalf("want miss, got ok=%v err=%v", ok, err)
						}
						r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", tc.live), []byte(tc.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, diag)
						if strings.Contains(r.Diff, jwtNeedle) || strings.Contains(r.NextSteps, jwtNeedle) || strings.Contains(r.ReceivedReq, jwtNeedle) {
							t.Fatalf("JWT leaked in report: diff=%q next=%q received=%q", r.Diff, r.NextSteps, r.ReceivedReq)
						}
						var sawTarget bool
						for _, d := range r.FieldDiffs {
							if strings.Contains(d.Expected, jwtNeedle) || strings.Contains(d.Actual, jwtNeedle) {
								t.Fatalf("JWT leaked in FieldDiff %+v", d)
							}
							if d.Path == tc.wantPath && d.Kind == tc.wantKind {
								sawTarget = true
								if mode.env == "" && d.Actual != tc.wantActual {
									t.Fatalf("want redacted Actual %q, got %+v", tc.wantActual, d)
								}
							}
						}
						if !sawTarget {
							t.Fatalf("expected %s %s in %+v", tc.wantPath, tc.wantKind, r.FieldDiffs)
						}
					})
				}
			})

			t.Run("folded array path with two refusal kinds keeps both in FieldDiffs", func(t *testing.T) {
				rec := `{"q":"a","items":[{"ts":1},{"ts":null}]}`
				live := `{"q":"a","items":[{},{"ts":false}]}`
				userBodyNoise := map[string][]string{"ts": {`^[0-9]+$`}}
				m := recorded("m-fold", "/search", jsonHdr, rec)
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

				ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, live), db, nil, userBodyNoise, nil, true, false, true, true, true)
				if err != nil || ok {
					t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
				}
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, userBodyNoise, diag)
				kinds := map[models.MockFieldDiffKind]int{}
				for _, d := range r.FieldDiffs {
					if d.Path == "body.items[].ts" {
						kinds[d.Kind]++
					}
				}
				if kinds[models.DiffKindMissingInLive] != 1 || kinds[models.DiffKindTypeChanged] != 1 {
					t.Fatalf("want body.items[].ts both missing_in_live and type_changed once each; got diffs=%+v reasons=%+v", r.FieldDiffs, r.FieldReasons)
				}
				const wantDiff = "body.items[].ts: absent in live call; body.items[].ts: type changed (recorded null, live boolean)"
				if r.Diff != wantDiff {
					t.Fatalf("got Diff=%q; want %q (diffs=%+v)", r.Diff, wantDiff, r.FieldDiffs)
				}
			})

			t.Run("pattern-guarded missing field preserves kernel recorded or live value (G6/G7)", func(t *testing.T) {
				for _, tc := range []struct {
					name            string
					rec, live       string
					userBodyNoise   map[string][]string
					wantPath        string
					wantKind        models.MockFieldDiffKind
					wantDiffUnset   string
					wantDiffProven1 string
				}{
					{
						name:            "G6 nested object field absent in live",
						rec:             `{"q":"a","meta":{"ts":7}}`,
						live:            `{"q":"a","meta":{}}`,
						userBodyNoise:   map[string][]string{"meta.ts": {`^[0-9]+$`}},
						wantPath:        "body.meta.ts",
						wantKind:        models.DiffKindMissingInLive,
						wantDiffUnset:   `body.meta.ts: recorded "7", absent in live call`,
						wantDiffProven1: `body.meta.ts: absent in live call`,
					},
					{
						name:            "G7 one-item array field absent in live",
						rec:             `{"q":"a","items":[{"ts":1}]}`,
						live:            `{"q":"a","items":[{}]}`,
						userBodyNoise:   map[string][]string{"ts": {`^[0-9]+$`}},
						wantPath:        "body.items[].ts",
						wantKind:        models.DiffKindMissingInLive,
						wantDiffUnset:   `body.items[].ts: recorded "1", absent in live call`,
						wantDiffProven1: `body.items[].ts: absent in live call`,
					},
					{
						name:            "nested object field absent in recording",
						rec:             `{"q":"a","meta":{}}`,
						live:            `{"q":"b","meta":{"ts":7}}`,
						userBodyNoise:   map[string][]string{"meta.ts": {`^[0-9]+$`}},
						wantPath:        "body.meta.ts",
						wantKind:        models.DiffKindMissingInMock,
						wantDiffUnset:   `body.q: recorded "a", live "b"; body.meta.ts: live "7", absent in recording`,
						wantDiffProven1: `body.meta.ts: absent in recording; body.q: recorded "a", live "b"`,
					},
				} {
					t.Run(tc.name, func(t *testing.T) {
						m := recorded("m-pat-miss", "/search", jsonHdr, tc.rec)
						db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

						ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, tc.live), db, nil, tc.userBodyNoise, nil, true, false, true, true, true)
						if err != nil || ok {
							t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
						}
						r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", tc.live), []byte(tc.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, tc.userBodyNoise, diag)
						var sawTarget bool
						for _, d := range r.FieldDiffs {
							if d.Path == tc.wantPath && d.Kind == tc.wantKind {
								sawTarget = true
							}
						}
						if !sawTarget {
							t.Fatalf("got FieldDiffs=%+v; want %s %s", r.FieldDiffs, tc.wantPath, tc.wantKind)
						}
						wantDiff := tc.wantDiffUnset
						if mode.env != "" {
							wantDiff = tc.wantDiffProven1
						}
						if r.Diff != wantDiff {
							t.Fatalf("got Diff=%q; want %q (diffs=%+v)", r.Diff, wantDiff, r.FieldDiffs)
						}
					})
				}
			})

			t.Run("pattern-guarded scalar type_changed preserves kernel typed values and redacts placeholders (G9/G8/G2)", func(t *testing.T) {
				longPH := "OBFPH77" + strings.Repeat("q", 293) // 300 bytes
				for _, tc := range []struct {
					name            string
					rec, live       string
					userBodyNoise   map[string][]string
					mockNoise       []string
					leakNeedle      string
					wantPath        string
					wantDiffUnset   string
					wantDiffProven1 string
					// wantStrictProven1 is, under the proven switch, the
					// strict comparison's own diffs (diag.differs) as the
					// report renders them, before the report folds each
					// path and kind into its first diff (the mismatch
					// builder's foldDiffs): every element of a folded array
					// keeps its own types there. Empty: not checked.
					wantStrictProven1 string
				}{
					{
						name:            "G9 top-level scalar type_changed keeps values",
						rec:             `{"q":"a","ts":1}`,
						live:            `{"q":"a","ts":"x"}`,
						userBodyNoise:   map[string][]string{"ts": {`^[0-9]+$`}},
						wantPath:        "body.ts",
						wantDiffUnset:   "body.ts: type changed (recorded number: 1, live string: x)",
						wantDiffProven1: "body.ts: type changed (recorded number, live string)",
					},
					{
						name:            "G8 one-item array scalar type_changed keeps values",
						rec:             `{"q":"a","items":[{"ts":1}]}`,
						live:            `{"q":"a","items":[{"ts":"x"}]}`,
						userBodyNoise:   map[string][]string{"ts": {`^[0-9]+$`}},
						wantPath:        "body.items[].ts",
						wantDiffUnset:   "body.items[].ts: type changed (recorded number: 1, live string: x)",
						wantDiffProven1: "body.items[].ts: type changed (recorded number, live string)",
					},
					{
						name:            "G2 scalar retyped to short placeholder stays redacted",
						rec:             `{"q":"a","ts":1}`,
						live:            `{"q":"a","ts":"OBF_PLACEHOLDER_77"}`,
						userBodyNoise:   map[string][]string{"ts": {`^[0-9]+$`}},
						mockNoise:       []string{`^OBF_PLACEHOLDER_77$`},
						leakNeedle:      "OBF_PLACEHOLDER_77",
						wantPath:        "body.ts",
						wantDiffUnset:   "body.ts: type changed (recorded number: 1, live string: (redacted))",
						wantDiffProven1: "body.ts: type changed (recorded number, live string)",
					},
					{
						name:            "G2 scalar retyped to 300B placeholder stays redacted before cut",
						rec:             `{"q":"a","ts":1}`,
						live:            `{"q":"a","ts":"` + longPH + `"}`,
						userBodyNoise:   map[string][]string{"ts": {`^[0-9]+$`}},
						mockNoise:       []string{"^" + regexp.QuoteMeta(longPH) + "$"},
						leakNeedle:      longPH[:40],
						wantPath:        "body.ts",
						wantDiffUnset:   "body.ts: type changed (recorded number: 1, live string: (redacted))",
						wantDiffProven1: "body.ts: type changed (recorded number, live string)",
					},
					{
						name:              "folded array with mismatched expected type does not copy wrong element values",
						rec:               `{"q":"a","items":[{"ts":1},{"ts":true}]}`,
						live:              `{"q":"a","items":[{"ts":"x"},{"ts":"y"}]}`,
						userBodyNoise:     map[string][]string{"ts": {`^[0-9]+$`}},
						wantPath:          "body.items[].ts",
						wantDiffUnset:     "body.items[].ts: type changed (recorded number, live string)",
						wantDiffProven1:   "body.items[].ts: type changed (recorded number, live string)",
						wantStrictProven1: "body.items[].ts: type changed (recorded number, live string); body.items[].ts: type changed (recorded boolean, live string)",
					},
					{
						name:              "folded array with mismatched actual type does not copy wrong element values",
						rec:               `{"q":"a","items":[{"ts":1},{"ts":2}]}`,
						live:              `{"q":"a","items":[{"ts":"x"},{"ts":false}]}`,
						userBodyNoise:     map[string][]string{"ts": {`^[0-9]+$`}},
						wantPath:          "body.items[].ts",
						wantDiffUnset:     "body.items[].ts: type changed (recorded number, live string)",
						wantDiffProven1:   "body.items[].ts: type changed (recorded number, live string)",
						wantStrictProven1: "body.items[].ts: type changed (recorded number, live string); body.items[].ts: type changed (recorded number, live boolean)",
					},
				} {
					t.Run(tc.name, func(t *testing.T) {
						m := recorded("m-pat-type", "/search", jsonHdr, tc.rec)
						m.Noise = tc.mockNoise
						db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

						ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, tc.live), db, nil, tc.userBodyNoise, nil, true, false, true, true, true)
						if err != nil || ok {
							t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
						}
						r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", tc.live), []byte(tc.live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, tc.userBodyNoise, diag)
						if tc.leakNeedle != "" && strings.Contains(r.Diff, tc.leakNeedle) {
							t.Fatalf("placeholder leaked in Diff: %q", r.Diff)
						}
						wantDiff := tc.wantDiffUnset
						if mode.env != "" {
							wantDiff = tc.wantDiffProven1
						}
						if r.Diff != wantDiff {
							t.Fatalf("got Diff=%q; want %q (diffs=%+v)", r.Diff, wantDiff, r.FieldDiffs)
						}
						if mode.env != "" && tc.wantStrictProven1 != "" {
							if got := mismatch.RenderFieldDiffs(fieldDiffs(diag.differs)); got != tc.wantStrictProven1 {
								t.Fatalf("strict comparison's diffs = %q; want %q", got, tc.wantStrictProven1)
							}
						}
					})
				}
			})

			t.Run("folded array path dedupes duplicate refusals of the same kind", func(t *testing.T) {
				rec := `{"q":"a","items":[{"ts":1},{"ts":2}]}`
				live := `{"q":"a","items":[{},{}]}`
				userBodyNoise := map[string][]string{"ts": {`^[0-9]+$`}}
				m := recorded("m-fold-dup", "/search", jsonHdr, rec)
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

				ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, live), db, nil, userBodyNoise, nil, true, false, true, true, true)
				if err != nil || ok {
					t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
				}
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, userBodyNoise, diag)
				var count int
				for _, d := range r.FieldDiffs {
					if d.Path == "body.items[].ts" && d.Kind == models.DiffKindMissingInLive {
						count++
					}
				}
				if mode.env == "" && count != 1 {
					t.Fatalf("want exactly 1 deduped body.items[].ts missing_in_live diff; got %+v", r.FieldDiffs)
				}
			})

			t.Run("pattern-guarded refusal at body.meta.ts does not delete sibling body.meta.ts2 diff", func(t *testing.T) {
				rec := `{"q":"a","meta":{"ts":3,"ts2":"a"}}`
				live := `{"q":"a","meta":{"ts":{"a":1},"ts2":"b"}}`
				userBodyNoise := map[string][]string{"meta.ts": {`^[0-9]+$`}}
				m := recorded("m-pat-sib", "/search", jsonHdr, rec)
				db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}

				ok, _, diag, err := newHTTP().match(context.Background(), sent("/search", jsonHdr, live), db, nil, userBodyNoise, nil, true, false, true, true, true)
				if err != nil || ok {
					t.Fatalf("expected miss, got ok=%v err=%v", ok, err)
				}
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, userBodyNoise, diag)
				var sawTS, sawTS2 bool
				for _, d := range r.FieldDiffs {
					if d.Path == "body.meta.ts" && d.Kind == models.DiffKindTypeChanged {
						sawTS = true
					}
					if d.Path == "body.meta.ts2" && d.Kind == models.DiffKindValueChanged {
						sawTS2 = true
					}
				}
				if !sawTS || !sawTS2 {
					t.Fatalf("want both body.meta.ts (type_changed) and sibling body.meta.ts2 (value_changed); got %+v", r.FieldDiffs)
				}
			})

			t.Run("fallback buildHTTPMismatchReport without diag.closest uses type-strict body diffs", func(t *testing.T) {
				m := recorded("m-fallback", "/search", jsonHdr, `{"q":"ada","ts":1}`)
				m.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}}
				live := `{"q":"ada"}`
				r := newHTTP().buildHTTPMismatchReport(context.Background(), makeJSONPostRequest("/search", live), []byte(live), &mockMemDb{mocks: []*models.Mock{m}}, nil, nil, nil, nil)
				if r == nil || len(r.FieldDiffs) != 1 || r.FieldDiffs[0].Path != "body.ts" || r.FieldDiffs[0].Kind != models.DiffKindMissingInLive || r.FieldDiffs[0].Expected != "1" {
					t.Fatalf("expected body.ts missing_in_live expected=1; got %+v", r)
				}
			})
		})
	}
}

func makeJSONPostRequest(path, body string) *http.Request {
	req, err := http.NewRequest("POST", "http://api.example"+path, strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", "api.example")
	req.Host = "api.example"
	return req
}

package matcher

import (
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// subsKeyMatchWithOriginal is a test helper that lets subtests pass
// CamelCase noise-map keys directly. It builds the lowered+merged
// companion map locally (same pattern as CompareHeaders in production)
// and delegates to SubstringKeyMatch. Not a contract — SubstringKeyMatch
// is idempotent to pre-lowering now, so tests could just as well call
// SubstringKeyMatch directly; this helper just keeps the subtests' data
// tables readable with CamelCase inputs.
func subsKeyMatchWithOriginal(s string, mp map[string][]string) ([]string, bool) {
	lowered := make(map[string][]string, len(mp))
	for k, v := range mp {
		lk := strings.ToLower(k)
		if existing, ok := lowered[lk]; ok {
			lowered[lk] = slices.Concat(existing, v)
		} else {
			lowered[lk] = slices.Clone(v)
		}
	}
	return SubstringKeyMatch(s, lowered)
}

// TestSubstringKeyMatch_CaseInsensitive verifies that SubstringKeyMatch treats
// both the header key and the noise-pattern key as case-insensitive. This
// guards against a historical regression where callers already lower-cased
// the incoming header key, but the noise-map key (as authored in keploy.yml)
// was compared verbatim, so a CamelCase noise pattern like "X-Correlation-Id"
// silently failed to match the already-lowercased header "x-correlation-id".
func TestSubstringKeyMatch_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name      string
		s         string
		noise     map[string][]string
		wantMatch bool
		wantVals  []string
	}{
		{
			name:      "CamelCase header matches lowercase noise pattern",
			s:         "X-Correlation-Id",
			noise:     map[string][]string{"x-correlation-id": {}},
			wantMatch: true,
			wantVals:  []string{},
		},
		{
			name:      "lowercase header matches CamelCase noise pattern",
			s:         "x-correlation-id",
			noise:     map[string][]string{"X-Correlation-Id": {}},
			wantMatch: true,
			wantVals:  []string{},
		},
		{
			name:      "Content-Type vs content-type matches",
			s:         "Content-Type",
			noise:     map[string][]string{"content-type": {}},
			wantMatch: true,
			wantVals:  []string{},
		},
		{
			name:      "unrelated header does not match correlation-id pattern",
			s:         "X-Different",
			noise:     map[string][]string{"x-correlation-id": {}},
			wantMatch: false,
			wantVals:  []string{},
		},
		{
			name:      "mixed-case header with regex payload preserves value",
			s:         "X-Request-Id",
			noise:     map[string][]string{"x-request-id": {".*"}},
			wantMatch: true,
			wantVals:  []string{".*"},
		},
		{
			name:      "empty noise map never matches",
			s:         "X-Correlation-Id",
			noise:     map[string][]string{},
			wantMatch: false,
			wantVals:  []string{},
		},
		{
			name:      "substring semantics: short noise key matches longer header",
			s:         "X-My-Correlation-Id-Extra",
			noise:     map[string][]string{"CORRELATION-ID": {}},
			wantMatch: true,
			wantVals:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := subsKeyMatchWithOriginal(tt.s, tt.noise)
			if ok != tt.wantMatch {
				t.Fatalf("SubstringKeyMatch(%q, %v) matched=%v, want %v",
					tt.s, tt.noise, ok, tt.wantMatch)
			}
			if len(got) != len(tt.wantVals) {
				t.Fatalf("SubstringKeyMatch(%q, ...) returned vals=%v, want %v",
					tt.s, got, tt.wantVals)
			}
			for i := range got {
				if got[i] != tt.wantVals[i] {
					t.Fatalf("SubstringKeyMatch(%q, ...) vals[%d]=%q, want %q",
						tt.s, i, got[i], tt.wantVals[i])
				}
			}
		})
	}
}

// TestSubstringKeyMatch_DirectUnNormalized pins down SubstringKeyMatch's
// "both sides case-insensitive" contract without going through the
// subsKeyMatchWithOriginal helper (which lowercases noise-map keys before
// delegating). Calling the function directly with an un-normalized
// (CamelCase / MIXED-CASE) noise map proves that SubstringKeyMatch itself
// performs the case folding on the map-key side — not merely on the
// incoming header string. If someone regresses SubstringKeyMatch back to
// verbatim map-key comparison, these assertions fail immediately.
func TestSubstringKeyMatch_DirectUnNormalized(t *testing.T) {
	tests := []struct {
		name      string
		s         string
		mp        map[string][]string
		wantVals  []string
		wantMatch bool
	}{
		{
			name:      "camel pattern vs lower input",
			s:         "x-correlation-id",
			mp:        map[string][]string{"X-Correlation-Id": {"val-a"}},
			wantVals:  []string{"val-a"},
			wantMatch: true,
		},
		{
			name:      "lower pattern vs camel input",
			s:         "X-Correlation-Id",
			mp:        map[string][]string{"x-correlation-id": {"val-b"}},
			wantVals:  []string{"val-b"},
			wantMatch: true,
		},
		{
			name:      "all-upper pattern vs all-upper input",
			s:         "CORRELATION-ID",
			mp:        map[string][]string{"CORRELATION-ID": {"val-c"}},
			wantVals:  []string{"val-c"},
			wantMatch: true,
		},
		{
			name:      "mixed-case pattern vs mixed-case input (different casings)",
			s:         "X-rEqUeSt-Id",
			mp:        map[string][]string{"X-Request-ID": {"val-d"}},
			wantVals:  []string{"val-d"},
			wantMatch: true,
		},
		{
			name:      "no match preserved with camel-case map key",
			s:         "X-Other",
			mp:        map[string][]string{"X-Correlation-Id": {"val-e"}},
			wantVals:  []string{},
			wantMatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			val, ok := SubstringKeyMatch(tc.s, tc.mp)
			if ok != tc.wantMatch {
				t.Fatalf("SubstringKeyMatch(%q, %v) matched=%v, want %v",
					tc.s, tc.mp, ok, tc.wantMatch)
			}
			if len(val) != len(tc.wantVals) {
				t.Fatalf("SubstringKeyMatch(%q, %v) vals=%v, want %v",
					tc.s, tc.mp, val, tc.wantVals)
			}
			for i := range val {
				if val[i] != tc.wantVals[i] {
					t.Fatalf("SubstringKeyMatch(%q, %v) vals[%d]=%q, want %q",
						tc.s, tc.mp, i, val[i], tc.wantVals[i])
				}
			}
		})
	}
}

// TestSubstringKeyMatch_GlobSuffixLiteral documents that SubstringKeyMatch does
// NOT interpret glob metacharacters (e.g. "x-*") — it does a literal substring
// match. A pattern like "x-*" will therefore only match a header that literally
// contains the "*" character, never an arbitrary "x-..." header. This is an
// intentional simplification; callers that want glob matching should use the
// regex-aware code paths (JSONDiffWithNoiseControl / noiseIndex.match).
func TestSubstringKeyMatch_GlobSuffixLiteral(t *testing.T) {
	noise := map[string][]string{"x-*": {}}

	// An arbitrary "x-..." header must NOT match the glob pattern — the "*" is
	// treated as a literal character, not a wildcard.
	if _, ok := subsKeyMatchWithOriginal("X-Correlation-Id", noise); ok {
		t.Fatalf("SubstringKeyMatch should not treat '*' as a glob; got match for X-Correlation-Id vs x-*")
	}

	// But when the header genuinely contains "x-*" (literal), it does match.
	if _, ok := subsKeyMatchWithOriginal("some-X-*-thing", noise); !ok {
		t.Fatalf("SubstringKeyMatch should match literal '*' substring; missed some-X-*-thing vs x-*")
	}
}

// findHeaderResult returns the HeaderResult whose expected or actual key matches k.
func findHeaderResult(res []models.HeaderResult, k string) (models.HeaderResult, bool) {
	for _, r := range res {
		if r.Expected.Key == k || r.Actual.Key == k {
			return r, true
		}
	}
	return models.HeaderResult{}, false
}

// TestCompareHeaders_CamelCaseNoiseKey is the focused regression test for the
// originally reported bug: a user authors a noise key in keploy.yml using the
// natural HTTP-header casing (e.g. "X-Correlation-Id") and expects
// CompareHeaders to treat that header as noise — i.e. differing values must
// NOT flip the overall match result to false. Before the case-insensitive
// fix, the CamelCase noise pattern silently failed to match the already-
// lowercased http.Header canonicalization, and the test would fail.
func TestCompareHeaders_CamelCaseNoiseKey(t *testing.T) {
	h1 := http.Header{}
	h1.Set("X-Correlation-Id", "req-aaaaaaaa")

	h2 := http.Header{}
	h2.Set("X-Correlation-Id", "req-bbbbbbbb") // different value — should be ignored

	// Note the CamelCase noise key, exactly as a user would author it.
	noise := map[string][]string{"X-Correlation-Id": {}}

	var res []models.HeaderResult
	ok := CompareHeaders(h1, h2, &res, noise)
	if !ok {
		t.Fatalf("CompareHeaders should return true when differing header is noise; got false (res=%+v)", res)
	}

	got, found := findHeaderResult(res, "X-Correlation-Id")
	if !found {
		t.Fatalf("expected a HeaderResult entry for X-Correlation-Id, got %+v", res)
	}
	if !got.Normal {
		t.Fatalf("expected Normal=true for noise-matched CamelCase key; got %+v", got)
	}
}

// TestLoweredNoise_MergesCaseCollisions verifies that when a user supplies two
// noise entries that differ only by case (e.g. "X-Request-Id" and
// "x-request-id"), CompareHeaders does not silently drop one of the regex
// slices. The expected behavior is that both regex patterns contribute — i.e.
// a header value matching EITHER pattern is still treated as noise. This
// pins down the collision-merge semantics independent of Go map iteration
// order.
func TestLoweredNoise_MergesCaseCollisions(t *testing.T) {
	h1 := http.Header{}
	h1.Set("X-Request-Id", "beta-12345") // matches the second (camel-case-authored) pattern only

	h2 := http.Header{}
	h2.Set("X-Request-Id", "beta-99999")

	// Two distinct noise entries keyed by case-only variants. If the builder
	// picked one and dropped the other (pre-fix behavior), then depending on
	// which regex survived, the "beta-..." value might not match and the
	// header would flip to non-noise → overall match=false.
	noise := map[string][]string{
		"x-request-id": {"^alpha-.*$"},
		"X-Request-Id": {"^beta-.*$"},
	}

	var res []models.HeaderResult
	ok := CompareHeaders(h1, h2, &res, noise)
	if !ok {
		t.Fatalf("CompareHeaders should treat value matching merged regex as noise; got false (res=%+v)", res)
	}

	got, found := findHeaderResult(res, "X-Request-Id")
	if !found {
		t.Fatalf("expected a HeaderResult entry for X-Request-Id, got %+v", res)
	}
	if !got.Normal {
		t.Fatalf("expected Normal=true after merging case-collided noise regex slices; got %+v", got)
	}

	// And the symmetric case: a value matching the OTHER pattern (alpha-*)
	// should also be treated as noise, proving the merge is bidirectional.
	h1b := http.Header{}
	h1b.Set("X-Request-Id", "alpha-abc")
	h2b := http.Header{}
	h2b.Set("X-Request-Id", "alpha-xyz")

	var resB []models.HeaderResult
	if ok := CompareHeaders(h1b, h2b, &resB, noise); !ok {
		t.Fatalf("CompareHeaders should treat alpha-* value as noise via merged slice; got false (res=%+v)", resB)
	}
}

// TestCompareHeaders_UppercaseHeaderSentinel verifies that the whole-section
// "header" noise sentinel is recognized case-insensitively — a user config
// with "Header" or "HEADER" must still mark every header as noise. This pins
// down the isHeaderNoisy-via-loweredNoise lookup fix.
func TestCompareHeaders_UppercaseHeaderSentinel(t *testing.T) {
	h1 := http.Header{}
	h1.Set("X-Anything", "val-a")

	h2 := http.Header{}
	h2.Set("X-Anything", "val-DIFFERENT")

	noise := map[string][]string{"Header": {}} // user-authored with title case

	var res []models.HeaderResult
	if ok := CompareHeaders(h1, h2, &res, noise); !ok {
		t.Fatalf("uppercase 'Header' sentinel should mark all headers noisy; got match=false (res=%+v)", res)
	}
}

func TestSplitNoise(t *testing.T) {
	cases := []struct {
		name       string
		noise      map[string][]string
		wantBody   map[string][]string
		wantHeader map[string][]string
		wantSkip   bool
	}{
		{
			name:       "dotted body key keeps its regex list",
			noise:      map[string][]string{"body.user.id": {".*"}},
			wantBody:   map[string][]string{"user.id": {".*"}},
			wantHeader: map[string][]string{},
		},
		{
			name:       "dotted header key is scoped to the header name",
			noise:      map[string][]string{"header.Date": {}},
			wantBody:   map[string][]string{},
			wantHeader: map[string][]string{"date": {}},
		},
		{
			name:       "sectioned body key lists paths, it does not skip the body",
			noise:      map[string][]string{"body": {"items.0.product.stock", "Total"}},
			wantBody:   map[string][]string{"items.0.product.stock": {}, "total": {}},
			wantHeader: map[string][]string{},
		},
		{
			name:       "sectioned header key lists header names",
			noise:      map[string][]string{"header": {"Content-Length"}},
			wantBody:   map[string][]string{},
			wantHeader: map[string][]string{"content-length": {}},
		},
		{
			name:       "empty body key is the whole-section sentinel",
			noise:      map[string][]string{"body": {}},
			wantBody:   map[string][]string{},
			wantHeader: map[string][]string{},
			wantSkip:   true,
		},
		{
			name:       "empty header key is the whole-section sentinel",
			noise:      map[string][]string{"header": {}},
			wantBody:   map[string][]string{},
			wantHeader: map[string][]string{"header": {}},
		},
		{
			name: "both shapes coexist",
			noise: map[string][]string{
				"body":            {"stock"},
				"body.created_at": {},
				"header":          {"Content-Length"},
				"header.Date":     {},
			},
			wantBody:   map[string][]string{"stock": {}, "created_at": {}},
			wantHeader: map[string][]string{"content-length": {}, "date": {}},
		},
		{
			name:       "unrelated sections are dropped",
			noise:      map[string][]string{"trailer.Foo": {}, "somethingelse": {"x"}},
			wantBody:   map[string][]string{},
			wantHeader: map[string][]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBody, gotHeader, gotSkip := SplitNoise(tc.noise, nil)
			if gotSkip != tc.wantSkip {
				t.Errorf("skipBody = %v, want %v", gotSkip, tc.wantSkip)
			}
			if !noiseMapsEqual(gotBody, tc.wantBody) {
				t.Errorf("bodyNoise = %v, want %v", gotBody, tc.wantBody)
			}
			if !noiseMapsEqual(gotHeader, tc.wantHeader) {
				t.Errorf("headerNoise = %v, want %v", gotHeader, tc.wantHeader)
			}
		})
	}
}

func noiseMapsEqual(got, want map[string][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok || len(gv) != len(wv) {
			return false
		}
		for i := range wv {
			if gv[i] != wv[i] {
				return false
			}
		}
	}
	return true
}

// TestSplitNoise_DottedWinsOverSectioned pins a deterministic merge when both
// shapes name the same path. Resolving it by map iteration order would make a
// run flaky rather than merely wrong.
func TestSplitNoise_DottedWinsOverSectioned(t *testing.T) {
	noise := map[string][]string{
		"body":         {"user.id"},
		"body.user.id": {"^[0-9]+$"},
		"header":       {"Date"},
		"header.Date":  {"^Mon"},
	}
	for i := 0; i < 200; i++ {
		body, header, _ := SplitNoise(noise, nil)
		if got := body["user.id"]; len(got) != 1 || got[0] != "^[0-9]+$" {
			t.Fatalf("iteration %d: bodyNoise[user.id] = %v, want the dotted entry's regex", i, got)
		}
		if got := header["date"]; len(got) != 1 || got[0] != "^Mon" {
			t.Fatalf("iteration %d: headerNoise[date] = %v, want the dotted entry's regex", i, got)
		}
	}
}

func TestSplitNoise_SectionNamesAreCaseInsensitive(t *testing.T) {
	body, header, skip := SplitNoise(map[string][]string{
		"Body":        {"Stock"},
		"Header.Date": {},
	}, nil)
	if _, ok := body["stock"]; !ok {
		t.Errorf("bodyNoise = %v, want a lowercased \"stock\" entry", body)
	}
	if _, ok := header["date"]; !ok {
		t.Errorf("headerNoise = %v, want a lowercased \"date\" entry", header)
	}
	if skip {
		t.Errorf("skipBody = true, want false")
	}
}

func TestCloneNoiseMap(t *testing.T) {
	original := map[string][]string{"a": {"x"}}
	clone := CloneNoiseMap(original)
	clone["b"] = []string{"y"}
	clone["a"][0] = "mutated"

	if _, ok := original["b"]; ok {
		t.Errorf("adding to the clone must not add to the original")
	}
	if original["a"][0] != "x" {
		t.Errorf("mutating a cloned value slice must not touch the original, got %v", original["a"])
	}
	if got := CloneNoiseMap(nil); got == nil || len(got) != 0 {
		t.Errorf("CloneNoiseMap(nil) = %v, want an empty non-nil map", got)
	}
}

// TestJSONDiffWithNoiseControl_IndexedPathIsNotSupported pins that the matcher
// deliberately does NOT paper over an indexed path.
//
// The JSON walker never adds an array position to its keys — every element of
// `items` is walked under the key "items" — so an index-free path works and an
// indexed one matches nothing. Normalizing the indexed form here by dropping
// numeric segments looks obvious and is a trap, twice over. Noise keys are
// matched with strings.Contains, so "items.0.id" would collapse to "items.id",
// a substring of "items.idempotency_key", and would silence it. And a string
// cannot tell an array position from a numeric object key, so "data.2026.count"
// would collapse to "data.count" and name a different field entirely.
//
// Producers get this right by construction instead: BodyNoiseFromJSONDiff walks
// the two documents in step with this walker, so it knows which numeric segments
// are positions because it can see the arrays. An entry recorded before that
// existed keeps failing loudly here, and WarnUnmatchableBodyNoise names it.
func TestJSONDiffWithNoiseControl_IndexedPathIsNotSupported(t *testing.T) {
	exp := `{"items":[{"product":{"name":"Laptop","stock":99}}]}`
	act := `{"items":[{"product":{"name":"Laptop","stock":100}}]}`

	t.Run("index-free path is honoured", func(t *testing.T) {
		e, a := exp, act
		v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
		if err != nil {
			t.Fatalf("ValidateAndMarshalJSON: %v", err)
		}
		res, err := JSONDiffWithNoiseControl(v, map[string][]string{"items.product.stock": {}}, false, nil)
		if err != nil {
			t.Fatalf("JSONDiffWithNoiseControl: %v", err)
		}
		if !res.IsExact() {
			t.Errorf("expected match: items.product.stock is the walker's own key syntax")
		}
	})

	t.Run("indexed path matches nothing", func(t *testing.T) {
		e, a := exp, act
		v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
		if err != nil {
			t.Fatalf("ValidateAndMarshalJSON: %v", err)
		}
		res, err := JSONDiffWithNoiseControl(v, map[string][]string{"items.0.product.stock": {}}, false, nil)
		if err != nil {
			t.Fatalf("JSONDiffWithNoiseControl: %v", err)
		}
		if res.IsExact() {
			t.Errorf("indexed noise paths are not supported yet; if this now passes, " +
				"the walker gained positions and the docs on SplitNoise need updating")
		}
	})

	t.Run("no normalization leak into a sibling", func(t *testing.T) {
		e := `{"items":[{"id":"1","idempotency_key":"k1"}]}`
		a := `{"items":[{"id":"1","idempotency_key":"TAMPERED"}]}`
		v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
		if err != nil {
			t.Fatalf("ValidateAndMarshalJSON: %v", err)
		}
		res, err := JSONDiffWithNoiseControl(v, map[string][]string{"items.0.id": {}}, false, nil)
		if err != nil {
			t.Fatalf("JSONDiffWithNoiseControl: %v", err)
		}
		if res.IsExact() {
			t.Errorf("idempotency_key must not be silenced by a noise path for id")
		}
	})
}

// TestJSONDiffWithNoiseControl_NumericObjectKey pins that a genuinely numeric
// object key stays scoped to itself and does not bleed into a same-named
// sibling one level up.
func TestJSONDiffWithNoiseControl_NumericObjectKey(t *testing.T) {
	exp := `{"data":{"2026":{"count":5},"count":7}}`
	act := `{"data":{"2026":{"count":5},"count":999}}`
	v, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act)
	if err != nil {
		t.Fatalf("ValidateAndMarshalJSON: %v", err)
	}
	res, err := JSONDiffWithNoiseControl(v, map[string][]string{"data.2026.count": {}}, false, nil)
	if err != nil {
		t.Fatalf("JSONDiffWithNoiseControl: %v", err)
	}
	if res.IsExact() {
		t.Errorf("data.count is a different field from data.2026.count and must still be compared")
	}
}

func TestJSONDiffWithNoiseControl_LargeIntegerPrecision(t *testing.T) {
	tests := []struct {
		name           string
		exp            string
		act            string
		noise          map[string][]string
		ignoreOrdering bool
		wantExact      bool
	}{
		{
			name:      "differing large integers above 2^53 (e.g. snowflake IDs) must not match",
			exp:       `{"id": 1234567890123456789}`,
			act:       `{"id": 1234567890123456700}`,
			wantExact: false,
		},
		{
			name:      "identical large integers above 2^53 must match",
			exp:       `{"id": 1234567890123456789}`,
			act:       `{"id": 1234567890123456789}`,
			wantExact: true,
		},
		{
			name:      "large integers with matching noise regex must pass",
			exp:       `{"id": 1234567890123456789}`,
			act:       `{"id": 1234567890123456700}`,
			noise:     map[string][]string{"id": {"^12345678901234567.*$"}},
			wantExact: true,
		},
		{
			name:      "mathematically equivalent number spellings match (1 vs 1.0)",
			exp:       `{"count": 1}`,
			act:       `{"count": 1.0}`,
			wantExact: true,
		},
		{
			name:      "scientific notation equivalent to integer matches (1e3 vs 1000)",
			exp:       `{"val": 1e3}`,
			act:       `{"val": 1000}`,
			wantExact: true,
		},
		{
			name:           "large integers in arrays with ignoreOrdering true match when elements are permutations",
			exp:            `{"ids": [1234567890123456789, 9876543210987654321]}`,
			act:            `{"ids": [9876543210987654321, 1234567890123456789]}`,
			ignoreOrdering: true,
			wantExact:      true,
		},
		{
			name:           "large integers in arrays with ignoreOrdering true fail when values differ slightly",
			exp:            `{"ids": [1234567890123456789, 9876543210987654321]}`,
			act:            `{"ids": [9876543210987654321, 1234567890123456700]}`,
			ignoreOrdering: true,
			wantExact:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, a := tc.exp, tc.act
			v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
			if err != nil {
				t.Fatalf("ValidateAndMarshalJSON: %v", err)
			}
			res, err := JSONDiffWithNoiseControl(v, tc.noise, tc.ignoreOrdering, zap.NewNop())
			if err != nil {
				t.Fatalf("JSONDiffWithNoiseControl: %v", err)
			}
			if got := res.IsExact(); got != tc.wantExact {
				t.Errorf("IsExact() = %v, want %v (differences: %v)", got, tc.wantExact, res.Differences())
			}
		})
	}
}

func TestJSONFieldDiffs_LargeIntegerPrecision(t *testing.T) {
	exp := `{"id": 1234567890123456789}`
	act := `{"id": 1234567890123456700}`
	diffs := JSONFieldDiffs(exp, act, nil, "body.", 0)
	if len(diffs) == 0 {
		t.Fatalf("JSONFieldDiffs should report diff for differing large integers, got 0 diffs")
	}
	if diffs[0].Expected != "1234567890123456789" || diffs[0].Actual != "1234567890123456700" {
		t.Errorf("JSONFieldDiffs large int formatting = (%q, %q), want exact integers", diffs[0].Expected, diffs[0].Actual)
	}

	// Decimals must render as human-readable JSON text (not rational fractions like "1/10").
	decDiffs := JSONFieldDiffs(`{"price": 0.1}`, `{"price": 19.99}`, nil, "body.", 0)
	if len(decDiffs) != 1 {
		t.Fatalf("JSONFieldDiffs decimal count = %d, want 1", len(decDiffs))
	}
	if decDiffs[0].Expected != "0.1" || decDiffs[0].Actual != "19.99" {
		t.Errorf("JSONFieldDiffs decimal formatting = (%q, %q), want (\"0.1\", \"19.99\")", decDiffs[0].Expected, decDiffs[0].Actual)
	}

	// Mathematically equal spellings must produce zero diffs.
	if eqDiffs := JSONFieldDiffs(`{"count": 1, "val": 1e3, "z": -0.0}`, `{"count": 1.0, "val": 1000, "z": 0}`, nil, "body.", 0); len(eqDiffs) != 0 {
		t.Errorf("JSONFieldDiffs reported unexpected diffs for equal numbers: %+v", eqDiffs)
	}

	// excludeRecordedValue receives human-readable decimal text.
	var seenVal string
	changed := ChangedJSONFieldPaths(`{"price": 0.1}`, `{"price": 0.2}`, nil, nil, false, func(v string) bool {
		seenVal = v
		return v == "0.1"
	})
	if seenVal != "0.1" || len(changed) != 0 {
		t.Errorf("ChangedJSONFieldPaths excludeRecordedValue saw %q, changed=%v; want \"0.1\" and nil", seenVal, changed)
	}
}

func TestCanonicalJSONNumber_HugeExponentsAndEdgeCases(t *testing.T) {
	// Huge scientific notation exponents must normalize in linear time without OOM/hang.
	if !equalJSONNumbers(json.Number("1e99999999"), json.Number("10e99999998")) {
		t.Errorf("1e99999999 should equal 10e99999998")
	}
	if equalJSONNumbers(json.Number("1e99999999"), json.Number("2e99999999")) {
		t.Errorf("1e99999999 should not equal 2e99999999")
	}
	if !equalJSONNumbers(json.Number("1e-99999999"), json.Number("0.1e-99999998")) {
		t.Errorf("1e-99999999 should equal 0.1e-99999998")
	}
	if !equalJSONNumbers(json.Number("0e99999999"), json.Number("-0.0")) {
		t.Errorf("0e99999999 should equal -0.0")
	}
	if !equalJSONNumbers(json.Number("1e99999999999999999999999"), json.Number("10e99999999999999999999998")) {
		t.Errorf("arbitrary-precision exponents should normalize accurately")
	}
	// Mixed float64 vs json.Number on non-dyadic decimals must match.
	for _, tc := range []struct {
		s string
		f float64
	}{
		{"0.1", 0.1},
		{"0.2", 0.2},
		{"19.99", 19.99},
		{"3.141592653589793", 3.141592653589793},
		{"-0.0", 0.0},
	} {
		if !equalJSONNumbers(json.Number(tc.s), tc.f) {
			t.Errorf("equalJSONNumbers(json.Number(%q), float64(%v)) = false, want true", tc.s, tc.f)
		}
	}
	// Non-JSON syntaxes must be rejected.
	for _, bad := range []string{"1/2", "0x10", "1_000", "+1", ".5", "1.", "01", ""} {
		if equalJSONNumbers(json.Number(bad), json.Number(bad)) {
			t.Errorf("invalid JSON number %q must not compare equal", bad)
		}
	}
}

func TestJsonContains_DecimalsArraysAndLargeIntegers(t *testing.T) {
	// Decimal float64 in expected map vs json.Number in actual JSON.
	ok, err := JsonContains(`{"price": 0.1, "tax": 19.99, "ids": [1, 2, 3], "items": [{"id": 1, "cost": 0.2}]}`, map[string]interface{}{
		"price": 0.1,
		"tax":   19.99,
		"ids":   []interface{}{float64(1), float64(2), float64(3)},
		"items": []interface{}{map[string]interface{}{"id": float64(1), "cost": 0.2}},
	})
	if err != nil || !ok {
		t.Fatalf("JsonContains decimals & numeric arrays = (%v, %v), want (true, nil)", ok, err)
	}

	// Large integer > 2^53 precision preserved in JsonContains.
	ok, err = JsonContains(`{"id": 1234567890123456789}`, map[string]interface{}{"id": json.Number("1234567890123456700")})
	if err != nil || ok {
		t.Fatalf("JsonContains differing large int = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = JsonContains(`{"id": 1234567890123456789}`, map[string]interface{}{"id": json.Number("1234567890123456789")})
	if err != nil || !ok {
		t.Fatalf("JsonContains matching large int = (%v, %v), want (true, nil)", ok, err)
	}

	// Empty actualJSON must return an error.
	if ok, err := JsonContains("", map[string]interface{}{"a": 1}); err == nil || ok {
		t.Errorf("JsonContains empty string = (%v, %v), want (false, non-nil err)", ok, err)
	}
}

func TestMixedFloat64AndJSONNumber_PreparedAndNoisePath(t *testing.T) {
	// NewValidatedJSON, JSONMatchKey, CollectJSONFields/CountJSONFieldDiffs with mixed float64 vs json.Number.
	expDoc := map[string]interface{}{"price": 0.1, "count": float64(1), "zero": math.Copysign(0, -1)}
	actDoc, err := UnmarshallJSON(`{"price": 0.1, "count": 1.0, "zero": 0}`, nil)
	if err != nil {
		t.Fatalf("UnmarshallJSON: %v", err)
	}
	vj := NewValidatedJSON(expDoc, actDoc)
	if !vj.IsIdentical() {
		t.Fatalf("NewValidatedJSON(float64, json.Number) IsIdentical = false, want true")
	}
	res, err := JSONDiffWithNoiseControl(vj, nil, false, zap.NewNop())
	if err != nil || !res.IsExact() {
		t.Fatalf("JSONDiffWithNoiseControl mixed types = (%v, %v), want exact match", res.IsExact(), err)
	}
	if JSONMatchKey(expDoc, nil) != JSONMatchKey(actDoc, nil) {
		t.Errorf("JSONMatchKey differs between float64 and json.Number for equivalent values")
	}
	if diffs := CountJSONFieldDiffs(CollectJSONFields(expDoc, nil), CollectJSONFields(actDoc, nil)); diffs != 0 {
		t.Errorf("CountJSONFieldDiffs mixed types = %d, want 0", diffs)
	}

	// BodyNoiseFromJSONDiff with ignoreOrdering=true and differently spelled equal numbers in object arrays.
	paths, skips := BodyNoiseFromJSONDiff(
		`{"items": [{"id": 1}, {"id": 2}]}`,
		`{"items": [{"id": 2.0}, {"id": 1.0}]}`,
		nil, true,
	)
	if len(paths) != 0 || len(skips) != 0 {
		t.Errorf("BodyNoiseFromJSONDiff reordered equal object array = (paths=%v, skips=%v), want empty", paths, skips)
	}

	// NormalizeNestedJSONForNoise must preserve large integers > 2^53.
	raw := `{"id": 1234567890123456789, "payload": "{\"nested_id\": 9876543210987654321, \"ts\": 1}"}`
	norm := NormalizeNestedJSONForNoise(raw, map[string][]string{"payload.ts": nil}, zap.NewNop())
	if !strings.Contains(norm, "1234567890123456789") || !strings.Contains(norm, "9876543210987654321") {
		t.Errorf("NormalizeNestedJSONForNoise lost large integer precision: %s", norm)
	}
}

func TestUnmarshallJSON_Strictness(t *testing.T) {
	// Trailing garbage must be rejected
	if _, err := UnmarshallJSON(`{"a": 1} trailing`, zap.NewNop()); err == nil {
		t.Errorf("expected error on trailing garbage after JSON")
	}
	// Concatenated valid JSON documents must be rejected
	if _, err := UnmarshallJSON(`{"a": 1}{"b": 2}`, zap.NewNop()); err == nil {
		t.Errorf("expected error on concatenated JSON documents")
	}
	// Whitespace-only input must be rejected
	if _, err := UnmarshallJSON("   \t\n ", zap.NewNop()); err == nil {
		t.Errorf("expected error on whitespace-only input")
	}
	// Whitespace-only trailing is valid
	if v, err := UnmarshallJSON("{\"a\": 1} \n\t ", zap.NewNop()); err != nil || v == nil {
		t.Errorf("expected success on trailing whitespace, got err: %v, v: %v", err, v)
	}
	// Empty string returns nil, nil
	if v, err := UnmarshallJSON("", zap.NewNop()); err != nil || v != nil {
		t.Errorf("expected nil, nil on empty string, got v: %v, err: %v", v, err)
	}
}

// TestLooksLikeRegex pins the heuristic behind the sectioned-noise warning.
// It must not flag OpenAPI/JSON-Schema document keys: telling a user to move
// "$ref" into a regex position is worse than silence, because as a regex it
// anchors to end-of-string and matches nothing.
func TestLooksLikeRegex(t *testing.T) {
	regexes := []string{
		`^[0-9]+$`,
		`^\d{4}-\d{2}-\d{2}$`,
		`.*`,
		`id-.+`,
		`\w+`,
		`(?i)abc`,
		`PENDING|PAID`,
		`[a-z]{3}`,
		`value$`,
	}
	for _, v := range regexes {
		if !looksLikeRegex(v) {
			t.Errorf("looksLikeRegex(%q) = false, want true", v)
		}
	}

	paths := []string{
		"items.product.stock",
		"user.id",
		"created_at",
		"components.schemas.User.$ref",
		"$schema",
		"$id",
		"definitions.Foo.$id",
		"data.2026.count",
		"items.0.product.stock",
		"X-Request-Id",
		"a+b",
		"arr[0]",
	}
	for _, v := range paths {
		if looksLikeRegex(v) {
			t.Errorf("looksLikeRegex(%q) = true, want false (it is a field path)", v)
		}
	}
}

// TestWarnIfRegexShaped_DedupesAcrossCalls pins that the warning does not fire
// once per test case. SplitNoise is on the per-test-case path (twice per case on
// the failure-assessment path), so an undeduped warning would bury real output.
func TestWarnIfRegexShaped_DedupesAcrossCalls(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)

	noise := map[string][]string{"body": {`^[0-9]+$`}}
	for i := 0; i < 50; i++ {
		SplitNoise(noise, logger)
	}

	if got := logs.Len(); got != 1 {
		t.Errorf("emitted %d warnings across 50 calls, want exactly 1", got)
	}
}

// TestSplitNoise_NoWarningForPlainPaths guards the common case from noise.
func TestSplitNoise_NoWarningForPlainPaths(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)

	SplitNoise(map[string][]string{
		"body":   {"items.product.stock", "components.schemas.User.$ref"},
		"header": {"X-Request-Id"},
	}, logger)

	if got := logs.Len(); got != 0 {
		t.Errorf("emitted %d warnings for plain field paths, want 0: %v", got, logs.All())
	}
}

// TestCompareHeaders_DoesNotMutateCallerNoiseMap is a STANDING anti-aliasing
// guard on the loweredNoise builder in CompareHeaders: the caller's noise map
// is read-only config, and a case-only key collision must never write through
// into the caller's slices.
//
// Be honest about its reach. It passes on the slices.Concat/slices.Clone
// builder AND on the make+copy builder that preceded it — both are
// non-aliasing, so there is no assertion that can tell them apart, and this
// test is not coverage for that refactor. What it does catch is the builder
// being rewritten to store the caller's slice verbatim and merge collisions
// in place, which is the regression the comments in utils.go warn against.
//
// The trap is spare capacity. Each noise value here is a len=1 window over a
// len=1/cap=3 backing array whose tail holds sentinels. A builder that stored
// the caller's slice in loweredNoise verbatim and then merged the collision
// with an in-place `append(existing, v...)` would find room in the spare
// capacity and write the other entry's regexes straight over the sentinels —
// silent, caller-visible corruption of a map the caller is entitled to reuse.
// (Cloning on insert is what defuses the trap: a clone has zero spare capacity,
// so a later append must reallocate. That is why the two lines only fail this
// test together, and why the guard is written against the builder as a whole
// rather than either line.) Whichever of the two case variants Go's randomized
// map iteration visits first becomes `existing`, so both backing arrays carry
// sentinels and the check is order-independent.
//
// CompareHeaders is deliberately called twice with the SAME map, because that
// is how the corruption compounds in production: keploy re-runs the matcher
// per test case against one parsed keploy.yml noise config.
func TestCompareHeaders_DoesNotMutateCallerNoiseMap(t *testing.T) {
	backingA := []string{"^alpha-.*$", "SENTINEL-A1", "SENTINEL-A2"}
	backingB := []string{"^beta-.*$", "SENTINEL-B1", "SENTINEL-B2"}

	noise := map[string][]string{
		"X-Correlation-Id": backingA[:1], // len 1, cap 3
		"x-correlation-id": backingB[:1], // len 1, cap 3
	}

	// Snapshot the exact slice headers the caller handed us so we can detect
	// both a length change and a backing-array rewrite.
	origA, origB := noise["X-Correlation-Id"], noise["x-correlation-id"]

	// Two calls, same map. Each call exercises one of the merged regexes, so a
	// merge that dropped either entry also fails here.
	for i, val := range []string{"alpha-abc", "beta-xyz"} {
		h1, h2 := http.Header{}, http.Header{}
		h1.Set("X-Correlation-Id", val+"-expected")
		h2.Set("X-Correlation-Id", val+"-actual")

		var res []models.HeaderResult
		if ok := CompareHeaders(h1, h2, &res, noise); !ok {
			t.Fatalf("call %d: value %q should be noise via the merged regex set; got match=false (res=%+v)", i, val, res)
		}
		got, found := findHeaderResult(res, "X-Correlation-Id")
		if !found || !got.Normal {
			t.Fatalf("call %d: expected Normal=true HeaderResult for %q; found=%v got=%+v", i, val, found, got)
		}
	}

	// The caller's map must be structurally untouched.
	if len(noise) != 2 {
		t.Fatalf("caller noise map was resized: len=%d, want 2 (%v)", len(noise), noise)
	}
	if len(noise["X-Correlation-Id"]) != 1 || len(noise["x-correlation-id"]) != 1 {
		t.Fatalf("caller noise slices grew: %v", noise)
	}
	if len(origA) != 1 || origA[0] != "^alpha-.*$" || len(origB) != 1 || origB[0] != "^beta-.*$" {
		t.Fatalf("caller noise slice contents changed: origA=%v origB=%v", origA, origB)
	}

	// …and so must the spare capacity behind them.
	wantA := []string{"^alpha-.*$", "SENTINEL-A1", "SENTINEL-A2"}
	wantB := []string{"^beta-.*$", "SENTINEL-B1", "SENTINEL-B2"}
	for i := range wantA {
		if backingA[i] != wantA[i] {
			t.Fatalf("CompareHeaders wrote into the caller's backing array: backingA=%v, want %v", backingA, wantA)
		}
		if backingB[i] != wantB[i] {
			t.Fatalf("CompareHeaders wrote into the caller's backing array: backingB=%v, want %v", backingB, wantB)
		}
	}
}

// exactUnderNoise is the verdict of the string entry points under noise:
// ValidateAndMarshalJSON, then JSONDiffWithNoiseControl without ordering.
func exactUnderNoise(exp, act string, noise map[string][]string) bool {
	vj, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act)
	if err != nil || !vj.IsIdentical() {
		return false
	}
	res, err := JSONDiffWithNoiseControl(vj, noise, false, zap.NewNop())
	return err == nil && res.IsExact()
}

// Two noise keys that differ only in case lower to one entry key. Which of the
// two applies used to follow Go's map order, so the same pair could pass in
// one call and fail in the next. The smallest configured key now applies, for
// path keys (buildNoiseIndex), global keys (dot-free, matched at any depth, the
// most common shape) and the dotted keys SplitNoise lowers into its sections.
func TestNoiseKeysDifferingOnlyInCaseApplyTheSameEntryEveryCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noise    map[string][]string
		exp, act string
		exact    bool // the smallest key ("CreatedAt..." < "createdAt...") applies
	}{
		{"path keys", map[string][]string{"CreatedAt.data": {"^v[0-9]$"}, "createdAt.data": {}, "b.C": {}, "B.c": {"^x$"}},
			`{"createdAt":{"data":"z"}}`, `{"createdAt":{"data":"y"}}`, false},
		{"path keys, unconditional smallest", map[string][]string{"CreatedAt.data": {}, "createdAt.data": {"^v[0-9]$"}},
			`{"createdAt":{"data":"z"}}`, `{"createdAt":{"data":"y"}}`, true},
		{"global keys", map[string][]string{"CreatedAt": {}, "createdAt": {"^v[0-9]$"}},
			`{"createdAt":"z"}`, `{"createdAt":"y"}`, true},
		{"global keys, guarded smallest", map[string][]string{"CreatedAt": {"^v[0-9]$"}, "createdAt": {}},
			`{"createdAt":"z"}`, `{"createdAt":"y"}`, false},
		{"global keys, three spellings, nested", map[string][]string{"CREATEDAT": {"^y$"}, "CreatedAt": {}, "createdAt": {"^v[0-9]$"}},
			`{"a":[{"createdAt":"z"}]}`, `{"a":[{"createdAt":"q"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantDiffs := len(JSONFieldDiffs(tc.exp, tc.act, tc.noise, "body.", 0))
			for i := 0; i < 500; i++ {
				if got := exactUnderNoise(tc.exp, tc.act, tc.noise); got != tc.exact {
					t.Fatalf("call %d: exact=%v, want %v (the smallest configured key applies)", i, got, tc.exact)
				}
				if len(JSONFieldDiffs(tc.exp, tc.act, tc.noise, "body.", 0)) != wantDiffs {
					t.Fatal("the reported diffs changed between calls")
				}
			}
		})
	}
}

// SplitNoise lowers the paths of dotted keys, so "body.CreatedAt" and
// "body.createdAt" name one entry of its body section (and "header.X-Id" and
// "header.x-id" one of its header section). The smallest configured key's
// patterns apply there too, every call.
func TestSplitNoiseKeysDifferingOnlyInCaseKeepTheSameEntryEveryCall(t *testing.T) {
	noise := map[string][]string{
		"body.createdAt": {"^v[0-9]$"}, "body.CreatedAt": {}, "Body.createdAt": {"^x$"},
		"header.x-id": {}, "header.X-Id": {"^h$"},
	}
	for i := 0; i < 500; i++ {
		body, header, _ := SplitNoise(noise, nil)
		if got := body["createdat"]; got == nil || len(got) != 1 || got[0] != "^x$" {
			t.Fatalf("call %d: body createdat = %q, want the entry of \"Body.createdAt\", the smallest key", i, got)
		}
		if got := header["x-id"]; len(got) != 1 || got[0] != "^h$" {
			t.Fatalf("call %d: header x-id = %q, want the entry of \"header.X-Id\", the smallest key", i, got)
		}
	}
}

// jsonDiffExact runs the body comparison with noise and reports whether the two
// documents compare equal under it.
func jsonDiffExact(t *testing.T, exp, act string, noise map[string][]string) bool {
	t.Helper()
	v, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act)
	if err != nil {
		t.Fatalf("ValidateAndMarshalJSON: %v", err)
	}
	res, err := JSONDiffWithNoiseControl(v, noise, false, nil)
	if err != nil {
		return false
	}
	return res.IsExact()
}

// An entry naming id keys excuses its field under every id key of the map, and
// nothing under an ordinary key there or anywhere else in the response.
func TestJSONDiffWithNoiseControl_IDKeyEntry(t *testing.T) {
	u, v := testUUID(1), testUUID(2)
	doc := func(t1, t2, rate, summary string) string {
		return `{"options":{"` + u + `":{"traceId":"` + t1 + `","rate":` + rate + `},"` + v +
			`":{"traceId":"` + t2 + `","rate":2},"summary":{"traceId":"` + summary + `"}}}`
	}
	noise := map[string][]string{"options.{id}.traceId": {}}
	exp := doc("a", "b", "1", "s")
	if !jsonDiffExact(t, exp, doc("x", "y", "1", "s"), noise) {
		t.Errorf("the traceId under both id keys is excused")
	}
	if jsonDiffExact(t, exp, doc("x", "y", "9", "s"), noise) {
		t.Errorf("a sibling field under an id key is still compared")
	}
	if jsonDiffExact(t, exp, doc("a", "b", "1", "changed"), noise) {
		t.Errorf("traceId under an ordinary key (summary) is still compared")
	}
}

// An entry naming ONE id keeps naming that id only. Auto-noise writes one per
// id whose field drifted, which on a real response is not every id, so reading
// it as every id would stop asserting the field where it never drifted.
func TestJSONDiffWithNoiseControl_PerIDEntryNamesOneID(t *testing.T) {
	u, v := testUUID(1), testUUID(2)
	doc := func(t1, t2 string) string {
		return `{"options":{"` + u + `":{"hold":"` + t1 + `"},"` + v + `":{"hold":"` + t2 + `"}}}`
	}
	noise := map[string][]string{"options." + u + ".hold": {}}
	if !jsonDiffExact(t, doc("a", "b"), doc("x", "b"), noise) {
		t.Errorf("the id the entry names is excused")
	}
	if jsonDiffExact(t, doc("a", "b"), doc("a", "y"), noise) {
		t.Errorf("the same field under another id is still compared")
	}
}

// Most specific first holds across the two spellings: an entry naming one id
// is more specific than the id-key entry for the same field, so its pattern
// decides for that id, and the id-key entry is not a way around it.
func TestJSONDiffWithNoiseControl_PerIDPatternOutranksIDKeyEntry(t *testing.T) {
	u, v := testUUID(1), testUUID(2)
	doc := func(t1, t2 string) string {
		return `{"options":{"` + u + `":{"t":"` + t1 + `"},"` + v + `":{"t":"` + t2 + `"}}}`
	}
	noise := map[string][]string{
		"options." + u + ".t": {"^ok-"},
		"options.{id}.t":      {},
	}
	if !jsonDiffExact(t, doc("ok-1", "b"), doc("ok-2", "y"), noise) {
		t.Errorf("values inside the narrower pattern, and the other id, are excused")
	}
	if jsonDiffExact(t, doc("ok-1", "b"), doc("bad", "b"), noise) {
		t.Errorf("a value outside the per-id pattern is a difference although an id-key entry also names the field")
	}
}

// Under the proven switch a learned entry names exactly its field and subtree;
// one naming id keys does so under every id key. The learned key is spelled as
// the index spells the entry, a literal id beside the segment included.
func TestLearnedIDKeyEntryIsExactUnderTheSwitch(t *testing.T) {
	u, v := testUUID(3), testUUID(4)
	learned := "Options[]." + u + ".{ID}.T"
	idx := buildNoiseIndex(map[string][]string{learned: {}}, nil)
	idx.markTypeStrict(map[string]struct{}{learned: {}})
	t.Setenv(provenswitch.Env, "1")
	if _, noisy, strict := idx.matchKind("options[]." + v + "." + u + ".t"); !noisy || !strict {
		t.Fatalf("the learned field under id keys: noisy=%v typeStrict=%v", noisy, strict)
	}
	if _, noisy, _ := idx.matchKind("options[]." + v + "." + u + ".tx"); noisy {
		t.Fatalf("a sibling whose name it prefixes is excused under the switch")
	}
}

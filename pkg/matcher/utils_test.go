package matcher

import (
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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
			merged := make([]string, 0, len(existing)+len(v))
			merged = append(merged, existing...)
			merged = append(merged, v...)
			lowered[lk] = merged
		} else {
			cp := make([]string, len(v))
			copy(cp, v)
			lowered[lk] = cp
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

// TestJSONDiffWithNoiseControl_IndexedPathIsNotSupported pins a known gap that
// this change deliberately does NOT paper over.
//
// The JSON walker never adds an array position to its keys — every element of
// `items` is walked under the key "items" — so an index-free path works and an
// indexed one matches nothing. Normalizing the indexed form by dropping numeric
// segments looks obvious and is a trap: noise keys are matched with
// strings.Contains, so "items.0.id" would collapse to "items.id", which is a
// substring of "items.idempotency_key" and would silence it. That trades this
// change's silent false negative for a wider one, so the indexed form is left
// unmatched (loud) until the walker itself carries positions.
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

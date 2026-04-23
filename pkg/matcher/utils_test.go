package matcher

import (
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// subsKeyMatchWithOriginal is a test helper that preserves the old
// "caller passes raw keys" ergonomics: it pre-lowercases the noise map
// exactly once (mirroring the contract documented on SubstringKeyMatch and
// implemented in CompareHeaders) and then delegates. Keeping the helper in
// tests lets the table-driven cases below keep expressive CamelCase /
// MIXED-CASE noise keys — which document the case-insensitivity guarantee —
// without burdening production callers with per-call normalization.
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

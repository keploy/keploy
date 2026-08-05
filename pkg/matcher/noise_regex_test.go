package matcher

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// diffNoise is a small helper: compare two JSON documents under a noise map and
// report whether the matcher considered them equal.
func diffNoise(t *testing.T, expected, actual string, noise map[string][]string) bool {
	t.Helper()
	e, a := expected, actual
	v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
	if err != nil {
		t.Fatalf("ValidateAndMarshalJSON: %v", err)
	}
	res, err := JSONDiffWithNoiseControl(v, noise, false, nil)
	if err != nil {
		t.Fatalf("JSONDiffWithNoiseControl: %v", err)
	}
	return res.IsExact()
}

// TestRegexNoise_EnforcesPattern pins the contract of a regex-guarded noise
// entry: the field is ignored ONLY when the replayed value matches one of the
// patterns. A value outside the pattern is a real difference.
//
// Previously the regex was decorative — a failed match fell through to
// `if e == a || noisy`, where noisy was still true, so the field was accepted
// anyway. Every regex-constrained noise entry was an unconditional skip, which
// is the opposite of what the pattern says.
func TestRegexNoise_EnforcesPattern(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		actual   string
		noise    map[string][]string
		want     bool
	}{
		{
			name:     "string: value inside the pattern is ignored",
			expected: `{"order":{"status":"PENDING"}}`,
			actual:   `{"order":{"status":"PAID"}}`,
			noise:    map[string][]string{"order.status": {"^(PENDING|PAID)$"}},
			want:     true,
		},
		{
			name:     "string: value outside the pattern is a real difference",
			expected: `{"order":{"status":"PENDING"}}`,
			actual:   `{"order":{"status":"GARBAGE"}}`,
			noise:    map[string][]string{"order.status": {"^(PENDING|PAID)$"}},
			want:     false,
		},
		{
			name:     "string: identical values still match even outside the pattern",
			expected: `{"order":{"status":"GARBAGE"}}`,
			actual:   `{"order":{"status":"GARBAGE"}}`,
			noise:    map[string][]string{"order.status": {"^(PENDING|PAID)$"}},
			want:     true,
		},
		{
			name:     "string: an empty regex list still ignores unconditionally",
			expected: `{"order":{"status":"PENDING"}}`,
			actual:   `{"order":{"status":"GARBAGE"}}`,
			noise:    map[string][]string{"order.status": {}},
			want:     true,
		},
		{
			name:     "number: value inside the pattern is ignored",
			expected: `{"order":{"total":1200}}`,
			actual:   `{"order":{"total":1250}}`,
			noise:    map[string][]string{"order.total": {`^12\d\d$`}},
			want:     true,
		},
		{
			name:     "number: value outside the pattern is a real difference",
			expected: `{"order":{"total":1200}}`,
			actual:   `{"order":{"total":99999}}`,
			noise:    map[string][]string{"order.total": {`^12\d\d$`}},
			want:     false,
		},
		{
			name:     "number: an empty regex list still ignores unconditionally",
			expected: `{"order":{"total":1200}}`,
			actual:   `{"order":{"total":99999}}`,
			noise:    map[string][]string{"order.total": {}},
			want:     true,
		},
		{
			name:     "number: fractional values compare in their JSON form",
			expected: `{"order":{"rate":12.5}}`,
			actual:   `{"order":{"rate":12.75}}`,
			noise:    map[string][]string{"order.rate": {`^12\.\d+$`}},
			want:     true,
		},
		{
			name:     "bool: value inside the pattern is ignored",
			expected: `{"order":{"paid":true}}`,
			actual:   `{"order":{"paid":false}}`,
			noise:    map[string][]string{"order.paid": {"^(true|false)$"}},
			want:     true,
		},
		{
			name:     "bool: value outside the pattern is a real difference",
			expected: `{"order":{"paid":true}}`,
			actual:   `{"order":{"paid":false}}`,
			noise:    map[string][]string{"order.paid": {"^true$"}},
			want:     false,
		},
		{
			name:     "bool: an empty regex list still ignores unconditionally",
			expected: `{"order":{"paid":true}}`,
			actual:   `{"order":{"paid":false}}`,
			noise:    map[string][]string{"order.paid": {}},
			want:     true,
		},
		{
			name:     "several patterns: matching any one of them ignores the field",
			expected: `{"order":{"status":"PENDING"}}`,
			actual:   `{"order":{"status":"SHIPPED"}}`,
			noise:    map[string][]string{"order.status": {"^PAID$", "^SHIPPED$"}},
			want:     true,
		},
		{
			name:     "several patterns: matching none of them is a real difference",
			expected: `{"order":{"status":"PENDING"}}`,
			actual:   `{"order":{"status":"CANCELLED"}}`,
			noise:    map[string][]string{"order.status": {"^PAID$", "^SHIPPED$"}},
			want:     false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diffNoise(t, c.expected, c.actual, c.noise); got != c.want {
				t.Errorf("IsExact() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRegexNoise_DoesNotLeakToSiblings pins that enforcing the pattern does not
// change which fields the entry applies to.
func TestRegexNoise_DoesNotLeakToSiblings(t *testing.T) {
	expected := `{"order":{"status":"PENDING","note":"hello"}}`
	actual := `{"order":{"status":"PAID","note":"TAMPERED"}}`
	noise := map[string][]string{"order.status": {"^(PENDING|PAID)$"}}

	if diffNoise(t, expected, actual, noise) {
		t.Errorf("note is not covered by the noise entry and must still be compared")
	}
}

// TestRegexNoise_UnparseableRegexDoesNotSilenceTheField guards the failure mode
// of a typo'd pattern. getCompiled falls back to a regexp that matches nothing,
// so the field must be compared normally rather than silently skipped.
func TestRegexNoise_UnparseableRegexDoesNotSilenceTheField(t *testing.T) {
	noise := map[string][]string{"order.status": {"^(unclosed"}}

	if diffNoise(t, `{"order":{"status":"PENDING"}}`, `{"order":{"status":"GARBAGE"}}`, noise) {
		t.Errorf("an invalid pattern must not silence the field")
	}
	if !diffNoise(t, `{"order":{"status":"PENDING"}}`, `{"order":{"status":"PENDING"}}`, noise) {
		t.Errorf("identical values must still match when the pattern is invalid")
	}
}

// TestGetCompiled_InvalidPatternNeverReturnsNil is the regression test for a
// crash: the "never matches" fallback used to be `(?!)`, a negative lookahead
// that Go's RE2 engine rejects, so getCompiled cached a nil *regexp.Regexp and
// the first noise comparison against it dereferenced nil. A typo in a noise
// pattern took the whole run down with a SIGSEGV.
func TestGetCompiled_InvalidPatternNeverReturnsNil(t *testing.T) {
	for _, pattern := range []string{
		`^(unclosed`,
		`(?!)`,      // the old fallback itself
		`(?<=look)`, // lookbehind, unsupported by RE2
		`a{2,1}`,    // invalid repetition
		`[z-a]`,     // invalid character range
	} {
		re := getCompiled(pattern)
		if re == nil {
			t.Fatalf("getCompiled(%q) = nil, want a non-nil never-matching regex", pattern)
		}
		// Must not panic, and must not match anything.
		for _, s := range []string{"", "abc", "look", "aa"} {
			if re.MatchString(s) {
				t.Errorf("getCompiled(%q).MatchString(%q) = true, want false", pattern, s)
			}
		}
	}
}

// TestGetCompiled_ValidPatternStillWorks guards the happy path and the cache.
func TestGetCompiled_ValidPatternStillWorks(t *testing.T) {
	re := getCompiled(`^ord-\d+$`)
	if re == nil {
		t.Fatal("getCompiled returned nil for a valid pattern")
	}
	if !re.MatchString("ord-42") || re.MatchString("ord-x") {
		t.Errorf("compiled pattern does not behave as written")
	}
	// Second call comes from the cache and must be equivalent.
	if again := getCompiled(`^ord-\d+$`); again != re {
		t.Errorf("expected the cached instance on the second call")
	}
}

// TestAnyRegexpMatchStr_ToleratesNilEntries is defence in depth: even if some
// future path puts a nil into the list, matching must not crash.
func TestAnyRegexpMatchStr_ToleratesNilEntries(t *testing.T) {
	if anyRegexpMatchStr("abc", []*regexp.Regexp{nil}) {
		t.Errorf("a nil pattern must not report a match")
	}
	if !anyRegexpMatchStr("abc", []*regexp.Regexp{nil, regexp.MustCompile(`^abc$`)}) {
		t.Errorf("a valid pattern after a nil must still be evaluated")
	}
}

// TestRegexNoise_GlobalKeyEnforcesPattern covers the shape most users actually
// write. A noise key with no dot is a "global" key, ignored at any depth — and
// it is what `body.status` becomes once http.Match strips the `body.` prefix.
// Its patterns used to be discarded outright when the key was classified, so
// enforcing them on the path-based branch alone left the common case unfixed.
func TestRegexNoise_GlobalKeyEnforcesPattern(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		actual   string
		noise    map[string][]string
		want     bool
	}{
		{
			name:     "top level, value inside the pattern",
			expected: `{"status":"PENDING"}`,
			actual:   `{"status":"PAID"}`,
			noise:    map[string][]string{"status": {"^(PENDING|PAID)$"}},
			want:     true,
		},
		{
			name:     "top level, value outside the pattern",
			expected: `{"status":"PENDING"}`,
			actual:   `{"status":"GARBAGE"}`,
			noise:    map[string][]string{"status": {"^(PENDING|PAID)$"}},
			want:     false,
		},
		{
			name:     "nested, value outside the pattern",
			expected: `{"order":{"status":"PENDING"}}`,
			actual:   `{"order":{"status":"GARBAGE"}}`,
			noise:    map[string][]string{"status": {"^(PENDING|PAID)$"}},
			want:     false,
		},
		{
			name:     "empty pattern list still ignores everywhere",
			expected: `{"order":{"status":"PENDING"}}`,
			actual:   `{"order":{"status":"GARBAGE"}}`,
			noise:    map[string][]string{"status": {}},
			want:     true,
		},
		{
			name:     "number leaf under a global key",
			expected: `{"total":1200}`,
			actual:   `{"total":99999}`,
			noise:    map[string][]string{"total": {`^12\d\d$`}},
			want:     false,
		},
		{
			name:     "unexpected extra key is excused only when it matches",
			expected: `{"a":1}`,
			actual:   `{"a":1,"status":"GARBAGE"}`,
			noise:    map[string][]string{"status": {"^PAID$"}},
			want:     false,
		},
		{
			name:     "unexpected extra key matching the pattern is excused",
			expected: `{"a":1}`,
			actual:   `{"a":1,"status":"PAID"}`,
			noise:    map[string][]string{"status": {"^PAID$"}},
			want:     true,
		},
		{
			// A pattern has no value to match against, and a field disappearing
			// is a schema change, not a volatile value.
			name:     "a missing key is not excused by a pattern-guarded entry",
			expected: `{"a":1,"status":"PAID"}`,
			actual:   `{"a":1}`,
			noise:    map[string][]string{"status": {"^PAID$"}},
			want:     false,
		},
		{
			name:     "a missing key is excused by an empty pattern list",
			expected: `{"a":1,"status":"PAID"}`,
			actual:   `{"a":1}`,
			noise:    map[string][]string{"status": {}},
			want:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diffNoise(t, c.expected, c.actual, c.noise); got != c.want {
				t.Errorf("IsExact() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRegexNoise_ContainerPathRelaxesLeafValues pins that a pattern on an object
// path relaxes the VALUES beneath it. A pattern describes one scalar, so
// applying it to every descendant would fail fields it was never written for.
// The subtree is not wholly ignored though — see
// TestRegexNoise_ContainerPathStillChecksStructure.
func TestRegexNoise_ContainerPathRelaxesLeafValues(t *testing.T) {
	expected := `{"user":{"session":{"token":"abcdef01","expiresAt":1700000000}}}`
	actual := `{"user":{"session":{"token":"ffffffff","expiresAt":1799999999}}}`

	if !diffNoise(t, expected, actual, map[string][]string{"user.session": {"^[a-f0-9]{8}$"}}) {
		t.Errorf("a pattern on a container path must relax the values beneath it")
	}
	// A sibling outside the guarded subtree is unaffected.
	exp2 := `{"user":{"session":{"token":"abcdef01"},"name":"alice"}}`
	act2 := `{"user":{"session":{"token":"ffffffff"},"name":"TAMPERED"}}`
	if diffNoise(t, exp2, act2, map[string][]string{"user.session": {"^[a-f0-9]{8}$"}}) {
		t.Errorf("name is outside the guarded subtree and must still be compared")
	}
}

// TestRegexNoise_ContainerPathStillChecksStructure pins that relaxing the VALUES
// under a pattern-guarded container does not also stop checking its SHAPE. A key
// removed, added or retyped inside is a schema change, not a volatile value, and
// a pattern was never a request to ignore it. Only an empty pattern list — which
// has always meant "ignore this subtree" — hides structure.
func TestRegexNoise_ContainerPathStillChecksStructure(t *testing.T) {
	guarded := map[string][]string{"user.session": {"^[a-f0-9]+$"}}
	unconditional := map[string][]string{"user.session": {}}

	cases := []struct {
		name     string
		expected string
		actual   string
	}{
		{
			name:     "key missing inside the guarded subtree",
			expected: `{"user":{"session":{"token":"abcd","kind":"bearer"}}}`,
			actual:   `{"user":{"session":{"token":"abcd"}}}`,
		},
		{
			name:     "extra key inside the guarded subtree",
			expected: `{"user":{"session":{"token":"abcd"}}}`,
			actual:   `{"user":{"session":{"token":"abcd","injected":"x"}}}`,
		},
		{
			name:     "scalar retyped to an object inside the guarded subtree",
			expected: `{"user":{"session":{"token":"abcd"}}}`,
			actual:   `{"user":{"session":{"token":{"evil":true}}}}`,
		},
		{
			name:     "array length change under the guarded path",
			expected: `{"user":{"session":["abcd","ef01"]}}`,
			actual:   `{"user":{"session":["abcd"]}}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if diffNoise(t, c.expected, c.actual, guarded) {
				t.Errorf("a pattern must not hide a structural change")
			}
			if !diffNoise(t, c.expected, c.actual, unconditional) {
				t.Errorf("an empty pattern list must still ignore the whole subtree")
			}
		})
	}
}

// TestRegexNoise_ScalarRetypedToContainerIsReported is the direct regression for
// a hole an earlier revision of this fix introduced: skipping a pattern-guarded
// child whenever the REPLAYED value was a container meant a scalar swapped for
// an object or array passed silently.
func TestRegexNoise_ScalarRetypedToContainerIsReported(t *testing.T) {
	noise := map[string][]string{"order.id": {`^ord-\d+$`}}
	for _, actual := range []string{
		`{"order":{"id":{"evil":true}}}`,
		`{"order":{"id":["evil"]}}`,
	} {
		if diffNoise(t, `{"order":{"id":"ord-1"}}`, actual, noise) {
			t.Errorf("a scalar replaced by a container must be reported, got a match for %s", actual)
		}
	}
}

// TestFormatJSONNumber matches encoding/json's own float rendering, so a
// pattern written against the value Keploy prints in its diff matches at
// replay. Plain 'f' formatting diverges at both extremes.
func TestFormatJSONNumber(t *testing.T) {
	for _, c := range []struct{ in float64 }{
		{1200}, {1200.5}, {100.10}, {0}, {math.Copysign(0, -1)}, {-12.75},
		{1e21}, {1e-7}, {5e-324}, {1e20}, {1e-6}, {123456789012345680000},
	} {
		want, err := json.Marshal(c.in)
		if err != nil {
			t.Fatalf("json.Marshal(%v): %v", c.in, err)
		}
		if got := formatJSONNumber(c.in); got != string(want) {
			t.Errorf("formatJSONNumber(%v) = %q, encoding/json renders %q", c.in, got, want)
		}
	}
}

// TestRegexNoise_OverlappingEntriesAreDeterministic pins that two noise entries
// whose paths overlap produce the same verdict on every run.
//
// noiseIndex.match returns the first matching entry, and buildNoiseIndex used to
// append them in Go map order, which is randomized per process. That was
// invisible while every entry meant "ignore unconditionally"; once patterns
// decide the verdict it made a suite flip green/red between runs with no input
// change. Entries are now ordered most-specific first.
func TestRegexNoise_OverlappingEntriesAreDeterministic(t *testing.T) {
	cases := []struct {
		name  string
		noise map[string][]string
	}{
		{"specific unconditional, general guarded", map[string][]string{"user.id": {`^\d+$`}, "order.user.id": {}}},
		{"both guarded", map[string][]string{"user.id": {`^\d+$`}, "order.user.id": {`^u-\d+$`}}},
		{"three overlapping", map[string][]string{"id": {`^\d+$`}, "user.id": {}, "order.user.id": {`^u-\d+$`}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := diffNoise(t, `{"order":{"user":{"id":"u-1"}}}`, `{"order":{"user":{"id":"u-2"}}}`, c.noise)
			for i := 0; i < 300; i++ {
				if got := diffNoise(t, `{"order":{"user":{"id":"u-1"}}}`, `{"order":{"user":{"id":"u-2"}}}`, c.noise); got != first {
					t.Fatalf("verdict flipped between runs (%v then %v) — entry order is not deterministic", first, got)
				}
			}
		})
	}
}

// TestRegexNoise_MostSpecificEntryWins pins which of two overlapping entries is
// chosen, so the ordering is a documented contract rather than an accident.
func TestRegexNoise_MostSpecificEntryWins(t *testing.T) {
	// The longer path is the one the user meant for this field: it says the id
	// looks like "u-<n>", so a value of that shape is ignored...
	noise := map[string][]string{"id": {`^\d+$`}, "order.user.id": {`^u-\d+$`}}
	if !diffNoise(t, `{"order":{"user":{"id":"u-1"}}}`, `{"order":{"user":{"id":"u-2"}}}`, noise) {
		t.Errorf("the more specific entry should have matched and ignored this value")
	}
	// ...and a value outside it is still a real difference, rather than being
	// rescued by the shorter, more general entry.
	if diffNoise(t, `{"order":{"user":{"id":"u-1"}}}`, `{"order":{"user":{"id":"XXX"}}}`, noise) {
		t.Errorf("a value outside the more specific entry's pattern must be reported")
	}
}

// TestRegexNoise_ArrayOfScalarsEnforcesPattern covers the one container where a
// pattern is unambiguous: `ids: ["^ord-\d+$"]` plainly means every id looks like
// that, so it is applied per element.
func TestRegexNoise_ArrayOfScalarsEnforcesPattern(t *testing.T) {
	noise := map[string][]string{"order.ids": {`^ord-\d+$`}}

	if !diffNoise(t, `{"order":{"ids":["ord-1","ord-2"]}}`, `{"order":{"ids":["ord-7","ord-9"]}}`, noise) {
		t.Errorf("every element matches the pattern, so the array should be ignored")
	}
	if diffNoise(t, `{"order":{"ids":["ord-1","ord-2"]}}`, `{"order":{"ids":["ord-7","HACK"]}}`, noise) {
		t.Errorf("an element outside the pattern must be reported")
	}
	// An empty pattern list keeps ignoring the array wholesale.
	if !diffNoise(t, `{"order":{"ids":["ord-1"]}}`, `{"order":{"ids":["HACK"]}}`, map[string][]string{"order.ids": {}}) {
		t.Errorf("an empty pattern list must still ignore the whole array")
	}
	// A length change is structural and is still reported.
	if diffNoise(t, `{"order":{"ids":["ord-1","ord-2"]}}`, `{"order":{"ids":["ord-1"]}}`, noise) {
		t.Errorf("an array length change must be reported")
	}
	// An array of objects has no single value for the pattern to describe, so it
	// falls back to relaxing the element values, as a map does.
	nested := map[string][]string{"order.items": {`^ord-\d+$`}}
	if !diffNoise(t, `{"order":{"items":[{"id":"ord-1"}]}}`, `{"order":{"items":[{"id":"HACK"}]}}`, nested) {
		t.Errorf("a non-scalar array relaxes its values rather than applying the pattern per element")
	}
}

// TestRegexNoise_InvalidPatternIsReported pins that a pattern that cannot be
// compiled is reported once. It silently matches nothing, so without a warning
// the user sees a field they marked as noise start failing with no clue why.
func TestRegexNoise_InvalidPatternIsReported(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)

	// Use a pattern unique to this test: the warning dedupes per pattern for the
	// life of the process.
	bad := `^(unclosed-` + t.Name()
	e, a := `{"order":{"status":"PENDING"}}`, `{"order":{"status":"GARBAGE"}}`
	v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
	if err != nil {
		t.Fatalf("ValidateAndMarshalJSON: %v", err)
	}
	noise := map[string][]string{"order.status": {bad}}

	res, err := JSONDiffWithNoiseControl(v, noise, false, logger)
	if err != nil {
		t.Fatalf("JSONDiffWithNoiseControl: %v", err)
	}
	if res.IsExact() {
		t.Errorf("an invalid pattern must not silence the field")
	}
	if logs.Len() != 1 {
		t.Fatalf("emitted %d warnings, want exactly 1: %v", logs.Len(), logs.All())
	}
	if got := logs.All()[0].ContextMap()["pattern"]; got != bad {
		t.Errorf("warning names pattern %v, want %q", got, bad)
	}

	// Repeated compilation of the same pattern must not repeat the warning.
	for i := 0; i < 20; i++ {
		if _, err := JSONDiffWithNoiseControl(v, noise, false, logger); err != nil {
			t.Fatalf("JSONDiffWithNoiseControl: %v", err)
		}
	}
	if logs.Len() != 1 {
		t.Errorf("warning repeated %d times for one pattern, want 1", logs.Len())
	}
}

// TestRegexNoise_ValidPatternWarnsNothing guards the common path from noise.
func TestRegexNoise_ValidPatternWarnsNothing(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)

	e, a := `{"order":{"status":"PENDING"}}`, `{"order":{"status":"PAID"}}`
	v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
	if err != nil {
		t.Fatalf("ValidateAndMarshalJSON: %v", err)
	}
	if _, err := JSONDiffWithNoiseControl(v, map[string][]string{"order.status": {"^(PENDING|PAID)$"}}, false, logger); err != nil {
		t.Fatalf("JSONDiffWithNoiseControl: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("emitted %d warnings for a valid pattern, want 0: %v", logs.Len(), logs.All())
	}
}

// TestRegexNoise_NullValuedFieldCanBeNoise covers a field recorded as null. A
// nullable volatile field is exactly what a pattern is for, but the null branch
// used to ignore noise entirely, so no entry could ever cover it.
func TestRegexNoise_NullValuedFieldCanBeNoise(t *testing.T) {
	recorded := `{"order":{"cancelledAt":null}}`
	replayed := `{"order":{"cancelledAt":"2026-08-05T08:22:51Z"}}`

	if !diffNoise(t, recorded, replayed, map[string][]string{"order.cancelledAt": {}}) {
		t.Errorf("an empty pattern list must cover a field recorded as null")
	}
	if !diffNoise(t, recorded, replayed, map[string][]string{"order.cancelledAt": {`^\d{4}-`}}) {
		t.Errorf("a matching pattern must cover a field recorded as null")
	}
	if diffNoise(t, recorded, replayed, map[string][]string{"order.cancelledAt": {`^nope$`}}) {
		t.Errorf("a non-matching pattern must leave the difference reported")
	}
	if !diffNoise(t, recorded, `{"order":{"cancelledAt":null}}`, map[string][]string{}) {
		t.Errorf("null replayed as null is unchanged")
	}
}

// TestRegexNoise_UnexpectedKeyHonoursPattern pins that an extra key in the
// replay is excused on the same terms under path noise as under a global entry.
func TestRegexNoise_UnexpectedKeyHonoursPattern(t *testing.T) {
	if !diffNoise(t, `{"a":1}`, `{"a":1,"trace":"ord-9"}`, map[string][]string{"trace": {`^ord-\d+$`}}) {
		t.Errorf("an unexpected key matching its pattern should be excused")
	}
	if diffNoise(t, `{"a":1}`, `{"a":1,"trace":"HACK"}`, map[string][]string{"trace": {`^ord-\d+$`}}) {
		t.Errorf("an unexpected key outside its pattern must be reported")
	}
}

// TestRegexNoise_MissingKeyRuleIsConsistent pins that path noise and global
// noise agree: only an unconditional entry excuses a key the replay dropped.
func TestRegexNoise_MissingKeyRuleIsConsistent(t *testing.T) {
	recorded := `{"order":{"a":1,"status":"PAID"}}`
	replayed := `{"order":{"a":1}}`

	for _, c := range []struct {
		name  string
		noise map[string][]string
		want  bool
	}{
		{"global, unconditional", map[string][]string{"status": {}}, true},
		{"global, pattern-guarded", map[string][]string{"status": {"^PAID$"}}, false},
		{"path, unconditional", map[string][]string{"order.status": {}}, true},
		{"path, pattern-guarded", map[string][]string{"order.status": {"^PAID$"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := diffNoise(t, recorded, replayed, c.noise); got != c.want {
				t.Errorf("IsExact() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSubstringKeyMatch_IsDeterministic pins that overlapping header-noise keys
// resolve the same way on every run. It used to range a map and take whichever
// key came first, so a header could be noisy in one run and not the next.
func TestSubstringKeyMatch_IsDeterministic(t *testing.T) {
	mp := map[string][]string{
		"id":              {"short"},
		"request-id":      {"medium"},
		"x-request-id":    {"long"},
		"x-request-id-v2": {"longest"},
	}
	for i := 0; i < 300; i++ {
		got, ok := SubstringKeyMatch("X-Request-Id", mp)
		if !ok {
			t.Fatalf("expected a match")
		}
		if len(got) != 1 || got[0] != "long" {
			t.Fatalf("iteration %d: got %v, want the most specific matching key", i, got)
		}
	}
}

// TestCompareHeaders_InvalidPatternDoesNotPanic covers the other consumer of
// getCompiled. MatchesAnyRegex dereferenced the same nil, so a typo in a header
// noise pattern crashed the run exactly as a body one did.
func TestCompareHeaders_InvalidPatternDoesNotPanic(t *testing.T) {
	h1 := http.Header{"X-Trace": []string{"abc"}}
	h2 := http.Header{"X-Trace": []string{"def"}}
	res := &[]models.HeaderResult{}

	// Must not panic, and an uncompilable pattern must not silence the header.
	if CompareHeaders(h1, h2, res, map[string][]string{"x-trace": {"^(unclosed"}}) {
		t.Errorf("an invalid pattern must not make a differing header noisy")
	}
}

// TestMatchesAnyRegex_InvalidPatternDoesNotPanic is the direct guard.
func TestMatchesAnyRegex_InvalidPatternDoesNotPanic(t *testing.T) {
	ok, pattern := MatchesAnyRegex("anything", []string{"^(unclosed", `^any`})
	if !ok || pattern != `^any` {
		t.Errorf("got (%v, %q), want the valid pattern to still match", ok, pattern)
	}
	if ok, _ := MatchesAnyRegex("anything", []string{"^(unclosed"}); ok {
		t.Errorf("an invalid pattern must not match")
	}
}

// TestCompareHeaders_RegexEnforced pins that a header noise pattern constrains
// the value, matching the body semantics.
func TestCompareHeaders_RegexEnforced(t *testing.T) {
	noise := map[string][]string{"x-trace": {`^ord-\d+$`}}

	inPattern := &[]models.HeaderResult{}
	if !CompareHeaders(
		http.Header{"X-Trace": []string{"ord-1"}},
		http.Header{"X-Trace": []string{"ord-2"}}, inPattern, noise) {
		t.Errorf("a value inside the pattern should be ignored")
	}

	outOfPattern := &[]models.HeaderResult{}
	if CompareHeaders(
		http.Header{"X-Trace": []string{"ord-1"}},
		http.Header{"X-Trace": []string{"HACK"}}, outOfPattern, noise) {
		t.Errorf("a value outside the pattern must be reported")
	}
}

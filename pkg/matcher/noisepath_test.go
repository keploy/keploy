package matcher

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// noiseFor is the caller's half of the contract: turn the derived root-relative
// paths into the on-disk noise map, then back through SplitNoise the way the
// real matcher does.
func noiseFor(paths []string) map[string][]string {
	noise := map[string][]string{}
	for _, p := range paths {
		noise["body."+p] = []string{}
	}
	body, _, _ := SplitNoise(noise, nil)
	return body
}

// bodyMatches runs the actual body comparison the matcher uses.
func bodyMatches(t *testing.T, exp, act string, bodyNoise map[string][]string) bool {
	t.Helper()
	v, err := ValidateAndMarshalJSON(zap.NewNop(), &exp, &act)
	if err != nil {
		t.Fatalf("ValidateAndMarshalJSON: %v", err)
	}
	res, err := JSONDiffWithNoiseControl(v, bodyNoise, false, zap.NewNop())
	if err != nil {
		return false
	}
	return res.IsExact()
}

func TestBodyNoiseFromJSONDiff(t *testing.T) {
	for _, tt := range []struct {
		name        string
		exp, act    string
		wantPaths   []string
		wantSkipped []NoiseSkip
		// numericKeysAreReal marks the one row where a digits-only segment is a
		// genuine object key rather than the array position this whole change is
		// about, so the blanket assertion below can stay blanket.
		numericKeysAreReal bool
	}{
		{
			name:      "scalar drift inside an array of objects is index-free",
			exp:       `{"items":[{"product":{"stock":99}}]}`,
			act:       `{"items":[{"product":{"stock":100}}]}`,
			wantPaths: []string{"items.product.stock"},
		},
		{
			name:      "drift in several elements collapses to one path",
			exp:       `{"items":[{"stock":1},{"stock":2}]}`,
			act:       `{"items":[{"stock":9},{"stock":8}]}`,
			wantPaths: []string{"items.stock"},
		},
		{
			name:      "nested arrays collapse at every level",
			exp:       `{"matrix":[[{"deep":1}]]}`,
			act:       `{"matrix":[[{"deep":2}]]}`,
			wantPaths: []string{"matrix.deep"},
		},
		{
			name:      "array of scalars keys on the array field",
			exp:       `{"wrap":{"tags":["a"]}}`,
			act:       `{"wrap":{"tags":["b"]}}`,
			wantPaths: []string{"wrap.tags"},
		},
		{
			name:      "root-level array element",
			exp:       `[{"id":"a"}]`,
			act:       `[{"id":"b"}]`,
			wantPaths: []string{"id"},
		},
		{
			// The whole reason a string-level index strip is unsafe: 2026 is a
			// real object key, not a position, and only the document knows.
			name:               "numeric object key is preserved, not mistaken for an index",
			exp:                `{"data":{"2026":{"count":1}}}`,
			act:                `{"data":{"2026":{"count":2}}}`,
			wantPaths:          []string{"data.2026.count"},
			numericKeysAreReal: true,
		},
		{
			name:      "keys needing RFC-6901 escapes carry no escapes here",
			exp:       `{"a/b":1,"c~d":1}`,
			act:       `{"a/b":2,"c~d":2}`,
			wantPaths: []string{"a/b", "c~d"},
		},
		{
			name:      "several drifting fields are sorted and deduplicated",
			exp:       `{"b":1,"a":1,"n":{"z":1}}`,
			act:       `{"b":2,"a":2,"n":{"z":2}}`,
			wantPaths: []string{"a", "b", "n.z"},
		},
		{
			name:        "added field is refused",
			exp:         `{"a":1}`,
			act:         `{"a":1,"b":2}`,
			wantSkipped: []NoiseSkip{{Path: "b", Reason: NoiseSkipStructural}},
		},
		{
			name:        "removed field is refused",
			exp:         `{"a":1,"b":2}`,
			act:         `{"a":1}`,
			wantSkipped: []NoiseSkip{{Path: "b", Reason: NoiseSkipStructural}},
		},
		{
			name:        "type change is refused",
			exp:         `{"a":"5"}`,
			act:         `{"a":5}`,
			wantSkipped: []NoiseSkip{{Path: "a", Reason: NoiseSkipStructural}},
		},
		{
			// Not a schema change as far as the walker is concerned: its nil
			// branch consults the noise entry against the replayed value, because
			// a nullable volatile field is an ordinary thing to suppress. Pinned
			// against the walker by the round-trip test below.
			name:      "null becoming a value is emitted, because the walker can ignore it",
			exp:       `{"a":null,"k":1}`,
			act:       `{"a":"2026-01-01","k":1}`,
			wantPaths: []string{"a"},
		},
		{
			name:        "null becoming a container is refused",
			exp:         `{"a":null}`,
			act:         `{"a":{"b":1}}`,
			wantSkipped: []NoiseSkip{{Path: "a", Reason: NoiseSkipStructural}},
		},
		{
			// items.id is a substring of items.idempotency_key, and entries match
			// by substring, so this path would stop asserting the idempotency key.
			name:        "a path that brushes a sibling is refused",
			exp:         `{"items":[{"id":"a","idempotency_key":"k1"}]}`,
			act:         `{"items":[{"id":"b","idempotency_key":"k1"}]}`,
			wantSkipped: []NoiseSkip{{Path: "items.id", Reason: NoiseSkipOverBroad}},
		},
		{
			// A single-segment path is a GLOBAL key: an exact field-name match at
			// every depth. Emitting it would also stop asserting user.requestId.
			name:        "a root field whose name repeats deeper is refused",
			exp:         `{"requestId":"a","user":{"requestId":"a"}}`,
			act:         `{"requestId":"b","user":{"requestId":"a"}}`,
			wantSkipped: []NoiseSkip{{Path: "requestId", Reason: NoiseSkipOverBroad}},
		},
		{
			// ...but the same shape is fine when every field it covers drifted.
			// Both occurrences are emitted: the global entry already covers the
			// nested one, so the second is redundant rather than wrong, and
			// dropping it would mean reasoning about which candidate subsumes
			// which — a second filtering pass to get right for no correctness gain.
			name:      "a repeated field name is emitted when every occurrence drifted",
			exp:       `{"requestId":"a","user":{"requestId":"a"}}`,
			act:       `{"requestId":"b","user":{"requestId":"c"}}`,
			wantPaths: []string{"requestId", "user.requestId"},
		},
		{
			// childNoisePath("a","") yields "a.", a substring of a.b and every
			// other sibling.
			name:        "an empty JSON key yields a trailing dot that covers its siblings",
			exp:         `{"a":{"":1,"b":2}}`,
			act:         `{"a":{"":9,"b":2}}`,
			wantSkipped: []NoiseSkip{{Path: "a.", Reason: NoiseSkipOverBroad}},
		},
		{
			name:        "an empty body appearing is a structural change, not a parse error",
			exp:         ``,
			act:         `{"a":1}`,
			wantSkipped: []NoiseSkip{{Reason: NoiseSkipStructural}},
		},
		{
			name: "two absent bodies are not a difference",
			exp:  ``,
			act:  ``,
		},
		{
			name:        "scalar replaced by a container is refused",
			exp:         `{"a":1}`,
			act:         `{"a":{"b":1}}`,
			wantSkipped: []NoiseSkip{{Path: "a", Reason: NoiseSkipStructural}},
		},
		{
			name:        "array length change is refused",
			exp:         `{"items":[{"a":1}]}`,
			act:         `{"items":[{"a":1},{"a":2}]}`,
			wantSkipped: []NoiseSkip{{Path: "items", Reason: NoiseSkipArrayLength}},
		},
		{
			name:        "root scalar drift is refused rather than skipping the body",
			exp:         `"before"`,
			act:         `"after"`,
			wantSkipped: []NoiseSkip{{Path: "", Reason: NoiseSkipRootValue}},
		},
		{
			name:        "a key containing a dot is unrepresentable",
			exp:         `{"user.name":"a"}`,
			act:         `{"user.name":"b"}`,
			wantSkipped: []NoiseSkip{{Path: "user.name", Reason: NoiseSkipUnrepresentable}},
		},
		{
			name:        "a dotted ancestor makes its whole subtree unrepresentable",
			exp:         `{"a.b":{"c":1}}`,
			act:         `{"a.b":{"c":2}}`,
			wantSkipped: []NoiseSkip{{Path: "a.b.c", Reason: NoiseSkipUnrepresentable}},
		},
		{
			name:        "non-JSON is reported, not guessed at",
			exp:         `not json`,
			act:         `{"a":1}`,
			wantSkipped: []NoiseSkip{{Reason: NoiseSkipInvalidJSON}},
		},
		{
			name: "identical bodies produce nothing",
			exp:  `{"a":1,"items":[{"b":2}]}`,
			act:  `{"a":1,"items":[{"b":2}]}`,
		},
		{
			name:        "a drifting scalar and a schema change are reported separately",
			exp:         `{"stock":1,"gone":2}`,
			act:         `{"stock":9}`,
			wantPaths:   []string{"stock"},
			wantSkipped: []NoiseSkip{{Path: "gone", Reason: NoiseSkipStructural}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			paths, skipped := BodyNoiseFromJSONDiff(tt.exp, tt.act, nil, false)

			if len(paths) != len(tt.wantPaths) || (len(paths) > 0 && !reflect.DeepEqual(paths, tt.wantPaths)) {
				t.Errorf("paths = %v, want %v", paths, tt.wantPaths)
			}
			if len(skipped) != len(tt.wantSkipped) || (len(skipped) > 0 && !reflect.DeepEqual(skipped, tt.wantSkipped)) {
				t.Errorf("skipped = %+v, want %+v", skipped, tt.wantSkipped)
			}

			// No emitted path may carry an array position — the failure that
			// started all of this.
			if !tt.numericKeysAreReal {
				for _, p := range paths {
					for _, seg := range strings.Split(p, ".") {
						if isAllDigits(seg) {
							t.Errorf("emitted path %q carries an array position", p)
						}
					}
				}
			}
		})
	}
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// TestBodyNoiseFromJSONDiff_RoundTripsThroughTheMatcher is the contract this
// package exists to keep. For every producing case it asserts BOTH halves:
//
//	the emitted paths make the real body comparison pass, and
//	they do not excuse a drift on some other field.
//
// It fails if either side of the contract moves — if a producer changes dialect,
// or if the walker starts keying array elements by position. That second half is
// the one that has no other guard: an unmatchable path does not error, it just
// silently stops asserting.
func TestBodyNoiseFromJSONDiff_RoundTripsThroughTheMatcher(t *testing.T) {
	for _, tt := range []struct {
		name          string
		exp, act      string
		tamper        string // a body differing from exp in a field that is NOT noise
		tamperMatters bool
	}{
		{
			name:          "array of objects",
			exp:           `{"items":[{"product":{"stock":99,"id":"p1"}}],"total":10}`,
			act:           `{"items":[{"product":{"stock":100,"id":"p1"}}],"total":10}`,
			tamper:        `{"items":[{"product":{"stock":100,"id":"HACKED"}}],"total":10}`,
			tamperMatters: true,
		},
		{
			name:          "sibling scalar under the same parent",
			exp:           `{"o":{"ts":"a","amount":5}}`,
			act:           `{"o":{"ts":"b","amount":5}}`,
			tamper:        `{"o":{"ts":"b","amount":6}}`,
			tamperMatters: true,
		},
		{
			name:          "numeric object key",
			exp:           `{"data":{"2026":{"count":1,"label":"x"}}}`,
			act:           `{"data":{"2026":{"count":2,"label":"x"}}}`,
			tamper:        `{"data":{"2026":{"count":2,"label":"TAMPERED"}}}`,
			tamperMatters: true,
		},
		{
			name:          "multiple elements drifting on the same field",
			exp:           `{"rows":[{"t":1,"k":"a"},{"t":2,"k":"b"}]}`,
			act:           `{"rows":[{"t":9,"k":"a"},{"t":8,"k":"b"}]}`,
			tamper:        `{"rows":[{"t":9,"k":"a"},{"t":8,"k":"ZZ"}]}`,
			tamperMatters: true,
		},
		{
			name:          "nested arrays",
			exp:           `{"m":[[{"deep":1,"keep":"s"}]]}`,
			act:           `{"m":[[{"deep":2,"keep":"s"}]]}`,
			tamper:        `{"m":[[{"deep":2,"keep":"CHANGED"}]]}`,
			tamperMatters: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			paths, skipped := BodyNoiseFromJSONDiff(tt.exp, tt.act, nil, false)
			if len(paths) == 0 {
				t.Fatalf("no noise derived (skipped=%+v); this row is supposed to produce", skipped)
			}
			noise := noiseFor(paths)

			if !bodyMatches(t, tt.exp, tt.act, noise) {
				t.Errorf("derived noise %v did not suppress its own drift", paths)
			}
			if tt.tamperMatters && bodyMatches(t, tt.exp, tt.tamper, noise) {
				t.Errorf("derived noise %v also suppressed an unrelated field — over-broad", paths)
			}
			// Without the noise the drift must be a real failure, otherwise the
			// row proves nothing.
			if bodyMatches(t, tt.exp, tt.act, nil) {
				t.Errorf("row does not actually differ; the assertion above is vacuous")
			}
		})
	}
}

// TestBodyNoiseFromJSONDiff_Converges pins the property that stops the noise
// list growing without bound: feeding a round's own output back as `known` must
// produce nothing the second time.
func TestBodyNoiseFromJSONDiff_Converges(t *testing.T) {
	exp := `{"items":[{"stock":1}],"ts":"a"}`
	act := `{"items":[{"stock":2}],"ts":"b"}`

	first, _ := BodyNoiseFromJSONDiff(exp, act, nil, false)
	if len(first) == 0 {
		t.Fatal("first round produced nothing")
	}
	second, _ := BodyNoiseFromJSONDiff(exp, act, noiseFor(first), false)
	if len(second) != 0 {
		t.Errorf("second round re-emitted %v; rounds do not converge", second)
	}
}

// TestBodyNoiseFromJSONDiff_RefusesRatherThanOverSuppress is the other half of
// the round-trip contract, and the one with teeth: for every case where the
// obvious path would also cover a field that did NOT drift, nothing may be
// emitted. Emitting it would make the case go green while silently giving up an
// assertion — the same harm as the bug this package fixes, pointing the other
// way.
//
// The round-trip test alone cannot catch this: its tamper fields are chosen not
// to collide, so an over-broad path would still satisfy it.
func TestBodyNoiseFromJSONDiff_RefusesRatherThanOverSuppress(t *testing.T) {
	for _, tt := range []struct {
		name        string
		exp, act    string
		tamper      string // differs from exp only in a field that did NOT drift
		wantRefusal NoiseSkipReason
	}{
		{
			name:        "substring sibling",
			exp:         `{"items":[{"id":"a","idempotency_key":"k1"}]}`,
			act:         `{"items":[{"id":"b","idempotency_key":"k1"}]}`,
			tamper:      `{"items":[{"id":"a","idempotency_key":"TAMPERED"}]}`,
			wantRefusal: NoiseSkipOverBroad,
		},
		{
			name:        "global key repeating at depth",
			exp:         `{"requestId":"a","user":{"requestId":"a"}}`,
			act:         `{"requestId":"b","user":{"requestId":"a"}}`,
			tamper:      `{"requestId":"a","user":{"requestId":"TAMPERED"}}`,
			wantRefusal: NoiseSkipOverBroad,
		},
		{
			name:        "empty key covering its siblings",
			exp:         `{"a":{"":1,"b":2}}`,
			act:         `{"a":{"":9,"b":2}}`,
			tamper:      `{"a":{"":1,"b":99}}`,
			wantRefusal: NoiseSkipOverBroad,
		},
		{
			name:        "array length change",
			exp:         `{"items":[{"a":1}],"keep":"s"}`,
			act:         `{"items":[{"a":1},{"a":2}],"keep":"s"}`,
			tamper:      `{"items":[{"a":1}],"keep":"TAMPERED"}`,
			wantRefusal: NoiseSkipArrayLength,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			paths, skipped := BodyNoiseFromJSONDiff(tt.exp, tt.act, nil, false)
			if len(paths) != 0 {
				t.Fatalf("emitted %v; that path also covers a field that did not drift", paths)
			}
			found := false
			for _, s := range skipped {
				if s.Reason == tt.wantRefusal {
					found = true
				}
			}
			if !found {
				t.Errorf("skipped = %+v, want a %s refusal", skipped, tt.wantRefusal)
			}

			// Prove the refusal was worth making: had the path been emitted, the
			// tampered field would have stopped being asserted. Emitting the
			// obvious candidate and checking the tamper now passes demonstrates
			// exactly what was avoided.
			candidate, _ := shortestPathFor(tt.exp, tt.act)
			if candidate == "" {
				return
			}
			if !bodyMatches(t, tt.exp, tt.tamper, noiseFor([]string{candidate})) {
				t.Errorf("control failed: %q does not actually over-suppress, so this row proves nothing", candidate)
			}
		})
	}
}

// shortestPathFor re-derives the naive candidate — the path the producer would
// have emitted before the coverage check — so the test above can demonstrate the
// damage that check prevents.
func shortestPathFor(exp, act string) (string, bool) {
	var e, a interface{}
	if json.Unmarshal([]byte(exp), &e) != nil || json.Unmarshal([]byte(act), &a) != nil {
		return "", false
	}
	w := &noiseCandidateWalk{seen: map[string]bool{}, skipSeen: map[NoiseSkip]bool{}, drifted: map[string]bool{}, knownGlobalRegs: map[string][]*regexp.Regexp{}}
	w.walk("", "", e, a, false, false)
	if len(w.paths) == 0 {
		return "", false
	}
	return w.paths[0], true
}

// With ignoreOrdering the matcher pairs array elements greedily, so a positional
// walk would invent drift on every field a reorder moved.
func TestBodyNoiseFromJSONDiff_IgnoreOrdering(t *testing.T) {
	const (
		exp = `{"items":[{"id":"a","ts":"t1"},{"id":"b","ts":"t2"}]}`
		// The same two elements, swapped: not a difference at all under
		// ignoreOrdering.
		reordered = `{"items":[{"id":"b","ts":"t2"},{"id":"a","ts":"t1"}]}`
		// Reordered AND drifting, so which element moved is unknowable.
		reorderedAndDrifted = `{"items":[{"id":"b","ts":"t9"},{"id":"a","ts":"t8"}]}`
	)

	t.Run("a pure reordering yields nothing", func(t *testing.T) {
		paths, skipped := BodyNoiseFromJSONDiff(exp, reordered, nil, true)
		if len(paths) != 0 || len(skipped) != 0 {
			t.Errorf("paths=%v skipped=%+v, want both empty", paths, skipped)
		}
	})

	t.Run("an unpairable difference is refused, not attributed", func(t *testing.T) {
		paths, skipped := BodyNoiseFromJSONDiff(exp, reorderedAndDrifted, nil, true)
		if len(paths) != 0 {
			t.Errorf("paths = %v; element pairing is unknowable here, so no path is trustworthy", paths)
		}
		if len(skipped) != 1 || skipped[0].Reason != NoiseSkipOrderAmbiguous {
			t.Errorf("skipped = %+v, want ORDER_AMBIGUOUS", skipped)
		}
	})

	t.Run("positional walking is still used when ordering matters", func(t *testing.T) {
		paths, _ := BodyNoiseFromJSONDiff(exp, `{"items":[{"id":"a","ts":"t9"},{"id":"b","ts":"t8"}]}`, nil, false)
		if len(paths) != 1 || paths[0] != "items.ts" {
			t.Errorf("paths = %v, want [items.ts]", paths)
		}
	})
}

// A known entry carrying patterns only excuses the values those patterns match.
// Treating it as an unconditional skip made the producer return nothing at all
// for a live drift, leaving the case red with no reason attached.
func TestBodyNoiseFromJSONDiff_PatternGuardedKnownDoesNotSwallowDrift(t *testing.T) {
	const (
		exp = `{"o":{"ts":"2024-01-01"}}`
		act = `{"o":{"ts":"2025-01-01"}}`
	)

	for _, known := range []map[string][]string{
		{"o.ts": {"^2024-"}}, // dotted form
		{"ts": {"^2024-"}},   // global form
	} {
		// Control: the matcher itself does NOT excuse this value, so the producer
		// must not believe it is already covered.
		if bodyMatches(t, exp, act, known) {
			t.Fatalf("control failed: matcher already excuses %v, so this case proves nothing", known)
		}
		paths, skipped := BodyNoiseFromJSONDiff(exp, act, known, false)
		if len(paths) != 1 || paths[0] != "o.ts" {
			t.Errorf("known=%v: paths = %v, skipped = %+v; want [o.ts]", known, paths, skipped)
		}
	}

	// A pattern that DOES match the replayed value is genuine coverage.
	matching := map[string][]string{"o.ts": {"^2025-"}}
	if paths, _ := BodyNoiseFromJSONDiff(exp, act, matching, false); len(paths) != 0 {
		t.Errorf("paths = %v, want none: the existing pattern already covers this value", paths)
	}
}

// An empty object or array contributes no LEAF, so a coverage index built only
// from leaves cannot see it — and a candidate that is a string-prefix of its key
// then silences the whole subtree, including the assertion that it stayed empty.
// Injecting into an empty container is exactly the payload that hides behind it.
func TestBodyNoiseFromJSONDiff_EmptyContainersAreCollateral(t *testing.T) {
	for _, tt := range []struct {
		name             string
		exp, act, tamper string
	}{
		{
			name:   "empty array sibling",
			exp:    `{"r":{"id":"1","idx":[]}}`,
			act:    `{"r":{"id":"2","idx":[]}}`,
			tamper: `{"r":{"id":"1","idx":[{"x":1}]}}`,
		},
		{
			name:   "empty object sibling",
			exp:    `{"r":{"id":"1","idx":{}}}`,
			act:    `{"r":{"id":"2","idx":{}}}`,
			tamper: `{"r":{"id":"1","idx":{"injected":"evil"}}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			paths, skipped := BodyNoiseFromJSONDiff(tt.exp, tt.act, nil, false)
			if len(paths) != 0 {
				t.Fatalf("emitted %v; %q is a prefix of the empty container's key, so it blindfolds it", paths, paths)
			}
			var over bool
			for _, s := range skipped {
				if s.Reason == NoiseSkipOverBroad {
					over = true
				}
			}
			if !over {
				t.Errorf("skipped = %+v, want an OVER_BROAD refusal", skipped)
			}
			// Control: had r.id been emitted, injecting into the empty container
			// would have stopped failing.
			if !bodyMatches(t, tt.exp, tt.tamper, noiseFor([]string{"r.id"})) {
				t.Errorf("control failed: r.id does not actually silence the container")
			}
		})
	}
}

// Two sibling keys differing only in case share one traversal key, so the matcher
// cannot address them separately. A drift on one must not vouch for the other.
func TestBodyNoiseFromJSONDiff_CaseCollidingSiblings(t *testing.T) {
	exp, act := `{"K":"1","k":"9"}`, `{"K":"2","k":"9"}`
	paths, skipped := BodyNoiseFromJSONDiff(exp, act, nil, false)
	if len(paths) != 0 {
		t.Errorf("emitted %v; the sibling %q shares this traversal key and did not drift", paths, "k")
	}
	if len(skipped) == 0 {
		t.Errorf("expected a refusal, got none")
	}
}

// A pattern-guarded entry on a CONTAINER sets the walker's ancestorNoisy, after
// which every descendant value is accepted before the pattern is consulted. The
// producer must model that, or it appends a permanent unconditional entry on top
// of the user's pattern-guarded one on every single round.
func TestBodyNoiseFromJSONDiff_PatternGuardedAncestorCoversDescendants(t *testing.T) {
	var (
		known    = map[string][]string{"o.p": {"^zzz$"}}
		exp, act = `{"o":{"p":{"q":"v1"}}}`, `{"o":{"p":{"q":"v2"}}}`
	)
	// Control: the matcher itself already accepts this.
	if !bodyMatches(t, exp, act, known) {
		t.Fatal("control failed: the matcher does not accept this, so the case proves nothing")
	}
	if paths, skipped := BodyNoiseFromJSONDiff(exp, act, known, false); len(paths) != 0 {
		t.Errorf("paths = %v (skipped %+v), want none: an ancestor entry already covers this value", paths, skipped)
	}
}

// A field already silenced unconditionally is not collateral — an entry that also
// covers it takes away nothing still being asserted. Without this the drifting
// sibling could never be auto-noised, and the case stayed red forever.
func TestBodyNoiseFromJSONDiff_AlreadySilencedIsNotCollateral(t *testing.T) {
	exp, act := `{"a":{"id":"1","idx":"p"}}`, `{"a":{"id":"2","idx":"q"}}`

	for _, known := range []map[string][]string{
		{"idx": {}},   // global form
		{"a.idx": {}}, // dotted form
	} {
		paths, skipped := BodyNoiseFromJSONDiff(exp, act, known, false)
		if len(paths) != 1 || paths[0] != "a.id" {
			t.Errorf("known=%v: paths = %v, skipped = %+v; want [a.id]", known, paths, skipped)
		}
	}
}

// Every element of an array shares the array's traversal key, so for an array of
// SCALARS the derived path is that key however the elements pair. Refusing there
// would make enabling ignoreOrdering strictly worse than leaving it off.
func TestBodyNoiseFromJSONDiff_ScalarArraysArePairingIndependent(t *testing.T) {
	exp, act := `{"ids":["a","b"]}`, `{"ids":["a","c"]}`

	paths, skipped := BodyNoiseFromJSONDiff(exp, act, nil, true)
	if len(paths) != 1 || paths[0] != "ids" {
		t.Fatalf("paths = %v, skipped = %+v; want [ids] even with ignoreOrdering", paths, skipped)
	}
	// And it really is honoured under the same ordering mode.
	e, a := exp, act
	v, err := ValidateAndMarshalJSON(zap.NewNop(), &e, &a)
	if err != nil {
		t.Fatalf("ValidateAndMarshalJSON: %v", err)
	}
	res, err := JSONDiffWithNoiseControl(v, noiseFor(paths), true, zap.NewNop())
	if err != nil || !res.IsExact() {
		t.Errorf("derived noise did not suppress the drift under ignoreOrdering (err=%v)", err)
	}
}

// A pattern that matches an ADDED field excuses it in the walker, because there
// is a replayed value for the pattern to describe. Reporting a refusal for a case
// the matcher passes would surface a schema change on a green run.
func TestBodyNoiseFromJSONDiff_PatternExcusesAnAddedField(t *testing.T) {
	var (
		known    = map[string][]string{"b": {"^tok-"}}
		exp, act = `{"a":1}`, `{"a":1,"b":"tok-9"}`
	)
	if !bodyMatches(t, exp, act, known) {
		t.Fatal("control failed: the matcher rejects this, so the case proves nothing")
	}
	if paths, skipped := BodyNoiseFromJSONDiff(exp, act, known, false); len(paths) != 0 || len(skipped) != 0 {
		t.Errorf("paths = %v skipped = %+v, want both empty: the pattern already excuses the added field", paths, skipped)
	}
}

// The coverage index must consider BOTH documents: a field the replay added is
// still something an entry can silence.
func TestBodyNoiseFromJSONDiff_CoverageConsidersTheReplayedBody(t *testing.T) {
	// `a.id` drifts; the replay adds `a.idx`, which `a.id` would also cover.
	exp, act := `{"a":{"id":"1"}}`, `{"a":{"id":"2","idx":"new"}}`
	paths, _ := BodyNoiseFromJSONDiff(exp, act, nil, false)
	for _, p := range paths {
		if p == "a.id" {
			t.Errorf("emitted a.id, which also covers the replay-only field a.idx")
		}
	}
}

// Refusals must be sorted for the same reason paths are: they reach a report and
// a log, and Go map order would reshuffle them on every run.
func TestBodyNoiseFromJSONDiff_SkippedIsSorted(t *testing.T) {
	exp := `{"z":1,"a":1,"m":1}`
	act := `{"z":{"x":1},"a":{"x":1},"m":{"x":1}}`
	_, skipped := BodyNoiseFromJSONDiff(exp, act, nil, false)
	if len(skipped) != 3 {
		t.Fatalf("skipped = %+v, want 3 refusals", skipped)
	}
	for i := 1; i < len(skipped); i++ {
		if skipped[i-1].Path > skipped[i].Path {
			t.Errorf("skipped is not sorted: %+v", skipped)
		}
	}
}

// TestBodyNoiseFromJSONDiff_MatcherHasTheLastWord pins the backstop.
//
// Everything else in this file reasons ABOUT the matcher's noise semantics, and
// each of those rules is a place the walk can drift from the walker. A drift that
// under-reports is a false green: the caller writes the derived entries, sees no
// refusal, and marks a case resolved that fails again on the next replay. So the
// derivation is finally checked against the matcher itself, which cannot drift.
//
// In the case below the walk emits a path that changes nothing. `w.ids` already
// carries a pattern, and on an array of SCALARS the matcher keeps applying that
// pattern per element rather than relaxing the array, so the out-of-pattern
// element is a real difference. The walk correctly declines to treat it as
// covered and emits `w.ids` — but the write path will not overwrite a pattern its
// author narrowed on purpose, so nothing would actually change on disk and the
// next replay fails identically. Only asking the matcher catches that.
//
// (A dot-free `ids` would behave differently: as a GLOBAL key its pattern has no
// single value to match against an array, so the matcher covers the array
// wholesale. Hence the nesting here.)
func TestBodyNoiseFromJSONDiff_MatcherHasTheLastWord(t *testing.T) {
	var (
		known    = map[string][]string{"w.ids": {"^a"}}
		exp, act = `{"w":{"ids":["a1","a2"]}}`, `{"w":{"ids":["a1","zz"]}}`
	)
	// Control: the matcher rejects this today.
	if bodyMatches(t, exp, act, known) {
		t.Fatal("control failed: the matcher already accepts this, so the case proves nothing")
	}

	_, skipped := BodyNoiseFromJSONDiff(exp, act, known, false)
	var unresolved bool
	for _, s := range skipped {
		if s.Reason == NoiseSkipUnresolved {
			unresolved = true
		}
	}
	if !unresolved {
		t.Errorf("skipped = %+v, want an UNRESOLVED refusal: the derived entries do not satisfy the matcher", skipped)
	}
}

// The backstop must stay quiet when the derivation genuinely works, or every
// resolvable case would be held failed.
func TestBodyNoiseFromJSONDiff_BackstopIsSilentOnASolvedCase(t *testing.T) {
	exp, act := `{"items":[{"product":{"stock":99}}],"total":10}`, `{"items":[{"product":{"stock":100}}],"total":10}`
	paths, skipped := BodyNoiseFromJSONDiff(exp, act, nil, false)
	if len(paths) != 1 || len(skipped) != 0 {
		t.Errorf("paths = %v, skipped = %+v; want one path and no refusal", paths, skipped)
	}
}

// SplitNoise turns every single-segment body entry — `body.user: []` — into the
// dot-free key `user`, which the matcher treats as GLOBAL: it skips that child's
// whole subtree wherever it appears, structure included, and with
// containersCovered=true so even a pattern-guarded entry does. Walking into it
// anyway invented refusals for differences the matcher already excuses.
func TestBodyNoiseFromJSONDiff_GlobalEntryCoversAPresentContainer(t *testing.T) {
	for _, known := range []map[string][]string{
		{"user": {}},        // unconditional
		{"user": {"^zzz$"}}, // pattern that cannot describe a container
	} {
		t.Run("known="+fmt.Sprint(known), func(t *testing.T) {
			exp := `{"user":{"a":1},"total":10}`
			act := `{"user":{"b":2},"total":11}`

			paths, skipped := BodyNoiseFromJSONDiff(exp, act, known, false)
			if len(skipped) != 0 {
				t.Errorf("skipped = %+v, want none: the entry already covers that subtree", skipped)
			}
			if len(paths) != 1 || paths[0] != "total" {
				t.Fatalf("paths = %v, want [total]", paths)
			}
			// And the derived entry really does finish the job.
			effective := map[string][]string{}
			for k, v := range known {
				effective[k] = v
			}
			for k, v := range noiseFor(paths) {
				effective[k] = v
			}
			if !bodyMatches(t, exp, act, effective) {
				t.Errorf("the case is not resolved by %v on top of %v", paths, known)
			}
		})
	}

	// A difference the entry already excuses must not produce a redundant path
	// either — it would burn a slot in the caller's per-case bound.
	if paths, _ := BodyNoiseFromJSONDiff(
		`{"user":{"id":"1"}}`, `{"user":{"id":"2"}}`, map[string][]string{"user": {}}, false,
	); len(paths) != 0 {
		t.Errorf("paths = %v, want none: the matcher already excuses this", paths)
	}
}

// A key containing a literal dot shares its traversal key with a genuinely
// nested path, so the two must not be treated as one node: a drift on the nested
// one would otherwise vouch for a dotted sibling that never moved.
func TestBodyNoiseFromJSONDiff_LiteralDotKeyDoesNotAliasNesting(t *testing.T) {
	exp := `{"a.b":{"c":"secret"},"a":{"b":{"c":1}}}`
	act := `{"a.b":{"c":"secret"},"a":{"b":{"c":2}}}`

	paths, skipped := BodyNoiseFromJSONDiff(exp, act, nil, false)
	if len(paths) != 0 {
		t.Fatalf("emitted %v; that path also silences [\"a.b\"][\"c\"], which did not drift", paths)
	}
	var over bool
	for _, s := range skipped {
		if s.Reason == NoiseSkipOverBroad {
			over = true
		}
	}
	if !over {
		t.Errorf("skipped = %+v, want an OVER_BROAD refusal", skipped)
	}
	// Control: emitting it really would have given up the assertion.
	tamper := `{"a.b":{"c":"TAMPERED"},"a":{"b":{"c":2}}}`
	if !bodyMatches(t, exp, tamper, noiseFor([]string{"a.b.c"})) {
		t.Errorf("control failed: a.b.c does not actually over-suppress here")
	}
}

// A dotted pattern on a field the replay ADDED excuses it, because there is a
// replayed value for the pattern to describe. Only the global form was covered.
func TestBodyNoiseFromJSONDiff_DottedPatternExcusesAnAddedField(t *testing.T) {
	var (
		known    = map[string][]string{"a.b": {"^tok-"}}
		exp, act = `{"a":{"x":1}}`, `{"a":{"x":1,"b":"tok-9"}}`
	)
	if !bodyMatches(t, exp, act, known) {
		t.Fatal("control failed: the matcher rejects this, so the case proves nothing")
	}
	if paths, skipped := BodyNoiseFromJSONDiff(exp, act, known, false); len(paths) != 0 || len(skipped) != 0 {
		t.Errorf("paths = %v skipped = %+v, want both empty", paths, skipped)
	}
}

// A pattern-guarded global does NOT excuse a field the replay dropped: there is
// no value left for the pattern to match, so only an unconditional entry covers
// it. Getting this wrong hides a removed field.
func TestBodyNoiseFromJSONDiff_PatternGuardedGlobalDoesNotExcuseARemovedField(t *testing.T) {
	var (
		known    = map[string][]string{"b": {"^tok-"}}
		exp, act = `{"a":1,"b":"tok-9"}`, `{"a":1}`
	)
	if bodyMatches(t, exp, act, known) {
		t.Fatal("control failed: the matcher accepts this, so the case proves nothing")
	}
	_, skipped := BodyNoiseFromJSONDiff(exp, act, known, false)
	var structural bool
	for _, s := range skipped {
		if s.Path == "b" && s.Reason == NoiseSkipStructural {
			structural = true
		}
	}
	if !structural {
		t.Errorf("skipped = %+v, want a STRUCTURAL refusal for the dropped field", skipped)
	}
}

// A dot-free known entry must be matched as a GLOBAL key — exact field name at
// any depth — not as the substring the dotted index uses. `id` must not be read
// as covering `idempotency_key`.
func TestBodyNoiseFromJSONDiff_GlobalKeyIsNotASubstringMatch(t *testing.T) {
	var (
		known    = map[string][]string{"id": {}}
		exp, act = `{"id":"a","idempotency_key":"k1"}`, `{"id":"b","idempotency_key":"k2"}`
	)
	paths, _ := BodyNoiseFromJSONDiff(exp, act, known, false)
	if len(paths) != 1 || paths[0] != "idempotency_key" {
		t.Errorf("paths = %v, want [idempotency_key]: a global `id` does not cover it", paths)
	}
}

// An empty JSON key at the document root is unaddressable — the joined path is
// the empty string. Reporting it as a ROOT_VALUE drift would advise switching
// off the entire body assertion over one field.
func TestBodyNoiseFromJSONDiff_EmptyRootKeyIsNotARootValueDrift(t *testing.T) {
	_, skipped := BodyNoiseFromJSONDiff(`{"":1}`, `{"":2}`, nil, false)
	if len(skipped) != 1 || skipped[0].Reason != NoiseSkipUnrepresentable {
		t.Errorf("skipped = %+v, want a single UNREPRESENTABLE refusal", skipped)
	}
	// A genuine root scalar still reports ROOT_VALUE.
	if _, s := BodyNoiseFromJSONDiff(`"a"`, `"b"`, nil, false); len(s) != 1 || s[0].Reason != NoiseSkipRootValue {
		t.Errorf("skipped = %+v, want ROOT_VALUE for a drifting root scalar", s)
	}
}

// A dot-free entry is a global key — an exact field-name match at any depth —
// so the walk must consult it the way the matcher does, not as a substring.
func TestBodyNoiseFromJSONDiff_HonoursExistingGlobalKey(t *testing.T) {
	exp := `{"id":"a","nested":{"id":"b"},"other":1}`
	act := `{"id":"x","nested":{"id":"y"},"other":2}`

	paths, _ := BodyNoiseFromJSONDiff(exp, act, map[string][]string{"id": {}}, false)
	if !reflect.DeepEqual(paths, []string{"other"}) {
		t.Errorf("paths = %v, want [other]: a global `id` already covers both id fields", paths)
	}
}

func TestUnmatchableBodyNoise(t *testing.T) {
	body := `{"items":[{"product":{"stock":99}}],"data":{"2026":{"count":1}},"id":"x"}`

	for _, tt := range []struct {
		name  string
		noise map[string][]string
		want  []string
	}{
		{
			name:  "indexed path is dead",
			noise: map[string][]string{"items.0.product.stock": {}},
			want:  []string{"items.0.product.stock"},
		},
		{
			name:  "index-free path is alive",
			noise: map[string][]string{"items.product.stock": {}},
		},
		{
			name:  "numeric object key is alive and must not be pruned",
			noise: map[string][]string{"data.2026.count": {}},
		},
		{
			name:  "undecoded RFC-6901 escape is dead",
			noise: map[string][]string{"a~1b": {}},
			want:  []string{"a~1b"},
		},
		{
			name:  "container path is alive",
			noise: map[string][]string{"items.product": {}},
		},
		{
			name:  "dot-free key matching a field name is alive",
			noise: map[string][]string{"id": {}},
		},
		{
			name:  "dot-free key matching nothing is dead",
			noise: map[string][]string{"nosuchfield": {}},
			want:  []string{"nosuchfield"},
		},
		{
			name:  "a dot-free key must not be revived by a substring match",
			noise: map[string][]string{"toc": {}}, // substring of "stock"
			want:  []string{"toc"},
		},
		{
			name:  "dead and alive together",
			noise: map[string][]string{"items.0.product.stock": {}, "items.product.stock": {}, "gone.away": {}},
			want:  []string{"gone.away", "items.0.product.stock"},
		},
		{
			name: "empty noise reports nothing",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := UnmatchableBodyNoise(tt.noise, body)
			if len(got) != len(tt.want) || (len(got) > 0 && !reflect.DeepEqual(got, tt.want)) {
				t.Errorf("UnmatchableBodyNoise = %v, want %v", got, tt.want)
			}
		})
	}
}

// An entry may legitimately name a field only the replay carries, so every body
// the entry could describe is consulted before calling it dead.
func TestUnmatchableBodyNoise_ConsidersEveryBody(t *testing.T) {
	recorded := `{"a":1}`
	replayed := `{"a":1,"b":2}`

	if got := UnmatchableBodyNoise(map[string][]string{"b": {}}, recorded); len(got) != 1 {
		t.Errorf("with only the recorded body, `b` should read as dead, got %v", got)
	}
	if got := UnmatchableBodyNoise(map[string][]string{"b": {}}, recorded, replayed); len(got) != 0 {
		t.Errorf("`b` exists in the replayed body and must not be pruned, got %v", got)
	}
}

// A body that will not parse is not evidence, so nothing may be pruned on its
// account — otherwise one malformed replay would strip a suite's noise.
func TestUnmatchableBodyNoise_UnparseableBodyProvesNothing(t *testing.T) {
	if got := UnmatchableBodyNoise(map[string][]string{"items.0.stock": {}}, "<html>not json</html>"); got != nil {
		t.Errorf("got %v, want nil for an unparseable body", got)
	}
	if got := UnmatchableBodyNoise(map[string][]string{"items.0.stock": {}}, "", "   "); got != nil {
		t.Errorf("got %v, want nil when no body is present", got)
	}
	// The dangerous shape: one body parses and the other does not. Judging the
	// entry against only the readable half would call a live entry dead, and the
	// caller PRUNES on that — one malformed replay would strip a suite's noise.
	if got := UnmatchableBodyNoise(map[string][]string{"b": {}}, `{"a":1}`, "<html>"); got != nil {
		t.Errorf("got %v, want nil: the unreadable body may well have carried `b`", got)
	}
	if got := UnmatchableBodyNoise(map[string][]string{"b": {}}, `{"a":1}`, ""); got != nil {
		t.Errorf("got %v, want nil: an absent body proves nothing either", got)
	}
	// With every body readable it still reports the genuinely dead entry.
	if got := UnmatchableBodyNoise(map[string][]string{"b": {}}, `{"a":1}`, `{"a":2}`); len(got) != 1 {
		t.Errorf("got %v, want [b] when both bodies are readable", got)
	}
}

// The matcher lowercases both the noise key and the traversal key, so this must
// too. It matters because this result is not only advisory: the cloud auto-noise
// write-back (k8s-proxy, enterprise) PRUNES the entries it names, so reporting a
// live entry as dead would delete it from the user's test case and silently
// restore an assertion they had switched off.
func TestUnmatchableBodyNoise_IsCaseInsensitive(t *testing.T) {
	const body = `{"data":{"Timestamp":"t","Items":[{"Stock":1}]}}`

	for _, k := range []string{"data.Timestamp", "DATA.timestamp", "data.Items.Stock", "Stock"} {
		if got := UnmatchableBodyNoise(map[string][]string{k: {}}, body); len(got) != 0 {
			t.Errorf("%q reported dead (%v), but the matcher honours it", k, got)
		}
	}
	// The wildcard is a documented sentinel, not a field that can be missing.
	if got := UnmatchableBodyNoise(map[string][]string{"*": {}}, body); len(got) != 0 {
		t.Errorf("the wildcard sentinel was reported dead: %v", got)
	}
	// A genuinely dead entry is still reported, whatever its case.
	if got := UnmatchableBodyNoise(map[string][]string{"data.Items.0.Stock": {}}, body); len(got) != 1 {
		t.Errorf("got %v, want the indexed entry reported dead", got)
	}
}

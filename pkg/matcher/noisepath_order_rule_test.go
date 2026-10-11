package matcher

import (
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The golden for the response walker: a reorder in which every element also
// drifts one leaf. With order compared (ignoreOrdering false) the positional
// walk sees `id` and `expires_in` drift at every index, derives both as noise,
// and the matcher then accepts the response: the reorder goes green. Under the
// switch the order rule refuses the array instead.
func TestBodyNoiseFromJSONDiff_OrderRuleRefusesReorderPlusDrift(t *testing.T) {
	const (
		exp = `{"tokens":[{"id":"a","expires_in":100},{"id":"b","expires_in":200}]}`
		act = `{"tokens":[{"id":"b","expires_in":261},{"id":"a","expires_in":161}]}`
	)

	t.Run("switch off: today's positional walk learns the reorder as noise", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		paths, skipped := BodyNoiseFromJSONDiff(exp, act, nil, false)
		if len(skipped) != 0 || len(paths) == 0 {
			t.Fatalf("paths=%v skipped=%+v: the control must reproduce the false pass the rule closes", paths, skipped)
		}
		if !bodyMatches(t, exp, act, noiseFor(paths)) {
			t.Fatalf("control: the derived noise %v does not green the reordered response; this test no longer shows the hole", paths)
		}
	})

	for _, ignoreOrdering := range []bool{false, true} {
		t.Run("switch on refuses it", func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			paths, skipped := BodyNoiseFromJSONDiff(exp, act, nil, ignoreOrdering)
			if len(paths) != 0 {
				t.Fatalf("ignoreOrdering=%v: derived %v from a reordered array; noise on its fields only hides the reorder", ignoreOrdering, paths)
			}
			if len(skipped) != 1 || skipped[0].Reason != NoiseSkipUnstableOrder || skipped[0].Path != "tokens" {
				t.Fatalf("ignoreOrdering=%v: skipped = %+v, want one UNSTABLE_ORDER at tokens", ignoreOrdering, skipped)
			}
		})
	}
}

// A pure reorder is not a difference under ignoreOrdering, and it IS one under
// the switch (arrays compared in order): refused as unstable order.
func TestBodyNoiseFromJSONDiff_OrderRuleRefusesAPureReorder(t *testing.T) {
	const (
		exp = `{"items":[{"id":"a"},{"id":"b"}],"tags":["x","y"]}`
		act = `{"items":[{"id":"b"},{"id":"a"}],"tags":["y","x"]}`
	)
	t.Setenv(provenswitch.Env, "1")
	paths, skipped := BodyNoiseFromJSONDiff(exp, act, nil, true)
	if len(paths) != 0 {
		t.Fatalf("paths = %v, want none", paths)
	}
	want := map[string]bool{"items": true, "tags": true}
	for _, s := range skipped {
		if s.Reason != NoiseSkipUnstableOrder || !want[s.Path] {
			t.Fatalf("skipped = %+v, want UNSTABLE_ORDER at items and tags only", skipped)
		}
		delete(want, s.Path)
	}
	if len(want) != 0 {
		t.Fatalf("skipped = %+v, missing UNSTABLE_ORDER for %v (scalars reorder too)", skipped, want)
	}
}

// An array whose elements pair by index is walked as before: its drifting leaf
// is learned, and the learned noise really greens the response.
func TestBodyNoiseFromJSONDiff_OrderRuleWalksAnArrayThatPairsByIndex(t *testing.T) {
	const (
		exp = `{"items":[{"id":"a","ts":"t1"},{"id":"b","ts":"t2"}]}`
		act = `{"items":[{"id":"a","ts":"t9"},{"id":"b","ts":"t8"}]}`
	)
	t.Setenv(provenswitch.Env, "1")
	paths, skipped := BodyNoiseFromJSONDiff(exp, act, nil, true)
	if len(skipped) != 0 || len(paths) != 1 || paths[0] != "items.ts" {
		t.Fatalf("paths=%v skipped=%+v, want [items.ts] and no refusal", paths, skipped)
	}
	if !bodyMatches(t, exp, act, noiseFor(paths)) {
		t.Fatalf("the derived noise %v does not make the in-order comparison accept the response", paths)
	}
}

// An array the matcher already accepts as it stands has nothing to refuse. A
// pattern-guarded path entry relaxes the leaf values of an array of objects,
// and is applied per element to an array of scalars; either way the in-order
// matcher accepts these reordered values, so calling the array reordered would
// refuse a difference the matcher does not have.
func TestBodyNoiseFromJSONDiff_OrderRuleLeavesAnExcusedArrayAlone(t *testing.T) {
	const (
		exp = `{"data":{"items":[{"id":"a"},{"id":"b"}],"ids":["a","b"]},"n":1}`
		act = `{"data":{"items":[{"id":"b"},{"id":"a"}],"ids":["b","a"]},"n":2}`
	)
	t.Setenv(provenswitch.Env, "1")
	known := map[string][]string{"data.items": {".*"}, "data.ids": {"^[a-z]$"}}
	paths, skipped := BodyNoiseFromJSONDiff(exp, act, known, false)
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none: both arrays are excused by the test case's own noise", skipped)
	}
	if len(paths) != 1 || paths[0] != "n" {
		t.Fatalf("paths = %v, want [n]", paths)
	}
}

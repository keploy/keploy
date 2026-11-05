package learn

import (
	"reflect"
	"strings"
	"testing"
)

// declaredKnown excuses, untyped, the leaves keploy.yml's noise names
// (declared) and those another untyped excuse names (values: an obfuscated
// value), and, typed, the noise learned onto the recording (learned). It
// declares the first alone.
type declaredKnown struct{ declared, values, learned map[string]bool }

func (k declaredKnown) Excused(l Leaf) (bool, bool) {
	switch {
	case k.learned[l.Path]:
		return true, true
	case k.declared[l.Path], k.values[l.Path]:
		return true, false
	}
	return false, false
}

func (k declaredKnown) Declares(l Leaf) bool { return k.declared[l.Path] }

// notDeclarer excuses as declaredKnown does, but is no Declarer.
type notDeclarer struct{ k declaredKnown }

func (n notDeclarer) Excused(l Leaf) (bool, bool) { return n.k.Excused(l) }

// keploy.yml's noise stops the comparison at a field, never the learn step's
// reading of it: a drifted value it alone excuses is Declared, never a
// drift, so the call stays exact (Pair) and the learn step can still read it
// as a variable's use. Every other excuse, and a leaf no one value holds on
// both sides, declares nothing.
func TestCompareNamesADriftKeployYmlNoiseExcusesAsDeclared(t *testing.T) {
	shape := func(leaves ...Leaf) Shape {
		s := Shape{Key: "k", Leaves: map[string]Leaf{}}
		for _, l := range leaves {
			s.Leaves[l.Path] = l
		}
		return s
	}
	str := func(p, v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	all := declaredKnown{declared: map[string]bool{"body.id": true, "body.gone": true, "body.kind": true, "body.u": true, "body.same": true}, values: map[string]bool{"body.secret": true}, learned: map[string]bool{"body.at": true, "body.both": true}}
	all.declared["body.both"] = true
	rows := []struct {
		name      string
		live, rec Shape
		known     Known
		declared  []string
		drift     []string
		exact     bool
	}{
		{"a drifted declared value", shape(str("body.id", "o-9"), str("body.n", "1")), shape(str("body.id", "o-1"), str("body.n", "1")), all, []string{"body.id"}, nil, true},
		{"declared beside a drift", shape(str("body.id", "o-9"), str("body.n", "2")), shape(str("body.id", "o-1"), str("body.n", "1")), all, []string{"body.id"}, []string{"body.n"}, false},
		{"an equal declared value", shape(str("body.same", "a")), shape(str("body.same", "a")), all, nil, nil, true},
		{"another untyped excuse", shape(str("body.secret", "b")), shape(str("body.secret", "a")), all, nil, nil, true},
		{"learned noise", shape(str("body.at", "b")), shape(str("body.at", "a")), all, nil, nil, true},
		{"learned noise at a declared field", shape(str("body.both", "b")), shape(str("body.both", "a")), all, nil, nil, true},
		{"a declared leaf on one side", shape(), shape(str("body.gone", "a")), all, nil, nil, true},
		{"a declared leaf of another type", shape(Leaf{Path: "body.kind", Noise: "body.kind", Type: Number, Value: "1"}), shape(str("body.kind", "1")), all, nil, nil, true},
		{"a declared leaf no path names alone", shape(Leaf{Path: "body.u", Noise: "body.u", Type: String, Value: "b", Unnamed: true}), shape(Leaf{Path: "body.u", Noise: "body.u", Type: String, Value: "a", Unnamed: true}), all, nil, nil, true},
		{"a Known that is no Declarer", shape(str("body.id", "o-9")), shape(str("body.id", "o-1")), notDeclarer{all}, nil, nil, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			d := Compare(r.live, r.rec, r.known)
			if d.Structural != "" || !reflect.DeepEqual(d.Declared, r.declared) || !reflect.DeepEqual(d.Drift, r.drift) {
				t.Fatalf("Compare = structural %q drift %v declared %v, want drift %v declared %v", d.Structural, d.Drift, d.Declared, r.drift, r.declared)
			}
			res := Pair(r.live, []Candidate{{Shape: r.rec, Known: r.known, Pairable: true}})
			if (res.Outcome == Exact) != r.exact || res.Index != 0 || !reflect.DeepEqual(res.Diff.Declared, r.declared) {
				t.Fatalf("Pair = %v index %d declared %v, want exact %v declared %v", res.Outcome, res.Index, res.Diff.Declared, r.exact, r.declared)
			}
			if _, places := missPlaces(r.live, r.rec, r.known); len(places) != 0 {
				t.Fatalf("missPlaces = %v: a declared drift is no place", places)
			}
		})
	}
}

// patternKnown declares a value its pattern matches (keploy.yml's noise
// entry with a pattern, here "o-" and digits) and excuses, untyped, those
// values and the values in obfuscated (another untyped excuse).
type patternKnown struct{ obfuscated map[string]bool }

func (k patternKnown) Excused(l Leaf) (bool, bool) {
	return k.obfuscated[l.Value] || k.Declares(l), false
}

func (k patternKnown) Declares(l Leaf) bool {
	return len(l.Value) > 2 && strings.HasPrefix(l.Value, "o-") && strings.Trim(l.Value[2:], "0123456789") == ""
}

// A noise entry with patterns constrains the live value as well as the
// recorded one: a drift is Declared only where the noise names both leaves.
// A live value no pattern matches is no declared drift, even where the
// recorded value is excused, by the pattern or by another excuse; nor is a
// live value the pattern matches beside a recorded value only another
// excuse covers.
func TestCompareDeclaresADriftOnlyWhereTheNoiseNamesBothSides(t *testing.T) {
	str := func(v string) Shape {
		return Shape{Key: "k", Leaves: map[string]Leaf{"body.id": {Path: "body.id", Noise: "body.id", Type: String, Value: v}}}
	}
	known := patternKnown{obfuscated: map[string]bool{"OBF-1": true}}
	for _, r := range []struct {
		name, rec, live string
		declared        []string
	}{
		{"both values match the pattern", "o-1", "o-9", []string{"body.id"}},
		{"the live value matches no pattern", "o-1", "x-9", nil},
		{"a recorded value only another excuse covers", "OBF-1", "o-9", nil},
	} {
		t.Run(r.name, func(t *testing.T) {
			d := Compare(str(r.live), str(r.rec), known)
			if d.Structural != "" || len(d.Drift) != 0 || !reflect.DeepEqual(d.Declared, r.declared) {
				t.Fatalf("Compare = structural %q drift %v declared %v, want declared %v", d.Structural, d.Drift, d.Declared, r.declared)
			}
		})
	}
}

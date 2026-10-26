// Package learn is the learn step of the proven model (design v14 §7.2, row
// L). It runs in record's full round F only: the round that replays, right
// after recording, the build that was just recorded. There a request that no
// recorded mock answers exactly is paired with the recorded mock it drifted
// from, and every value that drifted is learned as one of three things:
//
//   - noise: not compared from then on, its type still enforced;
//   - an echo: the mock's answer echoes the value, so it is a variable of
//     that call alone, bound at the request and answered with the live value
//     where the recorded answer held it;
//   - a variable: the value appeared earlier in the test's recording, and is
//     followed from that earlier place (bound there, asserted here).
//
// Echoes and variables are written as the mock's variables
// (models.MockVariables), which every replay after F follows (the HTTP
// parser's variable pass).
//
// Nothing is decided by a value's shape. F replays the build that made the
// recording, so whatever F sees change is non-determinism, never a
// regression, and only what F saw change is learned. A request that differs
// in anything L does not learn (a field added or removed, a type changed, an
// array reordered, a fixed part such as the method) is not paired: it is a
// miss, and its reason is judging's evidence (request_changed,
// unstable_order, unmocked_call).
//
// The package is pure. A protocol adapter turns each request into a Shape
// (its operation key, the parts that must be equal, and its learnable
// leaves), and Pair and Classify decide. The HTTP adapter lives with the HTTP
// parser.
package learn

import (
	"fmt"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// Type is a leaf's type. Two leaves of different types never pair: a type
// change is not a drifting value.
type Type byte

// Leaf types. A JSON body's empty object and empty array are leaves too, so
// that a container turning empty, or the reverse, is a change of shape.
const (
	String     Type = 's'
	Number     Type = 'n'
	Bool       Type = 'b'
	Null       Type = 'z'
	EmptyObj   Type = 'o'
	EmptyArray Type = 'a'
	// Keys is no leaf of a body but the keys of its JSON objects, one per
	// line (KeysLeaf): an earlier place a value may have come from, and an
	// answer that echoes one at a key, which keploy cannot answer there.
	Keys Type = 'k'
	// Instant is a field its protocol's decoder types as one instant (a
	// BSON UTC datetime or timestamp), written by InstantLeaf: compared
	// whole, never word by word (typed.go). Only a decoder makes a leaf
	// one: a string that reads as a date is a String.
	Instant Type = 't'
)

// Leaf is one learnable value of a request or a response.
type Leaf struct {
	// Path names the leaf exactly: object keys dot-joined, an array position
	// bracketed ("body.items[0].id", "header.authorization",
	// "url.query.page", "url.path.2"). What L learns is stored under it, so
	// what drifted in one element of an array is learned for that element
	// alone: F saw nothing of the others.
	Path string
	// Noise is the leaf's name in the request matcher's noise dialect, which
	// has no array positions ("body.items[].id"), the one keploy.yml's body
	// noise is written in: it is how declared noise names the leaf.
	Noise string
	Type  Type
	Value string
	// Unnamed: no path names the leaf alone (a JSON key holding '.', '[' or
	// ']'), so nothing can be learned about it, and a drift there is a change
	// L refuses.
	Unnamed bool
}

// Shape is one request as L compares it.
type Shape struct {
	// Key is the operation key (design §7.2 step 2). Only requests with the
	// same key are candidates for each other.
	Key string
	// Fixed are the parts that must be equal and are never learned (for HTTP
	// the media type and which headers are sent), by name.
	Fixed map[string]string
	// Leaves are the learnable leaves, by Path.
	Leaves map[string]Leaf
	// Body is the decoded JSON body (numbers as json.Number), for the order
	// rule. nil when the body is not JSON.
	Body interface{}
}

// Known says which leaves a comparison excuses: noise keploy.yml declares,
// and noise learned onto the mock earlier.
type Known interface {
	// Excused reports whether the leaf is not compared by value, and whether
	// its type is still enforced (typed). leaf is the recorded leaf, or the
	// live one when the recording lacks it. A typed leaf must also be
	// present on both sides; an untyped one may be missing from either.
	Excused(leaf Leaf) (excused, typed bool)
}

// None excuses nothing.
var None Known = none{}

type none struct{}

func (none) Excused(Leaf) (bool, bool) { return false, false }

// Diff is how a live request differs from a recorded one.
type Diff struct {
	// Structural names what differs that L does not learn (a fixed part, a
	// leaf present on one side only, a type change, a drift no path can
	// name), or "" when only values drifted.
	Structural string
	// Drift are the paths of the leaves whose value drifted, sorted.
	Drift []string
	// Reordered are the arrays the order rule refuses (matcher.ArrayReordered),
	// by path, sorted: they came back with the recorded elements in another
	// order.
	Reordered []string
}

// outside are the drifted leaves outside the reordered arrays, sorted: a
// reorder explains every positional difference inside its array.
func (d Diff) outside() []string {
	var out []string
	for _, p := range d.Drift {
		if !insideAny(p, d.Reordered) {
			out = append(out, p)
		}
	}
	return out
}

// count is the number of drifted leaves outside the reordered arrays.
func (d Diff) count() int { return len(d.outside()) }

// insideAny reports whether path is one of arrays or below one.
func insideAny(path string, arrays []string) bool {
	for _, a := range arrays {
		if path == a || strings.HasPrefix(path, a+"[") || strings.HasPrefix(path, a+".") {
			return true
		}
	}
	return false
}

// Compare says how live differs from rec, excusing what known excuses.
func Compare(live, rec Shape, known Known) Diff {
	if known == nil {
		known = None
	}
	var d Diff
	if name := fixedDiffers(live.Fixed, rec.Fixed); name != "" {
		d.Structural = name + " differs"
		return d
	}
	paths := make([]string, 0, len(rec.Leaves)+len(live.Leaves))
	for p := range rec.Leaves {
		paths = append(paths, p)
	}
	for p := range live.Leaves {
		if _, ok := rec.Leaves[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		r, inRec := rec.Leaves[p]
		l, inLive := live.Leaves[p]
		ref := r
		if !inRec {
			ref = l
		}
		excused, typed := known.Excused(ref)
		switch {
		case !inRec || !inLive:
			if excused && !typed {
				continue
			}
			if !inRec {
				d.Structural = p + " is not in the recording"
			} else {
				d.Structural = p + " is missing"
			}
			return d
		case l.Type != r.Type:
			if excused && !typed {
				continue
			}
			d.Structural = p + " changed type"
			return d
		case l.Value == r.Value || excused:
			continue
		case r.Unnamed:
			d.Structural = p + " drifted, and no path names it alone"
			return d
		}
		d.Drift = append(d.Drift, p)
	}
	d.Reordered = reorderedArrays(rec.Body, live.Body, known)
	return d
}

// fixedDiffers names the first fixed part (in sorted order) the two requests
// do not share, or "".
func fixedDiffers(live, rec map[string]string) string {
	names := make([]string, 0, len(rec)+len(live))
	for n := range rec {
		names = append(names, n)
	}
	for n := range live {
		if _, ok := rec[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		lv, inLive := live[n]
		rv, inRec := rec[n]
		if inLive != inRec || lv != rv {
			return n
		}
	}
	return ""
}

// Outcome is what L decided about a request.
type Outcome int

const (
	// Miss: no recorded mock answers the request (Result.Reason says why).
	Miss Outcome = iota
	// Exact: a candidate matches modulo what is known (step 1).
	Exact
	// Paired: a candidate differs only by values L may learn, and it is the
	// unique closest one (steps 4 and 5).
	Paired
)

// Candidate is one recorded request L may serve.
type Candidate struct {
	Shape Shape
	Known Known
	// Pairable: the candidate may be paired, not only matched exactly. Only
	// an unconsumed data-plane mock of the request's scope is (design §7.2
	// step 2), and a pool mock only in a full record (step 10).
	Pairable bool
	// Spent: one of the test's own mocks that answered a request of the test
	// already (design §5.2: each answers one). It is neither served nor
	// paired, and it still competes: a request it matches exactly, or that
	// it is the closest to, is one call more than recorded, a miss, never
	// paired with a farther recording.
	Spent bool
}

// Result is L's decision about one request.
type Result struct {
	Outcome Outcome
	// Index is the candidate served (Exact, Paired), or, for a miss, the one
	// L refused on order or the closest one, -1 when there is none.
	Index int
	// Diff is that candidate's.
	Diff Diff
	// Reason, for a miss: models.MissRequestChanged, MissUnstableOrder or
	// MissUnmockedCall; empty for one keploy could not judge because its
	// own store could not read a recorded answer back (keploy's failure,
	// not the request's).
	Reason string
	// Why, for a miss, is what differed, by path; it never holds a value.
	Why string
}

// Pair decides one request against its candidates (design §7.2 steps 1-5),
// which come in the order their scope offers them: the test's mocks in
// recorded order, then the pool.
//
//  1. The first candidate that matches exactly, modulo what is known, is
//     served, unless it is spent; a request only a spent candidate matches
//     exactly is one call more than recorded: a miss with no reason of the
//     learn step's.
//  2. The others with the request's operation key are compared.
//  3. Order first: an array the order rule refuses is not a drift.
//  4. A pairable candidate differs only by values, in learnable leaves, with
//     the same types.
//  5. The candidate whose drift set is the unique minimum is paired: the set
//     of fewest drifted leaves, which no other candidate's set of that size
//     differs from. Every candidate with the key and no difference in more
//     than values competes, pairable or not: a closer recording L may not
//     learn onto (another test's, or the pool's in a partial record) is
//     still the closest, and the request is then a miss, never paired with
//     a farther one. Candidates that share the minimum set (a call recorded
//     several times, each time with another value in the same place) are
//     told apart by nothing the request carries, and the first pairable
//     one, in recorded order, is taken: a test's mocks are consumed in
//     recorded order. Otherwise the request is a miss: request_changed when
//     some candidate had its key, unmocked_call when none did. When the
//     closest candidate is one the order rule refused and nothing else
//     drifted, it is unstable_order, and nothing is learned from that
//     array.
func Pair(live Shape, cands []Candidate) Result {
	type scored struct {
		i        int
		d        Diff
		drift    []string // outside the reordered arrays
		pairable bool
	}
	var pairable []scored
	keyed, closest := false, Result{Outcome: Miss, Index: -1}
	spentExact := -1
	for i, c := range cands {
		if c.Shape.Key != live.Key {
			continue
		}
		keyed = true
		d := Compare(live, c.Shape, c.Known)
		if d.Structural == "" && d.count() == 0 && len(d.Reordered) == 0 {
			if !c.Spent {
				return Result{Outcome: Exact, Index: i, Diff: d}
			}
			if spentExact < 0 {
				spentExact = i
			}
			continue
		}
		if d.Structural != "" {
			if c.Pairable && closest.Index < 0 {
				closest.Index, closest.Diff = i, d
			}
			continue
		}
		pairable = append(pairable, scored{i: i, d: d, drift: d.outside(), pairable: c.Pairable && !c.Spent})
	}
	if spentExact >= 0 {
		return Result{Outcome: Miss, Index: spentExact, Why: "it is the call a recorded call of the test answered already: one call more than the test recorded"}
	}
	if !keyed {
		return Result{Outcome: Miss, Index: -1, Reason: models.MissUnmockedCall, Why: "no recorded call has its method, destination and shape"}
	}
	if len(pairable) == 0 {
		closest.Reason = models.MissRequestChanged
		closest.Why = "it differs from every recorded call in more than values"
		if closest.Index >= 0 {
			closest.Why = "it differs from the closest recorded call in more than values: " + closest.Diff.Structural
		}
		return closest
	}
	best := pairable[0]
	var tied []scored
	for _, s := range pairable[1:] {
		switch {
		case len(s.drift) < len(best.drift):
			best, tied = s, nil
		case len(s.drift) == len(best.drift):
			tied = append(tied, s)
		}
	}
	for _, t := range tied {
		if !sameStrings(t.drift, best.drift) || !sameStrings(t.d.Reordered, best.d.Reordered) {
			return Result{Outcome: Miss, Index: best.i, Diff: best.d, Reason: models.MissRequestChanged,
				Why: fmt.Sprintf("%d recorded calls differ from it as much, in different places, so none can be told apart as its recording", len(tied)+1)}
		}
	}
	// Among recordings that drift alike, the first one L may learn onto.
	if !best.pairable {
		for _, t := range tied {
			if t.pairable {
				best = t
				break
			}
		}
	}
	if len(best.d.Reordered) > 0 {
		if len(best.drift) > 0 {
			return Result{Outcome: Miss, Index: best.i, Diff: best.d, Reason: models.MissRequestChanged,
				Why: strings.Join(best.d.Reordered, ", ") + " has the recorded elements in another order, and " + strings.Join(best.drift, ", ") + " changed too"}
		}
		return Result{Outcome: Miss, Index: best.i, Diff: best.d, Reason: models.MissUnstableOrder,
			Why: strings.Join(best.d.Reordered, ", ") + " has the recorded elements in another order"}
	}
	if !best.pairable && cands[best.i].Spent {
		return Result{Outcome: Miss, Index: best.i, Diff: best.d,
			Why: "its closest recorded call answered a request of the test already, so it is one call more than the test recorded: " + strings.Join(best.drift, ", ") + " changed"}
	}
	if !best.pairable {
		return Result{Outcome: Miss, Index: best.i, Diff: best.d, Reason: models.MissRequestChanged,
			Why: "its closest recorded call is not one the learn step may learn onto (another test's, or the pool's outside a full record): " + strings.Join(best.drift, ", ") + " changed"}
	}
	return Result{Outcome: Paired, Index: best.i, Diff: best.d}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

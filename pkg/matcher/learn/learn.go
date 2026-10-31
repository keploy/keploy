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
	d, _ := compare(live, rec, known, false)
	return d
}

// compare is Compare, which stops at the first place live differs from rec
// in more than values (Diff.Structural names it). With all it reads on past
// it, and places are then every such place, as a miss names them: each
// field on one side only once, however many leaves and fixed parts it holds
// (fieldPlace, within), each change of type, each fixed part not shared
// that no such field holds, and each drift no path names alone; first the
// fixed parts, then the leaves by path. The shadow of a field on one side
// only in a body's keys (a Keys leaf) is a place only when nothing else is.
// Drift is then every drifted leaf, and Reordered is left empty (the order
// rule reads only a request that differs in values alone). places is empty
// exactly when Structural is "". Without all, places holds the first place
// at most.
func compare(live, rec Shape, known Known, all bool) (d Diff, places []string) {
	if known == nil {
		known = None
	}
	structural := func(why string) bool {
		if d.Structural == "" {
			d.Structural = why
		}
		places = append(places, why)
		return !all
	}
	fixed := fixedDiffering(live.Fixed, rec.Fixed, all)
	if !all && len(fixed) > 0 {
		structural(fixed[0] + " differs")
		return d, places
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
	// With all: the fields on one side only, by path, each with the place
	// that names it; each side's containers (fieldPlace); a body's keys
	// that drifted.
	var fields []string
	var fieldWhy map[string]string
	var liveHas, recHas map[string]bool
	var keys []string
	var leafPlaces []string
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
			why, lacking, lacks := " is missing", live.Leaves, &liveHas
			if !inRec {
				why, lacking, lacks = " is not in the recording", rec.Leaves, &recHas
			}
			if !all {
				structural(p + why)
				return d, places
			}
			if d.Structural == "" {
				d.Structural = p + why
			}
			f := p
			if !ref.Unnamed {
				if *lacks == nil {
					*lacks = containersOf(lacking)
				}
				f = fieldPlace(p, *lacks)
			}
			if fieldWhy == nil {
				fieldWhy = map[string]string{}
			}
			if _, seen := fieldWhy[f+why]; !seen {
				fieldWhy[f+why] = f + why
				fields = append(fields, f)
				leafPlaces = append(leafPlaces, f+why)
			}
			continue
		case l.Type != r.Type:
			if excused && !typed {
				continue
			}
			if !all {
				structural(p + " changed type")
				return d, places
			}
			if d.Structural == "" {
				d.Structural = p + " changed type"
			}
			leafPlaces = append(leafPlaces, p+" changed type")
			continue
		case l.Value == r.Value || excused:
			continue
		case r.Unnamed:
			why := p + " drifted, and no path names it alone"
			if !all {
				structural(why)
				return d, places
			}
			if d.Structural == "" {
				d.Structural = why
			}
			if r.Type == Keys {
				keys = append(keys, why)
			} else {
				leafPlaces = append(leafPlaces, why)
			}
			continue
		}
		d.Drift = append(d.Drift, p)
	}
	for _, name := range fixed {
		if !withinAny(name, fields) {
			places = append(places, name+" differs")
		}
	}
	if len(fixed) > 0 {
		d.Structural = fixed[0] + " differs"
	}
	places = append(places, leafPlaces...)
	if len(places) == 0 {
		places = keys
	}
	if d.Structural == "" {
		d.Reordered = reorderedArrays(rec.Body, live.Body, known)
	}
	return d, places
}

// containersOf is every path that holds a leaf of leaves: a leaf's path cut
// where an object key or an array position begins ("a", "a.b" and "a.b[0]"
// of "a.b[0].c"; "a" and "a.0" of "a.0.c").
func containersOf(leaves map[string]Leaf) map[string]bool {
	out := make(map[string]bool, len(leaves))
	for p := range leaves {
		for i := 1; i < len(p); i++ {
			if p[i] == '.' || p[i] == '[' {
				out[p[:i]] = true
			}
		}
	}
	return out
}

// fieldPlace is where a leaf the other side lacks is named: the outermost
// path holding it that holds nothing on the other side (has: containersOf
// the other side's leaves), so that a field on one side only is one place
// however many leaves it holds, and a value that is a document on one side
// and a scalar on the other is one place each way; the leaf's own path when
// the other side holds leaves under every path above it.
func fieldPlace(p string, has map[string]bool) string {
	for i := 1; i < len(p); i++ {
		if (p[i] == '.' || p[i] == '[') && !has[p[:i]] {
			return p[:i]
		}
	}
	return p
}

// withinAny reports whether the fixed part name is one of a field of
// fields: named after a path in it (the field's path, then "'s", '.' or
// '['), as an adapter names what the leaves of a path do not tell (a field
// holding an array, a dotted key).
func withinAny(name string, fields []string) bool {
	for _, f := range fields {
		if len(name) > len(f) && strings.HasPrefix(name, f) && strings.ContainsRune("'.[", rune(name[len(f)])) {
			return true
		}
	}
	return false
}

// fixedDiffering names the fixed parts the two requests do not share, in
// sorted order (the first is the one a Diff names); without all, the first
// one alone.
func fixedDiffering(live, rec map[string]string, all bool) []string {
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
	var out []string
	for _, n := range names {
		lv, inLive := live[n]
		rv, inRec := rec[n]
		if inLive != inRec || lv != rv {
			out = append(out, n)
			if !all {
				break
			}
		}
	}
	return out
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
//     some candidate had its key, unmocked_call when none did. A request
//     that differs from every pairable candidate in more than values names
//     the one closest to it (the fewest places that differ in more than
//     values, as compare counts them, then the fewest drifted values, then
//     recorded order), and those places. When the
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
	// far are the pairable candidates that differ in more than values, by
	// their place in cands, with their Diff.
	type farCand struct {
		i int
		d Diff
	}
	var far []farCand
	keyed := false
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
			if c.Pairable {
				far = append(far, farCand{i: i, d: d})
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
		closest := Result{Outcome: Miss, Index: -1, Reason: models.MissRequestChanged,
			Why: "it differs from every recorded call in more than values"}
		// The closest is the one it differs from in the fewest places in
		// more than values, then in the fewest drifted values, the first in
		// recorded order of those as close. Never merely the first: a
		// recording of another operation sharing the key (another event
		// written to the same collection) may come before the request's
		// own, and its first difference then names a field the request's
		// own recording never differed in.
		best, bestDrift := -1, 0
		var bestPlaces []string
		for _, f := range far {
			d, places := compare(live, cands[f.i].Shape, cands[f.i].Known, true)
			if n := len(places); closest.Index < 0 || n < best || (n == best && len(d.Drift) < bestDrift) {
				closest.Index, closest.Diff, best, bestDrift, bestPlaces = f.i, f.d, n, len(d.Drift), places
			}
		}
		if closest.Index >= 0 {
			closest.Why = "it differs from the closest recorded call in more than values: " + bestPlaces[0] + alsoPlaces(bestPlaces)
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

// maxAlso is how many places past the first a miss names.
const maxAlso = 3

// alsoPlaces is what a miss says of the places past the first that its
// request differs from its closest recording in more than values (places,
// as compare names them): up to maxAlso of them by name, then how many
// more.
func alsoPlaces(places []string) string {
	if len(places) < 2 {
		return ""
	}
	rest := places[1:]
	named := rest
	if len(named) > maxAlso {
		named = named[:maxAlso]
	}
	out := "; also " + strings.Join(named, "; ")
	switch more := len(rest) - len(named); {
	case more == 1:
		out += "; and 1 other place"
	case more > 1:
		out += fmt.Sprintf("; and %d other places", more)
	}
	return out
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

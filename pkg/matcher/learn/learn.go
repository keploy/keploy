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
	"slices"
	"sort"
	"strconv"
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
	// Kinds name, by path, the kind of a value its leaves' paths do not
	// tell, in a miss's words ("an array", "a 64-bit integer"): a protocol
	// that joins an array's elements with '.', as a document's fields, or
	// walks a typed scalar as the document it is encoded as. A path its
	// leaves are not under is never named. Kinds only name: a miss says a
	// value at such a path changed kind as one place (missPlaces); what
	// pairs is decided by the leaves and fixed parts alone. nil when the
	// leaves tell every kind.
	Kinds map[string]string
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

// Compare says how live differs from rec, excusing what known excuses. It
// stops at the first place they differ in more than values, which
// Diff.Structural names; missPlaces reads on past it.
func Compare(live, rec Shape, known Known) Diff {
	if known == nil {
		known = None
	}
	var d Diff
	if names := fixedDiffering(live.Fixed, rec.Fixed, false); len(names) > 0 {
		d.Structural = names[0] + " differs"
		return d
	}
	for _, p := range leafPaths(live, rec) {
		switch c, _, _ := leafChange(p, live, rec, known); c {
		case leafSame:
			continue
		case leafOnlyLive:
			d.Structural = p + " is not in the recording"
		case leafOnlyRec:
			d.Structural = p + " is missing"
		case leafRetyped:
			d.Structural = p + " changed type"
		case leafUnnamedDrift:
			d.Structural = p + " drifted, and no path names it alone"
		case leafDrifted:
			d.Drift = append(d.Drift, p)
			continue
		}
		return d
	}
	d.Reordered = reorderedArrays(rec.Body, live.Body, known)
	return d
}

// leafPaths are the paths of both shapes' leaves, sorted, each once.
func leafPaths(live, rec Shape) []string {
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
	return paths
}

// leafDiff is how one leaf differs, as Compare and missPlaces read it alike.
type leafDiff byte

const (
	leafSame         leafDiff = iota // equal, or excused
	leafDrifted                      // its value drifted
	leafOnlyLive                     // not in the recording
	leafOnlyRec                      // missing from the request
	leafRetyped                      // its type changed
	leafUnnamedDrift                 // drifted where no path names it alone
)

// leafChange is how the leaf at p differs between live and rec, excusing
// what known excuses: an untyped excuse lets it drift, change type or be
// missing from either side; a typed one lets it drift only.
func leafChange(p string, live, rec Shape, known Known) (c leafDiff, l, r Leaf) {
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
			return leafSame, l, r
		}
		if !inRec {
			return leafOnlyLive, l, r
		}
		return leafOnlyRec, l, r
	case l.Type != r.Type:
		if excused && !typed {
			return leafSame, l, r
		}
		return leafRetyped, l, r
	case l.Value == r.Value || excused:
		return leafSame, l, r
	case r.Unnamed:
		return leafUnnamedDrift, l, r
	}
	return leafDrifted, l, r
}

// missPlaces is how live differs from rec read whole, for a miss to name:
// places are every place they differ in more than values, as the leaves
// and fixed parts Compare reads show them, each named once:
//   - a field on one side only, at the outermost path holding it that the
//     other side holds nothing at ("body.a is missing"), however many
//     leaves and fixed parts it holds;
//   - a value whose kind changed, at the outermost path holding it whose
//     kind differs (kindsOf), worded as the change ("body.a was a
//     document, is an array"), however its leaves and fixed parts show it:
//     a scalar that became a document, a document that became an array or
//     empty, a number that became a string, one typed scalar that became
//     another (as the adapter's Kinds name them);
//   - a fixed part not shared that no such place holds, by its name, or,
//     named after a path whose kind differs as the adapter's Kinds name
//     it, as that change (a Mongo field holding an array one side, a
//     document the other);
//   - a drift no path names alone.
//
// First the fixed parts, then the leaves by path. The shadow of a field on
// one side only in a body's keys (a Keys leaf) is a place only when nothing
// else is. Diff.Structural is the first place, Drift every drifted leaf, and
// Reordered empty (the order rule reads only a request that differs in
// values alone). places is empty exactly when Compare's Structural is "":
// both read each leaf and fixed part alike (leafChange, fixedDiffering),
// and each leaf or fixed part that differs is a place or inside one.
func missPlaces(live, rec Shape, known Known) (d Diff, places []string) {
	if known == nil {
		known = None
	}
	recKinds, liveKinds := kindsOf(rec), kindsOf(live)
	// held are the paths the field and kind places are at: a fixed part
	// named after one is theirs. said is every place named, once.
	var held, keys []string
	said := map[string]bool{}
	say := func(at, why string) {
		held = append(held, at)
		if !said[why] {
			said[why] = true
			places = append(places, why)
		}
	}
	var leafPlaces []string
	for _, p := range leafPaths(live, rec) {
		c, l, r := leafChange(p, live, rec, known)
		switch c {
		case leafSame:
			continue
		case leafDrifted:
			d.Drift = append(d.Drift, p)
			continue
		case leafUnnamedDrift:
			why := p + " drifted, and no path names it alone"
			if r.Type == Keys {
				keys = append(keys, why)
			} else {
				say(p, why)
			}
			continue
		}
		at, why, ok := placeOf(p, c, recKinds, liveKinds)
		if !ok {
			// Both sides hold every path down to p alike (Kinds name p
			// alike, or a path's kind is not known): p's own change.
			at, why = p, p+" is not in the recording"
			switch c {
			case leafOnlyRec:
				why = p + " is missing"
			case leafRetyped:
				why = p + " was " + kindNames[r.Type] + ", is " + kindNames[l.Type]
			}
		}
		say(at, why)
	}
	leafPlaces, places = places, nil
	var kindPaths []string
	for _, name := range fixedDiffering(live.Fixed, rec.Fixed, true) {
		if withinAny(name, held) {
			continue
		}
		why := name + " differs"
		if kindPaths == nil {
			kindPaths = kindsNamed(rec, live)
		}
		if at, kindWhy, ok := kindChangeHolding(name, kindPaths, recKinds, liveKinds); ok {
			// The other fixed parts named after at are this place too.
			held, why = append(held, at), kindWhy
		}
		places = append(places, why)
	}
	places = append(places, leafPlaces...)
	if len(places) == 0 {
		places = keys
	}
	if len(places) > 0 {
		d.Structural = places[0]
	}
	return d, places
}

// kindNames name a leaf's type as a miss says what a value was and is.
var kindNames = map[Type]string{
	String: "a string", Number: "a number", Bool: "a boolean", Null: "null",
	EmptyObj: "an empty document", EmptyArray: "an empty array", Keys: "keys", Instant: "an instant",
}

// docKind and arrayKind are the kinds of a path that holds leaves.
const (
	docKind   = "a document"
	arrayKind = "an array"
)

// bodyRoot is the path a Shape's decoded Body is at (JSONLeaves' root).
const bodyRoot = "body"

// kindsOf is the kind of every path of s that holds a value: what s.Kinds
// names there; else, in a decoded Body, the kind of the value at each of its
// paths, spelled as JSONLeaves spells them (so a key holding '.', '[' or ']'
// is one step, and only its own path is held); else a leaf's, by its type
// (kindNames); else, for a path other leaves are under, a document or an
// array as their paths go on from it ('.' or '['; a document when both
// do), held with no kind ("") when only unnamed leaves are under it.
func kindsOf(s Shape) kinds {
	out := make(map[string]string, 2*len(s.Leaves))
	var parent map[string]string
	inBody := func(p string) bool {
		return s.Body != nil && (p == bodyRoot || strings.HasPrefix(p, bodyRoot+".") || strings.HasPrefix(p, bodyRoot+"["))
	}
	for p, l := range s.Leaves {
		if inBody(p) {
			continue
		}
		for i := 1; i < len(p); i++ {
			switch {
			case p[i] != '.' && p[i] != '[':
			case l.Unnamed:
				if _, ok := out[p[:i]]; !ok {
					out[p[:i]] = ""
				}
			case p[i] == '.':
				out[p[:i]] = docKind
			case out[p[:i]] != docKind:
				out[p[:i]] = arrayKind
			}
		}
	}
	for p, l := range s.Leaves {
		if !inBody(p) {
			out[p] = kindNames[l.Type]
		}
	}
	if s.Body != nil {
		parent = map[string]string{}
		bodyKinds(bodyRoot, "", s.Body, out, parent)
	}
	for p, k := range s.Kinds {
		out[p] = k
	}
	return kinds{of: out, parent: parent}
}

// kinds are one side's kindsOf: the kind at each path it holds (of), and,
// for a decoded Body's paths, the path each is directly in (parent).
type kinds struct {
	of, parent map[string]string
}

// pathsTo are the paths holding the leaf at p, outermost first, p last:
// in a decoded Body, the paths it is in, step by step; elsewhere p cut
// where a key or a position begins.
func (k kinds) pathsTo(p string) []string {
	if _, ok := k.parent[p]; ok {
		out := []string{p}
		for q := k.parent[p]; q != ""; q = k.parent[q] {
			out = append(out, q)
		}
		slices.Reverse(out)
		return out
	}
	var out []string
	for i := 1; i < len(p); i++ {
		if p[i] == '.' || p[i] == '[' {
			out = append(out, p[:i])
		}
	}
	return append(out, p)
}

// bodyKinds adds the kind of the decoded JSON value v at path (in the path
// up), and of every value in it, to out, and the path each is in to parent.
// Two values one path spells (a key "a.b" beside a holding b) of other kinds
// are held with no kind (""), whichever is read first.
func bodyKinds(path, up string, v interface{}, out, parent map[string]string) {
	if prev, ok := parent[path]; ok && prev < up {
		// Two values spell path: it is in the outer one's path, whichever
		// is read first.
		up = prev
	}
	parent[path] = up
	k := ""
	switch t := v.(type) {
	case map[string]interface{}:
		k = kindNames[EmptyObj]
		if len(t) > 0 {
			k = docKind
		}
		for key, child := range t {
			bodyKinds(path+"."+key, path, child, out, parent)
		}
	case []interface{}:
		k = kindNames[EmptyArray]
		if len(t) > 0 {
			k = arrayKind
		}
		for i, child := range t {
			bodyKinds(path+"["+strconv.Itoa(i)+"]", path, child, out, parent)
		}
	default:
		typ, _ := scalar(t)
		k = kindNames[typ]
	}
	if prev, ok := out[path]; ok && prev != k {
		k = ""
	}
	out[path] = k
}

// placeOf is where a leaf at p that is on one side only, or changed type
// (c), is named, and how (rec and live: kindsOf each side): at the
// outermost path holding it (pathsTo, on its own side), p included, that
// the other side holds nothing at (a field on one side only) or holds as
// another kind (a kind changed). A path held with no kind (one only
// unnamed leaves are under, or two values of other kinds spell), p
// included, is passed over: nothing says what it is. ok is false when both
// sides hold every path down to p alike, or one with no kind.
func placeOf(p string, c leafDiff, rec, live kinds) (at, why string, ok bool) {
	own := rec
	if c == leafOnlyLive {
		own = live
	}
	for _, q := range own.pathsTo(p) {
		rk, inRec := rec.of[q]
		lk, inLive := live.of[q]
		switch {
		case (inRec && rk == "") || (inLive && lk == ""):
			continue
		case !inRec:
			return q, q + " is not in the recording", true
		case !inLive:
			return q, q + " is missing", true
		case rk != lk:
			return q, q + " was " + rk + ", is " + lk, true
		}
	}
	return "", "", false
}

// kindsNamed are the paths either shape's Kinds name, sorted.
func kindsNamed(rec, live Shape) []string {
	paths := make([]string, 0, len(rec.Kinds)+len(live.Kinds))
	for p := range rec.Kinds {
		paths = append(paths, p)
	}
	for p := range live.Kinds {
		if _, dup := rec.Kinds[p]; !dup {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// kindChangeHolding is the kind change a fixed part not shared is named as
// (name: the fixed part's name): at the outermost of paths (kindsNamed)
// that the name is named after (within) and that both sides hold as
// different kinds.
func kindChangeHolding(name string, paths []string, recKinds, liveKinds kinds) (at, why string, ok bool) {
	for _, q := range paths {
		rk, inRec := recKinds.of[q]
		lk, inLive := liveKinds.of[q]
		if inRec && inLive && rk != "" && lk != "" && rk != lk && within(name, q) {
			return q, q + " was " + rk + ", is " + lk, true
		}
	}
	return "", "", false
}

// withinAny reports whether name is named after one of paths (within).
func withinAny(name string, paths []string) bool {
	for _, f := range paths {
		if within(name, f) {
			return true
		}
	}
	return false
}

// within reports whether name is named after the path f: f itself (a
// body's media type, named "body"), or f then "'s", '.' or '[' (a fixed
// part an adapter names after a path, as what the leaves of the path do
// not tell: a field holding an array, a dotted key; a path inside it).
func within(name, f string) bool {
	return name == f || len(name) > len(f) && strings.HasPrefix(name, f) && strings.ContainsRune("'.[", rune(name[len(f)]))
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
//     values, as missPlaces counts them, then the fewest drifted values,
//     then recorded order), and those places; its Diff is that whole
//     reading (missPlaces), Structural its first place. When the
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
	// their place in cands, with Compare's Diff.
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
			// A spent candidate is never paired (Candidate.Spent), so it is
			// never the closest a miss in more than values names either,
			// whatever Pairable says.
			if c.Pairable && !c.Spent {
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
			d, places := missPlaces(live, cands[f.i].Shape, cands[f.i].Known)
			if len(places) == 0 {
				// missPlaces reads each leaf and fixed part as Compare
				// does, so it finds the place Compare stopped at; should
				// the two ever part, the miss still says that place.
				d.Structural, places = f.d.Structural, []string{f.d.Structural}
			}
			if n := len(places); closest.Index < 0 || n < best || (n == best && len(d.Drift) < bestDrift) {
				closest.Index, closest.Diff, best, bestDrift, bestPlaces = f.i, d, n, len(d.Drift), places
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
// as missPlaces names them): up to maxAlso of them by name, then how many
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

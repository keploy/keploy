package matcher

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// A noise path is dot-joined object keys, index-free at every depth, matched
// against a traversal key by strings.Contains.
//
// That vocabulary is defined by the JSON walker and nowhere else: the
// map[string]interface{} branch of matchJSONWithNoiseHandlingIndexed extends the
// key (prefix = key + "."), while BOTH []interface{} branches recurse with the
// key unchanged. So every element of `items` is walked under the key "items",
// and an array position is a segment the walker never emits.
//
// Being implicit, that vocabulary was re-derived by every producer, and the one
// that fed it back into a test case got it wrong: the cloud auto-noise
// write-back serialised an RFC-6901 JSON Pointer (which keeps array positions
// and ~0/~1 escapes). A path in the wrong dialect does not fail — it silently
// matches nothing, so the field it names goes on being compared while the
// recording reads as though it were suppressed.
//
// Two more dialects exist and are deliberately left alone, because they feed
// display rather than matching: risk.go's field diffs use a "[]" suffix and
// report rendering uses "$.a[0].b". Reconciling those is a separate change; the
// hazard is only that someone copies one of them into a noise path.
//
// This file makes the vocabulary an API. BodyNoiseFromJSONDiff is the supported
// way to turn a response-body difference into noise paths, and
// UnmatchableBodyNoise is the supported way to ask whether an existing path can
// ever match.
//
// Deriving a path is only half the job. Because entries are matched by substring
// — and because a single-segment entry is a GLOBAL key matched against a field
// name at every depth — the shortest path naming a drifting field routinely
// covers fields that did not drift. `items.id` is a substring of
// `items.idempotency_key`; a root-level `requestId` also silences
// `user.requestId`. Emitting those would delete assertions, which is the same
// class of harm as the bug this file fixes, pointing the other way. So every
// candidate is checked against the document for exactly that, and one that
// cannot be expressed without collateral is refused rather than emitted.
//
// That check is only as good as the document it reads. Fields the walker cannot
// address separately — array elements, and keys differing only in case or
// containing a literal dot — are treated as one, so a candidate covering any of
// them is refused unless every one of them drifted. Erring toward refusal is
// deliberate: a refused case stays red with a reason, an over-broad entry goes
// green while quietly asserting less.

// NoiseSkipReason explains why a difference must not be turned into a noise
// path. Auto-noise that silences anything other than the volatile value it was
// derived from is a correctness bug — it deletes an assertion rather than
// tolerating nondeterminism — so these are deliberate refusals, not failures.
// Callers are expected to surface the reason and leave the case failing.
type NoiseSkipReason string

const (
	// NoiseSkipStructural — a field appeared or disappeared, or a value changed
	// between a scalar and a container. Suppressing that hides a schema change.
	NoiseSkipStructural NoiseSkipReason = "STRUCTURAL"
	// NoiseSkipArrayLength — the array changed length. The only entries that
	// excuse a length change are ones covering the array itself or an ancestor,
	// and either blindfolds every field beneath it as well.
	NoiseSkipArrayLength NoiseSkipReason = "ARRAY_LENGTH"
	// NoiseSkipRootValue — the whole document is a scalar that drifted. The only
	// expressible entry is the bare-section skip sentinel, which switches off
	// every body assertion on the test case.
	NoiseSkipRootValue NoiseSkipReason = "ROOT_VALUE"
	// NoiseSkipUnrepresentable — a JSON key contains ".", so once joined the path
	// cannot be told apart from nesting and would name a different field.
	NoiseSkipUnrepresentable NoiseSkipReason = "UNREPRESENTABLE"
	// NoiseSkipOverBroad — the path naming this field also covers a field that
	// did not drift, because entries match by substring and a single-segment
	// entry matches a field name at any depth. Emitting it would stop asserting
	// something that is still being asserted today.
	NoiseSkipOverBroad NoiseSkipReason = "OVER_BROAD"
	// NoiseSkipOrderAmbiguous — ignoreOrdering is on and this array's elements
	// cannot be paired, so which element drifted is unknowable and any path
	// derived from a positional pairing may name a field that never moved.
	NoiseSkipOrderAmbiguous NoiseSkipReason = "ORDER_AMBIGUOUS"
	// NoiseSkipUnresolved — the derived entries do not make the matcher accept the
	// response. Reported by asking the matcher itself rather than by reasoning
	// about it, so a difference can never be reported as handled when it is not.
	NoiseSkipUnresolved NoiseSkipReason = "UNRESOLVED"
	// NoiseSkipInvalidJSON — one of the bodies is not JSON.
	NoiseSkipInvalidJSON NoiseSkipReason = "INVALID_JSON"
)

// NoiseSkip is one difference that must not be auto-noised. Path is the best
// available identifier for it and is empty for a root-level difference.
type NoiseSkip struct {
	Path   string
	Reason NoiseSkipReason
}

// BodyNoiseFromJSONDiff returns the noise paths covering the differences between
// a recorded and a replayed response body, in the JSON walker's own key
// vocabulary. Paths are root-relative — the caller adds the "body." section
// prefix — and preserve the document's letter case for readability, since
// matching is case-insensitive everywhere downstream.
//
// A path is emitted only when it is a scalar VALUE drift AND the path covers
// nothing else in the document. Everything else is refused with a
// NoiseSkipReason explaining which assertion emitting it would have deleted.
//
// known is the noise the test case already carries, root-relative and lowercased
// as SplitNoise returns it, including any regex patterns. Differences it already
// excuses are not re-emitted, so successive auto-noise rounds converge instead of
// appending forever; a pattern-guarded entry only excuses the values its pattern
// actually matches, exactly as the matcher treats it.
//
// ignoreOrdering must be the value the replay will use. With it set the matcher
// pairs array elements greedily rather than by position, so a pure reordering is
// not a difference at all and a genuine one cannot be attributed to a particular
// element.
//
// Both return values are sorted and deduplicated: this output is persisted to a
// test case file, and Go map order would rewrite the file on every run.
func BodyNoiseFromJSONDiff(expJSON, actJSON string, known map[string][]string, ignoreOrdering bool) (paths []string, skipped []NoiseSkip) {
	// An absent body is not malformed JSON. UnmarshallJSON maps "" to nil for
	// the same reason: a 204 that starts returning a body is a structural
	// change, and reporting it as a parse error reads as a bug in the parser.
	expEmpty, actEmpty := strings.TrimSpace(expJSON) == "", strings.TrimSpace(actJSON) == ""
	switch {
	case expEmpty && actEmpty:
		return nil, nil
	case expEmpty || actEmpty:
		return nil, []NoiseSkip{{Reason: NoiseSkipStructural}}
	}

	var exp, act interface{}
	if err := json.Unmarshal([]byte(expJSON), &exp); err != nil {
		return nil, []NoiseSkip{{Reason: NoiseSkipInvalidJSON}}
	}
	if err := json.Unmarshal([]byte(actJSON), &act); err != nil {
		return nil, []NoiseSkip{{Reason: NoiseSkipInvalidJSON}}
	}

	w := &noiseCandidateWalk{
		seen:            map[string]bool{},
		skipSeen:        map[NoiseSkip]bool{},
		drifted:         map[string]bool{},
		ignoreOrdering:  ignoreOrdering,
		knownGlobalRegs: map[string][]*regexp.Regexp{},
	}
	// Mirror JSONDiffWithNoiseControl's split: a key with no dot is a GLOBAL key
	// matched exactly against a field name at any depth, and only a dotted key
	// goes into the substring-matched path index. Treating the two alike here
	// would misjudge what the test case already covers.
	dotted := map[string][]string{}
	for k, v := range known {
		if strings.Contains(k, ".") {
			dotted[k] = v
			continue
		}
		w.knownGlobalRegs[strings.ToLower(k)] = compilePatterns(v, nil)
		w.hasKnownGlobal = true
	}
	w.knownPath = buildNoiseIndex(dotted, nil)

	w.walk("", "", exp, act, false, false)

	// Nothing may be emitted until the document says it is safe. A candidate is
	// the SHORTEST path naming the field that drifted, and the shortest path is
	// exactly the one most likely to also cover a neighbour.
	//
	// Both documents are indexed: a field that exists only in the replay is still
	// something an entry can silence, so it counts as collateral.
	cov := newCoverageIndex(exp, act)
	for _, p := range w.paths {
		if _, ok := cov.coversOnly(p, w.drifted, w.alreadySilenced); !ok {
			w.skip(p, NoiseSkipOverBroad)
			continue
		}
		paths = append(paths, p)
	}

	sort.Strings(paths)

	// Ground truth. Everything above reasons ABOUT the matcher's noise semantics —
	// which entry excuses a removed key, when a pattern on a container relaxes its
	// descendants, when an array's pattern applies per element. Every one of those
	// rules is a place this walk can drift from the walker, and a drift that
	// under-reports is a false green: the caller writes the derived entries, sees
	// no refusal, and marks a case resolved that fails again on the next replay.
	//
	// So the last word belongs to the matcher itself. If it does not accept the
	// response with the derived entries in place, something is unresolved,
	// whatever this walk concluded. That cannot drift, because it is not a model
	// of the matcher — it is the matcher.
	// Only when nothing else already explains it: a specific reason is more
	// actionable than this one, and every refusal above is by definition still a
	// difference, so reporting both would just double every entry.
	if len(w.skipped) == 0 && !w.acceptedByMatcher(exp, act, known, paths, ignoreOrdering) {
		w.skip("", NoiseSkipUnresolved)
	}

	sort.Slice(w.skipped, func(i, j int) bool {
		if w.skipped[i].Path != w.skipped[j].Path {
			return w.skipped[i].Path < w.skipped[j].Path
		}
		return w.skipped[i].Reason < w.skipped[j].Reason
	})
	return paths, w.skipped
}

type noiseCandidateWalk struct {
	knownPath       noiseIndex
	knownGlobalRegs map[string][]*regexp.Regexp
	hasKnownGlobal  bool
	ignoreOrdering  bool

	seen     map[string]bool
	skipSeen map[NoiseSkip]bool
	paths    []string
	skipped  []NoiseSkip
	// drifted holds the NODE IDENTITY of every scalar difference found, including
	// ones already covered or later refused. Coverage is judged against all of
	// them: a path that also covers another field that genuinely drifted is not
	// collateral damage.
	//
	// The identity is the field names joined by NUL, not the traversal key, and it
	// keeps their original case. Two things would otherwise alias and let a drift
	// on one vouch for a sibling that never moved: keys differing only in case
	// ("K" and "k"), and a key containing a literal dot ({"a.b":{"c":…}} shares the
	// traversal key "a.b.c" with {"a":{"b":{"c":…}}}). Array elements DO alias, by
	// design — the walker cannot address them separately either.
	drifted map[string]bool
}

// alreadySilenced reports whether the test case's existing noise unconditionally
// ignores this key already. Such a field is not collateral: a new entry that also
// covers it takes away nothing that is still being asserted.
func (w *noiseCandidateWalk) alreadySilenced(lowerKey string, ancestryLower []string) bool {
	if regs, noisy := w.knownPath.match(lowerKey); noisy && len(regs) == 0 {
		return true
	}
	for _, f := range ancestryLower {
		if regs, ok := w.knownGlobalRegs[f]; ok && len(regs) == 0 {
			return true
		}
	}
	return false
}

// knownCoversLeaf reports whether the test case's existing noise already excuses
// this leaf, applying patterns the way noisyForValue does — an entry with
// patterns only excuses the values those patterns match, so treating it as an
// unconditional skip would make the producer return nothing for a live drift.
func (w *noiseCandidateWalk) knownCoversLeaf(key string, actual interface{}) bool {
	value, isScalar := jsonScalarToString(actual)
	if regs, noisy := w.knownPath.match(strings.ToLower(key)); noisy {
		if len(regs) == 0 {
			return true
		}
		if isScalar && anyRegexpMatchStr(value, regs) {
			return true
		}
	}
	if !w.hasKnownGlobal {
		return false
	}
	// A global entry is keyed on the field name, i.e. the last segment.
	field := key
	if i := strings.LastIndex(key, "."); i >= 0 {
		field = key[i+1:]
	}
	regs, isGlobal := w.knownGlobalRegs[strings.ToLower(field)]
	if !isGlobal {
		return false
	}
	if len(regs) == 0 {
		return true
	}
	return isScalar && anyRegexpMatchStr(value, regs)
}

// knownCoversSubtree mirrors the walker's container early-return: only an entry
// with NO patterns ignores a whole subtree, structure included. A pattern-guarded
// entry on a container does not stop the walk — it relaxes the descendant values
// instead, which is what ancestorNoisy carries.
func (w *noiseCandidateWalk) knownCoversSubtree(key string) bool {
	if key == "" {
		return false
	}
	regs, noisy := w.knownPath.match(strings.ToLower(key))
	return noisy && len(regs) == 0
}

// knownMatches reports whether any existing entry addresses this key at all,
// with or without patterns. On a container that is what sets the walker's
// ancestorNoisy, after which every descendant value is accepted unconditionally.
func (w *noiseCandidateWalk) knownMatches(key string) bool {
	if key == "" {
		return false
	}
	_, noisy := w.knownPath.match(strings.ToLower(key))
	return noisy
}

// walk mirrors matchJSONWithNoiseHandlingIndexed node for node, which is what
// makes the emitted paths keys the matcher actually produces.
//
// ambiguous is carried down once any ancestor key contained a ".": every path
// beneath it is unrepresentable, because the joined result can no longer be
// parsed back to the field it came from.
//
// ancestorNoisy mirrors the walker's own flag. Once an ancestor container matches
// a noise entry — WITH or without patterns — every descendant value is accepted
// unconditionally, because the leaf test short-circuits on it before the pattern
// is ever consulted. Re-evaluating the pattern at the leaf instead would have the
// producer append a permanent unconditional entry on top of every pattern-guarded
// container entry the user wrote, on every round.
func (w *noiseCandidateWalk) walk(key, id string, exp, act interface{}, ambiguous, ancestorNoisy bool) {
	if w.knownCoversSubtree(key) {
		return
	}

	switch e := exp.(type) {
	case map[string]interface{}:
		a, ok := act.(map[string]interface{})
		if !ok {
			w.skip(key, NoiseSkipStructural)
			return
		}
		// Only a CONTAINER raises the flag, exactly as the walker does. A leaf
		// evaluates its own entry through knownCoversLeaf, so that a
		// pattern-guarded entry there still only excuses the values it describes.
		ancestorNoisy = ancestorNoisy || w.knownMatches(key)
		for k, ev := range e {
			child := childNoisePath(key, k)
			av, present := a[k]
			if !present {
				// A key the replay dropped. The walker excuses that only for an
				// entry that ignores the field unconditionally — a pattern has no
				// value left to match against — and it does NOT consult
				// ancestorNoisy here, so neither may this.
				if w.knownCoversSubtree(child) || w.globalUnconditional(k) {
					continue
				}
				w.skip(child, NoiseSkipStructural)
				continue
			}
			// A global entry on a key that IS present skips the child subtree
			// wholesale, structure included — and with containersCovered=true, so
			// even a pattern-guarded one does. Walking into it anyway invented
			// refusals for differences the matcher already excuses, and derived
			// redundant entries for values it had already stopped comparing.
			if w.globalCoversSubtree(k, av) {
				continue
			}
			w.walk(child, childNodeID(id, k), ev, av, ambiguous || strings.Contains(k, "."), ancestorNoisy)
		}
		for k, av := range a {
			if _, present := e[k]; present {
				continue
			}
			// A key the replay added. Here a pattern CAN excuse it, because there
			// is a replayed value for it to describe — via a global entry or a
			// path entry, both passed to noisyForRegexps in utils.go's added-key
			// branch. That branch does not consult ancestorNoisy either.
			child := childNoisePath(key, k)
			if w.globalCoversValue(k, av) || w.pathCoversValue(child, av) {
				continue
			}
			w.skip(child, NoiseSkipStructural)
		}

	case []interface{}:
		a, ok := act.([]interface{})
		if !ok {
			w.skip(key, NoiseSkipStructural)
			return
		}
		if len(e) != len(a) {
			w.skip(key, NoiseSkipArrayLength)
			return
		}
		// A pattern on an array of SCALARS is unambiguous — the matcher applies it
		// per element and keeps comparing them — so only a heterogeneous or nested
		// array relaxes its values wholesale. Raising the flag unconditionally
		// here would treat every element as already covered.
		if w.knownMatches(key) && !allJSONScalars(e) {
			ancestorNoisy = true
		}
		// Under ignoreOrdering the matcher pairs elements greedily, so a positional
		// walk would attribute a drift to whichever element sits at that index.
		//
		// That only matters when the elements have INTERNAL structure. Every
		// element of an array shares the array's own traversal key, so for an array
		// of scalars the derived path is that key however the elements pair —
		// refusing there would make enabling ignoreOrdering strictly worse and
		// leave an ordinary volatile list red forever.
		if w.ignoreOrdering && len(e) > 1 && !(allJSONScalars(e) && allJSONScalars(a)) {
			if sameElementMultiset(e, a) {
				return
			}
			w.skip(key, NoiseSkipOrderAmbiguous)
			return
		}
		// The walker recurses into elements with the key UNCHANGED, so every
		// element contributes to the same path. This is precisely what makes an
		// index-free path the only spelling that can ever match.
		for i := range e {
			w.walk(key, id, e[i], a[i], ambiguous, ancestorNoisy)
		}

	default:
		if reflect.DeepEqual(exp, act) {
			return
		}
		// What the walker can be told to ignore, and nothing more.
		//
		// A scalar turning into a container (or the reverse) only yields to an
		// unconditional entry on the key, which blindfolds the whole subtree.
		// A scalar changing TYPE — "5" becoming 5 — is rejected with
		// "type not matched" before noise is consulted at all, so no entry
		// suppresses it either.
		//
		// null turning into a value is the exception and must not be lumped in
		// with them: the walker's nil branch deliberately consults the noise
		// entry against the replayed value, because a nullable volatile field is
		// an ordinary thing to want suppressed.
		_, expScalar := jsonScalarToString(exp)
		_, actScalar := jsonScalarToString(act)
		if !expScalar || !actScalar {
			w.skip(key, NoiseSkipStructural)
			return
		}
		if exp != nil && reflect.TypeOf(exp) != reflect.TypeOf(act) {
			w.skip(key, NoiseSkipStructural)
			return
		}
		// Record the drift before any decision: coverage is judged against every
		// field that genuinely moved, whether or not it ends up emitted.
		if key != "" {
			w.drifted[id] = true
		}
		if ancestorNoisy || w.knownCoversLeaf(key, act) {
			return
		}
		switch {
		case key == "" && id == "":
			w.skip("", NoiseSkipRootValue)
		case key == "":
			// A JSON key that is literally "" — the joined path is the empty
			// string, which addresses nothing. Not a root-value drift: reporting
			// it as one would advise switching off the whole body assertion over
			// a single field.
			w.skip("", NoiseSkipUnrepresentable)
		case ambiguous:
			w.skip(key, NoiseSkipUnrepresentable)
		default:
			w.emit(key)
		}
	}
}

// globalUnconditional reports whether a global entry ignores this field name
// outright — the only form that also excuses the field being absent.
func (w *noiseCandidateWalk) globalUnconditional(field string) bool {
	if !w.hasKnownGlobal {
		return false
	}
	regs, ok := w.knownGlobalRegs[strings.ToLower(field)]
	return ok && len(regs) == 0
}

// acceptedByMatcher runs the real body comparison with the test case's existing
// noise plus the paths just derived, and reports whether the matcher is satisfied.
//
// The derived paths are added unconditionally (no patterns), which is exactly how
// the caller will persist them, so this asks the same question the next replay
// will. An error from the comparison counts as not accepted: the next replay
// would fail on it too.
func (w *noiseCandidateWalk) acceptedByMatcher(exp, act interface{}, known map[string][]string, derived []string, ignoreOrdering bool) bool {
	effective := make(map[string][]string, len(known)+len(derived))
	for k, v := range known {
		effective[k] = v
	}
	for _, p := range derived {
		// Never displace an entry the test case already carries. The write path
		// dedupes against the existing key rather than replacing it — precisely so
		// auto-noise cannot widen a pattern its author narrowed on purpose — so
		// replacing one here would model a persist that never happens and vouch
		// for a difference that is still live. A nil value is the one exception,
		// and not an exception at all: SplitNoise yields nil for an unconditional
		// entry, which is what the derived one would say anyway.
		if lk := strings.ToLower(p); effective[lk] == nil {
			effective[lk] = []string{}
		}
	}
	res, err := JSONDiffWithNoiseControl(ValidatedJSON{expected: exp, actual: act}, effective, ignoreOrdering, nil)
	return err == nil && res.IsExact()
}

// pathCoversValue reports whether a path entry excuses a field the replay added.
// Mirrors utils.go's added-key branch, which passes the replayed value to
// noisyForRegexps so a pattern can describe it.
func (w *noiseCandidateWalk) pathCoversValue(key string, value interface{}) bool {
	regs, noisy := w.knownPath.match(strings.ToLower(key))
	if !noisy {
		return false
	}
	return noisyForRegexps(regs, value, false)
}

// globalCoversSubtree reports whether a global entry covers a key that is present
// in both documents. Mirrors utils.go's present-key branch, which passes
// containersCovered=true: a pattern that cannot describe a container still covers
// it, because the entry named the field and there is no single value to test.
func (w *noiseCandidateWalk) globalCoversSubtree(field string, value interface{}) bool {
	if !w.hasKnownGlobal {
		return false
	}
	regs, ok := w.knownGlobalRegs[strings.ToLower(field)]
	if !ok {
		return false
	}
	return noisyForRegexps(regs, value, true)
}

// globalCoversValue reports whether a global entry excuses a field the replay
// added, which a pattern can do because there is a value for it to describe.
func (w *noiseCandidateWalk) globalCoversValue(field string, value interface{}) bool {
	if !w.hasKnownGlobal {
		return false
	}
	regs, ok := w.knownGlobalRegs[strings.ToLower(field)]
	if !ok {
		return false
	}
	return noisyForRegexps(regs, value, false)
}

func (w *noiseCandidateWalk) emit(path string) {
	if w.seen[path] {
		return
	}
	w.seen[path] = true
	w.paths = append(w.paths, path)
}

func (w *noiseCandidateWalk) skip(path string, reason NoiseSkipReason) {
	s := NoiseSkip{Path: path, Reason: reason}
	if w.skipSeen[s] {
		return
	}
	w.skipSeen[s] = true
	w.skipped = append(w.skipped, s)
}

func childNoisePath(key, field string) string {
	if key == "" {
		return field
	}
	return key + "." + field
}

// sameElementMultiset reports whether two arrays hold the same elements in some
// order, which under ignoreOrdering means they do not differ at all. Go marshals
// map keys in sorted order, so the canonical form is stable. An element that will
// not marshal reports false, which routes the array to a refusal rather than to a
// pairing that cannot be justified.
func sameElementMultiset(e, a []interface{}) bool {
	if len(e) != len(a) {
		return false
	}
	canon := func(vals []interface{}) []string {
		out := make([]string, 0, len(vals))
		for _, v := range vals {
			b, err := json.Marshal(v)
			if err != nil {
				return nil
			}
			out = append(out, string(b))
		}
		sort.Strings(out)
		return out
	}
	ce, ca := canon(e), canon(a)
	if ce == nil || ca == nil {
		return false
	}
	for i := range ce {
		if ce[i] != ca[i] {
			return false
		}
	}
	return true
}

// coverageEntry is one addressable node of the document: the key the walker would
// use for it, plus the field names on the path that reaches it so a global entry
// can be evaluated too.
type coverageEntry struct {
	lowerKey      string
	ancestryLower []string
}

// childNodeID extends a node identity. NUL cannot occur in a Go string literal
// read from JSON, so joining on it keeps {"a.b":{"c":…}} distinct from
// {"a":{"b":{"c":…}}} even though both are addressed as "a.b.c".
func childNodeID(id, field string) string {
	// Always joined, even at the root, so the empty identity means "the document
	// itself" and nothing else — that is what tells a drifting root scalar apart
	// from a drift under a key that is literally "".
	return id + "\x00" + field
}

// coverageIndex answers "what else would this entry silence?" against the real
// documents, which is the only place that question can be answered: whether a
// numeric segment is an array position or an object key, and whether a short path
// brushes a neighbour, are both facts about the document rather than the string.
type coverageIndex struct {
	// nodes is keyed by NODE IDENTITY (see noiseCandidateWalk.drifted), so two
	// distinct fields that share a traversal key stay distinct here. Containers
	// are included as well as leaves: an empty object or array contributes no
	// leaf, and a candidate that is a prefix of its key silences the whole
	// subtree — including the structural assertion that it stayed empty.
	nodes map[string]coverageEntry
}

func newCoverageIndex(docs ...interface{}) *coverageIndex {
	c := &coverageIndex{nodes: map[string]coverageEntry{}}
	for _, d := range docs {
		c.collect(d, "", "", nil)
	}
	return c
}

func (c *coverageIndex) collect(node interface{}, key, id string, ancestry []string) {
	if key != "" {
		if _, ok := c.nodes[id]; !ok {
			c.nodes[id] = coverageEntry{lowerKey: strings.ToLower(key), ancestryLower: ancestry}
		}
	}
	switch v := node.(type) {
	case map[string]interface{}:
		for k, child := range v {
			c.collect(child, childNoisePath(key, k), childNodeID(id, k),
				append(append([]string(nil), ancestry...), strings.ToLower(k)))
		}
	case []interface{}:
		// Key AND identity unchanged, matching both slice branches of the walker:
		// elements are genuinely indistinguishable to it.
		for _, child := range v {
			c.collect(child, key, id, ancestry)
		}
	}
}

// coversOnly reports whether an entry for path silences nothing beyond fields
// that actually drifted or that the existing noise already ignores, and names the
// first offender when it does not.
func (c *coverageIndex) coversOnly(path string, drifted map[string]bool, silenced func(lowerKey string, ancestryLower []string) bool) (collateral string, ok bool) {
	lp := strings.ToLower(path)
	global := !strings.Contains(lp, ".")
	for nodeID, e := range c.nodes {
		covered := false
		if global {
			// A global entry skips the whole subtree under any field with that
			// name, so every node beneath such a field is silenced.
			for _, f := range e.ancestryLower {
				if f == lp {
					covered = true
					break
				}
			}
		} else {
			covered = strings.Contains(e.lowerKey, lp)
		}
		if !covered || drifted[nodeID] {
			continue
		}
		if silenced != nil && silenced(e.lowerKey, e.ancestryLower) {
			continue
		}
		return e.lowerKey, false
	}
	return "", true
}

// UnmatchableBodyNoise returns the body-noise keys that cannot match anything in
// the given documents — the exact answer to "why did my noise entry do nothing".
//
// It reports dead entries whatever made them dead: an array position, an
// undecoded ~0/~1 escape, a typo, or a field that no longer exists. It does so
// by asking the document rather than the string, which is what lets it leave a
// genuinely numeric object key such as data.2026.count alone.
//
// bodyNoise is root-relative; keys are compared case-insensitively because the
// matcher lowercases both sides, and a caller that merged raw config noise into
// SplitNoise's output would otherwise have live entries reported as dead.
//
// Pass every body the entry could legitimately describe — the recorded one and
// the replayed one — since an entry may name a field only one of them carries.
// Returned keys are in the caller's original spelling, sorted.
//
// If ANY body passed is absent or will not parse, nothing is reported at all.
// Callers prune on this result, so a body that cannot be read must not be taken
// as evidence that the fields it would have carried are gone.
func UnmatchableBodyNoise(bodyNoise map[string][]string, bodies ...string) []string {
	if len(bodyNoise) == 0 {
		return nil
	}
	var (
		paths     = map[string]bool{} // full traversal keys, for dotted entries
		fields    = map[string]bool{} // bare field names, for dot-free global entries
		parsedAny bool
	)
	for _, b := range bodies {
		if strings.TrimSpace(b) == "" {
			// A body that is not there describes nothing, and an entry may name a
			// field only the missing one carried. Callers PRUNE on this result, so
			// the absence of evidence must not be read as evidence of absence.
			return nil
		}
		var doc interface{}
		if err := json.Unmarshal([]byte(b), &doc); err != nil {
			// Likewise for a body that will not parse: one malformed replay would
			// otherwise strip a whole suite's noise.
			return nil
		}
		parsedAny = true
		collectTraversalKeys(doc, "", paths, fields)
	}
	if !parsedAny {
		return nil
	}

	var dead []string
	for k := range bodyNoise {
		lk := strings.ToLower(k)
		if lk == "" || lk == "*" {
			// "*" is the documented ignore-everything wildcard, not a field.
			continue
		}
		if !strings.Contains(lk, ".") {
			// A dot-free key is global: an exact field-name lookup at every
			// depth, not a substring test.
			if !fields[lk] {
				dead = append(dead, k)
			}
			continue
		}
		alive := false
		for p := range paths {
			// noiseIndex.match's test, verbatim: the noise key is the needle and
			// the traversal key is the haystack.
			if strings.Contains(p, lk) {
				alive = true
				break
			}
		}
		if !alive {
			dead = append(dead, k)
		}
	}
	sort.Strings(dead)
	return dead
}

// warnedUnmatchable dedupes the diagnostic below. It runs per failing test case,
// and again for a case that is retried, so warning unconditionally would emit a
// line per attempt per case and bury the actual failures. The
// key is scoped per test case, because "which case did this come from" is the
// first thing the reader needs and a global key answers it for only one of them.
//
// Per-case keys mean the map grows with the suite, and this package is linked
// into a long-lived replay server that runs many suites, so it is bounded: past
// the cap the whole cache is dropped rather than grown. Re-warning about an
// entry seen long ago is the harmless direction; retaining every key a server
// ever saw is not.
//
// The counter is approximate under concurrency — a Clear racing an Add can drop
// a few counts — which only shifts when the cache is dropped, never whether a
// first warning is emitted.
var (
	warnedUnmatchable      sync.Map
	warnedUnmatchableCount atomic.Int64
)

// maxWarnedUnmatchable caps the dedup cache. Comfortably above any single
// suite's (case, entry) count, so within one run the dedup always holds.
const maxWarnedUnmatchable = 8192

// WarnUnmatchableBodyNoise reports body-noise entries that cannot match anything
// in the bodies involved in a failure. This is the diagnostic for the worst
// failure mode a noise config has: an entry that reads as an assertion being
// suppressed while suppressing nothing, so the case goes red naming a field the
// user believes they already excluded.
//
// Call it only on a JSON body failure. It is exact rather than heuristic — it
// asks the document, so it catches an array position, an undecoded ~0/~1 escape
// and a plain typo alike, and it stays quiet about a genuinely numeric object
// key.
func WarnUnmatchableBodyNoise(logger *zap.Logger, testCase string, bodyNoise map[string][]string, bodies ...string) {
	if logger == nil {
		return
	}
	for _, k := range UnmatchableBodyNoise(bodyNoise, bodies...) {
		if _, seen := warnedUnmatchable.LoadOrStore(testCase+"\x00"+k, struct{}{}); seen {
			continue
		}
		if warnedUnmatchableCount.Add(1) > maxWarnedUnmatchable {
			warnedUnmatchable.Clear()
			warnedUnmatchableCount.Store(0)
		}
		logger.Warn("body noise entry matches nothing in this response, so the field it names is still being compared",
			zap.String("testCase", testCase),
			zap.String("entry", k),
			zap.String("hint", "array positions are not part of a noise path: the walker keys every element of `items` under `items`, so write items.product.stock, not items.0.product.stock"))
	}
}

// collectTraversalKeys records every key the JSON walker would consult for this
// document, lowercased. Container keys are included because a noise entry naming
// a container legitimately silences its whole subtree.
func collectTraversalKeys(doc interface{}, key string, paths, fields map[string]bool) {
	if key != "" {
		paths[strings.ToLower(key)] = true
	}
	switch v := doc.(type) {
	case map[string]interface{}:
		for k, child := range v {
			fields[strings.ToLower(k)] = true
			collectTraversalKeys(child, childNoisePath(key, k), paths, fields)
		}
	case []interface{}:
		// Key unchanged, matching both slice branches of the walker.
		for _, child := range v {
			collectTraversalKeys(child, key, paths, fields)
		}
	}
}

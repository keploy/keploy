package matcher

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
)

type pathMaps struct {
	types  map[string]string // path -> type ("string","number","boolean","null")
	values map[string]string // path -> scalar stringified value (only for primitives)
	// coveredTypes and coveredValues hold, as types and values do, the
	// scalars a pattern-guarded noise entry matched. They are set aside, not
	// dropped: a pattern excuses a field only when the recorded and the live
	// side each match it (bothSides), which one side cannot tell alone. Nil
	// until a pattern matches.
	coveredTypes  map[string]string
	coveredValues map[string]string
}

func newPathMaps() pathMaps {
	return pathMaps{types: map[string]string{}, values: map[string]string{}}
}

// coveredOnly reports whether every scalar m holds at path is one a
// pattern-guarded noise entry matched.
func (m *pathMaps) coveredOnly(path string) bool {
	if _, plain := m.types[path]; plain {
		return false
	}
	_, covered := m.coveredTypes[path]
	return covered
}

// collectBoth collects the recorded (exp) and the live (act) document under
// one noise index and applies bothSides to them.
func collectBoth(exp, act interface{}, ni noiseIndex) (expMaps, actMaps pathMaps) {
	expMaps, actMaps = newPathMaps(), newPathMaps()
	collectJSON(exp, "", ni, &expMaps)
	collectJSON(act, "", ni, &actMaps)
	bothSides(&expMaps, &actMaps)
	return expMaps, actMaps
}

// bothSides decides the scalars a pattern-guarded noise entry matched on
// either side. A pattern says the field may vary within it, so it excuses a
// path only when every scalar both sides hold there matches it. Anywhere else
// the matched scalars are compared like any other: the recorded id 1 under
// {"id": ["^1$"]} against a live id 2 is a changed value, not a recorded id
// hidden and a live one added, and a matched field the other side lacks is
// missing from it. A scalar that did not match keeps its place before one
// that did, as before.
func bothSides(exp, act *pathMaps) {
	// A path is restored on a side that holds only matched scalars there when
	// the other side does not, so restoring exp first cannot change what act
	// decides: act was not matched-only at any path exp restores.
	restore := func(m, other *pathMaps) {
		for p, typ := range m.coveredTypes {
			if _, plain := m.types[p]; plain || other.coveredOnly(p) {
				continue
			}
			m.types[p] = typ
			if v, ok := m.coveredValues[p]; ok {
				m.values[p] = v
			}
		}
	}
	restore(exp, act)
	restore(act, exp)
}

func ComputeFailureAssessmentJSON(expJSON, actJSON string, bodyNoise map[string][]string, ignoreOrdering bool) (*models.FailureAssessment, error) {
	// Quick exit if either side isn't JSON
	if !json.Valid([]byte(expJSON)) || !json.Valid([]byte(actJSON)) {
		return nil, nil
	}

	var exp, act interface{}
	if err := json.Unmarshal([]byte(expJSON), &exp); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(actJSON), &act); err != nil {
		return nil, err
	}

	idx := buildNoiseIndex(bodyNoise, nil) // already in matcher/utils.go

	// collectBoth, reading first where the live side holds only values a
	// pattern matched: the reasons below name the other side there.
	expMaps, actMaps := newPathMaps(), newPathMaps()
	collectJSON(exp, "", idx, &expMaps)
	collectJSON(act, "", idx, &actMaps)
	liveIn := map[string]bool{}
	for p := range actMaps.coveredTypes {
		liveIn[p] = actMaps.coveredOnly(p)
	}
	bothSides(&expMaps, &actMaps)

	added, removed, typeChanges, valueChanges := diffMaps(expMaps, actMaps)

	assess := &models.FailureAssessment{
		AddedFields:   added,
		RemovedFields: removed,
		TypeChanges:   typeChanges,
		ValueChanges:  valueChanges,
	}

	hasAdded := len(added) > 0
	hasRemoved := len(removed) > 0
	hasType := len(typeChanges) > 0
	hasValue := len(valueChanges) > 0

	// The path maps above keep one value per path, an array's elements
	// folded into one "[]" path (the last element's value wins), so they
	// cannot see a value, a field or an element that changed inside an array
	// while another element still holds it, and they read a reordered array
	// as changed values. "Only new fields were added" is decided by
	// containment instead (JSONContainsWithNoiseControl, with the noise and
	// the ordering the comparison applied): every recorded value is still
	// where it was, element for element, and the answer differs only by
	// members it added. keploy test's additive-schema auto-pass trusts this
	// grade (Low), so it passes nothing else.
	contains, decided, _ := JSONContainsWithNoiseControl(NewValidatedJSON(exp, act), bodyNoise, ignoreOrdering, nil)
	samePaths := !hasAdded && !hasRemoved && !hasType && !hasValue
	if decided && contains && !samePaths && !hasRemoved && !hasType {
		// The values the path maps call changed are how they fold arrays
		// (a reordered array): every recorded value is still there. A field
		// they read as removed or retyped keeps its HIGH grade below, as on
		// main, so neither the auto-pass nor an auto-replay's noise learning
		// (which skips HIGH) treats it as new fields only.
		assess.ValueChanges = nil
		assess.Reasons = []string{"Backward-compatible: only new fields added."}
		assess.Category = []models.FailureCategory{models.SchemaAdded}
		assess.Risk = models.Low
		return assess, nil
	}
	notContained := ""
	switch {
	case !decided:
		notContained = "Not graded as only new fields: the response's arrays hold too many elements to pair one by one, so it was not verified that nothing but new fields changed."
	case !contains:
		notContained = "An array no longer holds what it held, element for element: a value, a field or an element of it changed."
	}

	// Build reasons (human friendly)
	if hasRemoved {
		assess.Reasons = append(assess.Reasons, "Removed fields: "+strings.Join(removed, ", "))
	}
	if hasType {
		assess.Reasons = append(assess.Reasons, "Type changes at: "+strings.Join(typeChanges, ", "))
	}
	if hasAdded && !hasRemoved && !hasType && hasValue {
		assess.Reasons = append(assess.Reasons, "Backward-compatible (with value differences): new fields plus value changes on existing fields.")
	}
	if !hasAdded && !hasRemoved && !hasType && hasValue {
		assess.Reasons = append(assess.Reasons, "Schema identical; only values changed.")
	}
	if samePaths && notContained == "" {
		assess.Reasons = append(assess.Reasons, "Schema and values are identical.")
	}
	if notContained != "" && (!decided || (!hasRemoved && !hasType && !hasValue)) {
		// What the path maps do not show, or that the arrays went unpaired.
		assess.Reasons = append(assess.Reasons, notContained)
	}

	// Categorize per spec:
	// - Schema Changes (removed/type change) -> High
	// - Schema Same (value-only)             -> Medium
	// - Schema Addition:
	//     * only new fields                  -> Low (contained, above)
	//     * new + some value changes        -> Medium
	// - No differences at all               -> SchemaUnchanged / None
	switch {
	case hasRemoved || hasType:
		assess.Category = []models.FailureCategory{models.SchemaBroken}
		assess.Risk = models.High

	case samePaths && notContained == "":
		assess.Category = []models.FailureCategory{models.SchemaUnchanged}
		assess.Risk = models.None

	case !hasAdded:
		// Values changed: by the path maps, or inside an array they fold.
		assess.Category = []models.FailureCategory{models.SchemaUnchanged}
		assess.Risk = models.Medium

	default:
		// New fields, and values changed besides: by the path maps, or
		// inside an array they fold.
		assess.Category = []models.FailureCategory{models.SchemaAdded}
		assess.Risk = models.Medium
	}

	// A field keploy.yml's noise lets vary only within patterns is compared
	// when a side's value is outside them: name the field, the patterns and
	// that side, as mock matching does.
	changed := slices.Concat(removed, typeChanges, valueChanges, added)
	assess.Reasons = append(assess.Reasons, bodyPatternWhys(idx, bodyNoise, expMaps, actMaps, liveIn, changed)...)

	return assess, nil
}

// ChangedJSONFieldPaths returns the set of dot-delimited field paths that
// differ between two JSON bodies — value changes, type changes, and removed
// fields (pure additions are NOT returned, since a brand-new field is not
// "noise" the way a drifting value is). Array elements normalize to a "[]"
// suffix (collectJSON's convention), so per-index churn collapses to a single
// path. `known` is the already-flagged noise (path -> regex list): any path
// matched by it is excluded from the diff so re-runs don't re-report it.
// Returns nil if either side isn't valid JSON. Paths are root-relative (no
// "body." prefix) — callers add the prefix to match the testcase convention.
// `typeStrict` names learned-dynamic paths: their VALUE is ignored but their
// TYPE must still match, so a value change on them is tolerated while a type
// change (or removal) is reported. A path is resolved under the same
// most-specific-first ordering as `known`, so a more-specific `known` entry wins
// over a less-specific type-strict one.
// excludeRecordedValue, when non-nil, is consulted with the recorded
// (expected) scalar value of each changed path — returning true drops that
// path. This lets the proxy exclude fields the enterprise obfuscator already
// redacted (recorded value matches a Mock.Noise regex) so secret fields are
// not re-flagged as schema noise. (Note: for a type-strict path the recorded
// value is not captured, so excludeRecordedValue cannot apply to it.)
// valueChangesOnly, when true, returns only fields whose VALUE drifted (same
// type) — not type changes or removed fields. The auto-noising LEARN pass uses
// it so a field that changed type (or vanished) is a real difference, never
// learned as ignorable noise; enforcement passes false to see every drift.
func ChangedJSONFieldPaths(expJSON, actJSON string, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, excludeRecordedValue func(string) bool) []string {
	if !json.Valid([]byte(expJSON)) || !json.Valid([]byte(actJSON)) {
		return nil
	}

	var exp, act interface{}
	if err := json.Unmarshal([]byte(expJSON), &exp); err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(actJSON), &act); err != nil {
		return nil
	}
	return ChangedDecodedJSONFieldPaths(exp, act, known, typeStrict, valueChangesOnly, excludeRecordedValue)
}

// ChangedDecodedJSONFieldPaths is ChangedJSONFieldPaths over two bodies
// already decoded, each by json.Unmarshal into an interface{}: a body
// compared with many others is decoded once. It only reads exp and act.
func ChangedDecodedJSONFieldPaths(exp, act interface{}, known map[string][]string, typeStrict map[string]struct{}, valueChangesOnly bool, excludeRecordedValue func(string) bool) []string {
	idx := buildNoiseIndex(known, nil)
	// typeStrict paths are LEARNED-dynamic: their VALUE is ignored but their TYPE
	// must match. Marked on the index so they resolve under the same
	// most-specific-first ordering as user/regex noise — a more-specific user entry
	// still wins and keeps its own semantics. Empty => behaviour is unchanged.
	idx.markTypeStrict(typeStrict)

	expMaps, actMaps := collectBoth(exp, act, idx)

	_, removed, typeChanges, valueChanges := diffMaps(expMaps, actMaps)

	// live is the live document read alone, each scalar a pattern-guarded
	// entry matched set aside: whether the live value at a path is inside
	// its patterns, which bothSides no longer tells. Read on first need.
	live := sync.OnceValue(func() pathMaps {
		m := newPathMaps()
		collectJSON(act, "", idx, &m)
		return m
	})
	// heldRecorded is the paths where the recorded document, read alone,
	// holds a scalar outside a pattern-guarded entry's patterns that the
	// obfuscator did not redact: wherever it stands in an array, which the
	// path maps, holding one value per path, do not tell. Read on first
	// need.
	heldRecorded := sync.OnceValue(func() map[string]bool {
		out := map[string]bool{}
		eachJSONScalar(exp, "", func(path string, v interface{}) {
			regs, _, _ := idx.matchKind(strings.ToLower(path))
			str, _ := jsonScalarToString(v)
			if _, val := scalarTypeValue(v); !anyRegexpMatchStr(str, regs) && !excludeRecordedValue(val) {
				out[path] = true
			}
		})
		return out
	})
	keep := func(path string) bool {
		if excludeRecordedValue == nil {
			return true
		}
		v, ok := expMaps.values[path]
		if !ok || !excludeRecordedValue(v) {
			return true
		}
		// The obfuscator redacted the recorded value, so it is not
		// compared. Where an entry with patterns guards the path, the live
		// value alone is held to them, and so is every recorded value
		// there it did not redact: one outside them, or none, is a change.
		if regs, _, _ := idx.matchKind(strings.ToLower(path)); len(regs) == 0 {
			return false
		}
		if heldRecorded()[path] {
			return true
		}
		m := live()
		return !m.coveredOnly(path)
	}

	groups := [][]string{valueChanges, typeChanges, removed}
	if valueChangesOnly {
		groups = [][]string{valueChanges}
	}
	out := make([]string, 0, len(valueChanges)+len(typeChanges)+len(removed))
	for _, group := range groups {
		for _, p := range group {
			if keep(p) {
				out = append(out, p)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// JSONFieldDiffs returns field-level diffs between two JSON documents with
// recorded/live values attached, for mock-mismatch reporting. It walks both
// documents with the same traversal as ComputeFailureAssessmentJSON (arrays
// normalize to a "[]" path suffix) and skips paths covered by `known` noise
// (path -> regex list, root-relative). `pathPrefix` (e.g. "body.") is
// prepended to every returned path so the result lines up with the noise
// config vocabulary. Values are truncated to at most maxVal bytes (cut at a
// rune boundary) to keep reports and yaml output bounded. Returns nil when
// either side isn't valid JSON.
func JSONFieldDiffs(expJSON, actJSON string, known map[string][]string, pathPrefix string, maxVal int) []models.MockFieldDiff {
	if !json.Valid([]byte(expJSON)) || !json.Valid([]byte(actJSON)) {
		return nil
	}

	var exp, act interface{}
	if err := json.Unmarshal([]byte(expJSON), &exp); err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(actJSON), &act); err != nil {
		return nil
	}

	idx := buildNoiseIndex(known, nil)

	expMaps, actMaps := collectBoth(exp, act, idx)

	added, removed, typeChanges, valueChanges := diffMaps(expMaps, actMaps)

	// diffMaps iterates Go maps, so bucket order is randomized per run; sort
	// each bucket so reports, yaml output and the capped diff subset are
	// stable across runs.
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(typeChanges)
	sort.Strings(valueChanges)

	trunc := func(s string) string {
		if maxVal <= 0 || len(s) <= maxVal {
			return s
		}
		// Back the cut off to a rune boundary so a multi-byte UTF-8
		// character is never split — invalid UTF-8 here would garble the
		// report yaml/json encoders downstream.
		cut := maxVal
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		return s[:cut] + "…"
	}

	var out []models.MockFieldDiff
	for _, p := range valueChanges {
		out = append(out, models.MockFieldDiff{
			Path:     pathPrefix + p,
			Kind:     models.DiffKindValueChanged,
			Expected: trunc(expMaps.values[p]),
			Actual:   trunc(actMaps.values[p]),
		})
	}
	for _, p := range typeChanges {
		out = append(out, models.MockFieldDiff{
			Path:     pathPrefix + p,
			Kind:     models.DiffKindTypeChanged,
			Expected: trunc(expMaps.types[p] + ": " + expMaps.values[p]),
			Actual:   trunc(actMaps.types[p] + ": " + actMaps.values[p]),
		})
	}
	for _, p := range removed {
		out = append(out, models.MockFieldDiff{
			Path:     pathPrefix + p,
			Kind:     models.DiffKindMissingInLive,
			Expected: trunc(expMaps.values[p]),
		})
	}
	for _, p := range added {
		out = append(out, models.MockFieldDiff{
			Path:   pathPrefix + p,
			Kind:   models.DiffKindMissingInMock,
			Actual: trunc(actMaps.values[p]),
		})
	}
	return out
}

func collectJSON(v interface{}, path string, ni noiseIndex, out *pathMaps) {
	keyLower := strings.ToLower(path)
	// matchKind resolves the winning (most-specific-first) entry. A type-strict
	// entry records the field's TYPE but not its VALUE — so a type change is
	// reported while a value change is tolerated — but ONLY when it is itself the
	// most specific match; a more-specific user/regex entry still wins and keeps
	// its full-ignore / value-regex semantics. (The learned path is also carried in
	// the known-noise set, so non-JSON parsers, which don't consult the flag, keep
	// fully excluding it.)
	regs, noisy, typeStrict := ni.matchKind(keyLower)
	if noisy && !typeStrict {
		// An entry with no patterns ignores the whole subtree. A pattern-guarded
		// entry ignores only the values it describes, so it must be evaluated
		// against this value — otherwise a field the matcher just reported as a
		// difference would be missing from the report explaining it.
		// An unconditional entry hides the whole subtree. A pattern-guarded one
		// hides only a scalar it actually matches — the matcher keeps walking a
		// guarded container, so dropping it here would report "nothing changed"
		// for a test the matcher just failed.
		// A matched scalar is set aside rather than dropped: whether the
		// pattern excuses it depends on the other side too (bothSides).
		if len(regs) == 0 {
			return
		}
		if str, isScalar := jsonScalarToString(v); isScalar && anyRegexpMatchStr(str, regs) {
			if out.coveredTypes == nil {
				out.coveredTypes, out.coveredValues = map[string]string{}, map[string]string{}
			}
			typ, val := scalarTypeValue(v)
			out.coveredTypes[path], out.coveredValues[path] = typ, val
			return
		}
	}

	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			collectJSON(child, p, ni, out)
		}
	case []interface{}:
		// normalize array paths using [] suffix so we don't depend on indices
		p := path
		if p != "" {
			p += "[]"
		} else {
			p = "[]"
		}
		for _, e := range t {
			collectJSON(e, p, ni, out)
		}
	case string, float64, bool, nil:
		typ, val := scalarTypeValue(t)
		out.types[path] = typ
		if !typeStrict {
			out.values[path] = val
		}
	default:
		// other JSON forms won't appear here
	}
}

// eachJSONScalar calls fn with every scalar of v and its path, as collectJSON
// names it (object keys dot-joined, an array's elements under "[]").
func eachJSONScalar(v interface{}, path string, fn func(path string, v interface{})) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			eachJSONScalar(child, p, fn)
		}
	case []interface{}:
		for _, e := range t {
			eachJSONScalar(e, path+"[]", fn)
		}
	case string, float64, bool, nil:
		fn(path, t)
	}
}

// scalarTypeValue is a JSON scalar's type and value as the path maps hold
// them.
func scalarTypeValue(v interface{}) (typ, val string) {
	switch t := v.(type) {
	case string:
		return "string", t
	case float64:
		return "number", fmt.Sprintf("%v", t)
	case bool:
		return "boolean", strconv.FormatBool(t)
	}
	return "null", "null"
}

func diffMaps(exp, act pathMaps) (added, removed, typeChanges, valueChanges []string) {
	seen := map[string]struct{}{}

	// look at expected keys
	for k, expType := range exp.types {
		seen[k] = struct{}{}
		actType, ok := act.types[k]
		if !ok {
			removed = append(removed, k)
			continue
		}
		if expType != actType {
			typeChanges = append(typeChanges, k)
			continue
		}
		// same type and it's a primitive leaf we tracked
		ev, eok := exp.values[k]
		av, aok := act.values[k]
		// If both present and differ -> value change
		if eok && aok && ev != av {
			valueChanges = append(valueChanges, k)
		}
	}

	// anything new in actual?
	for k := range act.types {
		if _, ok := seen[k]; !ok {
			added = append(added, k)
		}
	}
	return
}

func MaxRisk(a, b models.RiskLevel) models.RiskLevel {
	rank := map[models.RiskLevel]int{
		models.None:   0,
		models.Low:    1,
		models.Medium: 2,
		models.High:   3,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

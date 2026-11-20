package matcher

import (
	"sort"
	"strings"
)

// DecodedJSONFieldsDiffering names, sorted, the fields at which exp, a
// recorded JSON document, and act, a live one, each decoded by
// json.Unmarshal into an interface{}, are not the same document outside
// known noise (root-relative, as ChangedDecodedJSONFieldPaths reads it,
// typeStrict marking its learned entries). Unlike that comparison, which
// reads only the fields the recorded document holds and one value per array
// path, it compares the two both ways and whole: a field only one side
// holds is a difference, whichever side it is, and an array is compared
// element by element in its order, so arrays of other lengths, or with
// other elements anywhere, differ. Outside that it reads noise as
// collectJSON does: an entry with no patterns hides a field and all it
// holds (an array's elements, their count too, when it names them, "ids[]");
// a learned (type-strict) entry compares a scalar's type only; an entry with
// patterns excuses a scalar where each side's value matches one (a recorded
// value excludeRecordedValue reports, which the obfuscator redacted, counts
// as matching), and compares it otherwise; elsewhere a recorded value
// excludeRecordedValue reports is not compared. A field is named by its
// path as collectJSON names it (keys dot-joined, an array's elements under
// "[]"), in the documents' case; the documents themselves, where they are
// not the same kind of value, by "". It only reads exp and act. nil when
// they are the same document outside the noise.
func DecodedJSONFieldsDiffering(exp, act interface{}, known map[string][]string, typeStrict map[string]struct{}, excludeRecordedValue func(string) bool) []string {
	idx := buildNoiseIndex(known, nil)
	idx.markTypeStrict(typeStrict)
	d := jsonDiffering{idx: idx, excluded: excludeRecordedValue}
	d.walk(exp, act, "", true, true)
	return sortedPaths(d.out)
}

// DecodedJSONFieldsOnOneSide names, sorted, the fields of exp and act, two
// JSON documents decoded as DecodedJSONFieldsDiffering reads them, that only
// one of the two holds, whichever it is, read with no noise and named as
// DecodedJSONFieldsDiffering names them: a key of an object both hold at the
// same path, at any depth, the elements of arrays of one length read in
// their order. A field both hold with another value or type, or an array of
// another length, is none. nil when there is none.
func DecodedJSONFieldsOnOneSide(exp, act interface{}) []string {
	var d jsonDiffering
	d.walk(exp, act, "", true, true)
	return sortedPaths(d.oneSide)
}

// sortedPaths is the paths of set, sorted; nil when it holds none.
func sortedPaths(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// jsonDiffering is DecodedJSONFieldsDiffering's walk: the noise it reads and
// the paths it found differing (out), those of a field only one side holds
// among them (oneSide).
type jsonDiffering struct {
	idx      noiseIndex
	excluded func(string) bool
	out      map[string]struct{}
	oneSide  map[string]struct{}
}

func (d *jsonDiffering) differs(path string) {
	if d.out == nil {
		d.out = map[string]struct{}{}
	}
	d.out[path] = struct{}{}
}

// onOneSide notes path as differing, a field only one side holds.
func (d *jsonDiffering) onOneSide(path string) {
	d.differs(path)
	if d.oneSide == nil {
		d.oneSide = map[string]struct{}{}
	}
	d.oneSide[path] = struct{}{}
}

// hides reports whether an entry with no patterns, not a learned one, hides
// path and all it holds.
func (d *jsonDiffering) hides(path string) bool {
	regs, noisy, typeStrict := d.idx.matchKind(strings.ToLower(path))
	return noisy && !typeStrict && len(regs) == 0
}

// walk compares e and a, the values the two documents hold at path (inE,
// inA: whether each holds one there at all).
func (d *jsonDiffering) walk(e, a interface{}, path string, inE, inA bool) {
	if d.hides(path) {
		return
	}
	if !inE || !inA {
		d.onOneSide(path)
		return
	}
	switch et := e.(type) {
	case map[string]interface{}:
		at, ok := a.(map[string]interface{})
		if !ok {
			d.differs(path)
			return
		}
		for k, ev := range et {
			av, in := at[k]
			d.walk(ev, av, childPath(path, k), true, in)
		}
		for k, av := range at {
			if _, in := et[k]; !in {
				d.walk(nil, av, childPath(path, k), false, true)
			}
		}
	case []interface{}:
		at, ok := a.([]interface{})
		if !ok {
			d.differs(path)
			return
		}
		p := path + "[]"
		if d.hides(p) {
			return
		}
		if len(et) != len(at) {
			d.differs(p)
			return
		}
		for i := range et {
			d.walk(et[i], at[i], p, true, true)
		}
	default:
		d.scalar(e, a, path)
	}
}

// scalar compares e, a recorded scalar, with a, the live value at path.
func (d *jsonDiffering) scalar(e, a interface{}, path string) {
	es, _ := jsonScalarToString(e)
	as, isScalar := jsonScalarToString(a)
	if !isScalar {
		d.differs(path)
		return
	}
	et, ev := scalarTypeValue(e)
	at, av := scalarTypeValue(a)
	excluded := d.excluded != nil && d.excluded(ev)
	regs, noisy, typeStrict := d.idx.matchKind(strings.ToLower(path))
	switch {
	case noisy && typeStrict:
		if et != at {
			d.differs(path)
		}
	case noisy:
		// An entry with no patterns hid the field (walk), so this one has
		// some.
		if (excluded || anyRegexpMatchStr(es, regs)) && anyRegexpMatchStr(as, regs) {
			return
		}
		if et != at || ev != av {
			d.differs(path)
		}
	case excluded:
	case et != at || ev != av:
		d.differs(path)
	}
}

// childPath is the path of the field k of the object at path, as
// collectJSON names it.
func childPath(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

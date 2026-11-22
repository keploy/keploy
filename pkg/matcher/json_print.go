package matcher

import (
	"hash/maphash"
	"strings"
)

// JSONPrinter prints decoded JSON documents (Print) over the fields its
// noise does not name, so that two documents DecodedJSONFieldsDiffering
// finds the same, under that noise or any part of it, print the same: two
// prints that differ are a field the comparison compares that differs,
// found without walking the two documents together. A document's print is
// taken once and compared with many.
//
// It leaves out, with all it holds, every field (an array's elements too,
// "ids[]") that an entry of its noise names as the comparison's index names
// a field (matchKind), read by the entry's key alone and as no learned
// entry: whatever the entry's patterns, and by the key anywhere in the
// path, where a learned entry, under the proven switch, names only the
// paths it begins. So it leaves out at least every field the comparison
// hides, excuses by a pattern, or compares by its type alone. Every field
// it keeps it reads as the comparison compares it: an object's keys, in any
// order, and their values; an array's length and its elements in their
// order; a scalar's type and value as scalarTypeValue gives them.
type JSONPrinter struct {
	idx noiseIndex
}

// NewJSONPrinter is the printer that leaves out what an entry of known
// names, each entry read by its key alone.
func NewJSONPrinter(known map[string][]string) JSONPrinter {
	keys := make(map[string][]string, len(known))
	for k := range known {
		keys[k] = nil
	}
	return JSONPrinter{idx: buildNoiseIndex(keys, nil)}
}

// Print is doc's print, doc a JSON document json.Unmarshal decoded into an
// interface{}. decided is false where a scalar it reads holds a value
// excludeRecordedValue reports (one the obfuscator redacted), which the
// comparison does not compare when doc is its recorded document: that
// print says nothing of whether doc is another document. A live document
// is printed with nil.
//
// For any noise whose entries are all among the printer's, any learned
// (typeStrict) entries of it, and any excludeRecordedValue: where
// DecodedJSONFieldsDiffering(exp, act, noise, typeStrict,
// excludeRecordedValue) is nil and Print(exp, excludeRecordedValue) is
// decided, it equals Print(act, nil).
func (p JSONPrinter) Print(doc any, excludeRecordedValue func(string) bool) (print uint64, decided bool) {
	w := jsonPrinting{idx: p.idx, paths: len(p.idx.entries) > 0, excluded: excludeRecordedValue, decided: true}
	if w.leftOut("") {
		return printLeftOut, true
	}
	return w.print(doc, ""), w.decided
}

// The prints of a document's kinds of value, each mixed with what it holds.
const (
	printLeftOut      uint64 = 0x243f6a8885a308d3
	printObject       uint64 = 0x13198a2e03707344
	printArray        uint64 = 0xa4093822299f31d0
	printArrayLeftOut uint64 = 0x082efa98ec4e6c89
	printScalar       uint64 = 0x452821e638d01377
)

// printSeed seeds the prints of keys, types and values: prints are compared
// only within the process that took them.
var printSeed = maphash.MakeSeed()

// jsonPrinting is Print's walk.
type jsonPrinting struct {
	idx noiseIndex
	// paths: the index has entries, so a field's path is read; with none,
	// no field is left out, and no path is built.
	paths    bool
	excluded func(string) bool
	decided  bool
}

// leftOut reports whether an entry names path (matchKind, whatever kind
// of entry it is).
func (w *jsonPrinting) leftOut(path string) bool {
	_, noisy, _ := w.idx.matchKind(strings.ToLower(path))
	return noisy
}

// fieldPath is childPath where paths are read.
func (w *jsonPrinting) fieldPath(path, k string) string {
	if !w.paths {
		return ""
	}
	return childPath(path, k)
}

// elementsPath is the path of the elements of the array at path, as walk
// names them, where paths are read.
func (w *jsonPrinting) elementsPath(path string) string {
	if !w.paths {
		return ""
	}
	return path + "[]"
}

// print is the print of v, the value at path, which no entry names.
func (w *jsonPrinting) print(v any, path string) uint64 {
	switch t := v.(type) {
	case map[string]any:
		// Each field's print, its key mixed with its value's, is summed,
		// so the keys' order is no difference.
		var sum uint64
		for k, fv := range t {
			fp := w.fieldPath(path, k)
			if w.leftOut(fp) {
				continue
			}
			sum += printMix(maphash.String(printSeed, k), w.print(fv, fp))
		}
		return printMix(printObject, sum)
	case []any:
		ep := w.elementsPath(path)
		if w.leftOut(ep) {
			return printArrayLeftOut
		}
		// Each element is mixed in in its order, so the order and the
		// length are differences.
		h := printArray
		for _, ev := range t {
			h = printMix(h, w.print(ev, ep))
		}
		return h
	default:
		typ, val := scalarTypeValue(v)
		if w.excluded != nil && w.excluded(val) {
			w.decided = false
		}
		return printMix(printMix(printScalar, maphash.String(printSeed, typ)), maphash.String(printSeed, val))
	}
}

// printMix mixes b into a, in that order: printMix(a, b) is not
// printMix(b, a).
func printMix(a, b uint64) uint64 {
	h := a*0x9e3779b97f4a7c15 + b
	h ^= h >> 31
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 29
	return h
}

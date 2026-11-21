package matcher

import (
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
)

// A caller that compares one live document against many recorded ones (a
// message matched against every recorded publish of its topic) parses and walks
// each recorded document on every comparison when it goes through the string
// entry points (ValidateAndMarshalJSON, JSONFieldDiffs). The functions here let
// it parse each document once and keep what it needs.
//
// Every document they take is a parsed JSON value exactly as json.Unmarshal
// into an interface{} leaves it (float64 numbers), the form the string entry
// points parse into.

// NewValidatedJSON wraps two parsed documents for JSONDiffWithNoiseControl, as
// ValidateAndMarshalJSON does for two strings: IsIdentical reports whether
// their top-level JSON types agree.
func NewValidatedJSON(expected, actual interface{}) ValidatedJSON {
	return ValidatedJSON{
		expected:    expected,
		actual:      actual,
		isIdentical: areJSONTypesEqual(expected, actual),
	}
}

// JSONMatchKey hashes a parsed document with every object member that noise
// names left out: a member whose key is a global noise key (no dot, any
// depth), or whose lowercased path contains a path noise key, whatever the
// entry's patterns.
//
// Two documents that JSONDiffWithNoiseControl finds exact under noise, with
// ignoreOrdering false, have equal keys. JSONDiffWithNoiseControl excuses a
// difference only at a member noise names (a scalar its pattern matches, a
// member missing or added, a whole subtree), and the key leaves every such
// member out; everything else it compares exactly, and so does the key (object
// members by name in any order, array elements in order, numbers by value).
// So a document whose key differs from the live one's need not be compared.
// Equal keys prove nothing: compare those.
//
// A path entry naming id keys (NoiseIDKey) names a member at any id, as the
// matcher reads it (NoiseKeyCovers), whichever document holds it. The matcher
// excuses less than that, a member the live document added not at all
// (noiseIndex.matchAdded); leaving out more only makes more pairs compared.
func JSONMatchKey(doc interface{}, noise map[string][]string) uint64 {
	k := matchKeyer{h: fnv.New64a()}
	for key := range noise {
		lk := strings.ToLower(key)
		if !strings.Contains(key, ".") {
			if k.global == nil {
				k.global = make(map[string]struct{}, len(noise))
			}
			k.global[lk] = struct{}{}
			continue
		}
		if ek, idKeys := NoiseEntryKey(lk); idKeys {
			k.idPaths = append(k.idPaths, ek)
			continue
		}
		k.paths = append(k.paths, lk)
	}
	k.walk(doc, "")
	return k.h.Sum64()
}

type matchKeyer struct {
	global map[string]struct{}
	paths  []string
	// idPaths are the path entries naming id keys, as NoiseEntryKey spells
	// them: each is tested against the path with its id keys collapsed.
	idPaths []string
	h       interface {
		Write([]byte) (int, error)
		Sum64() uint64
	}
	buf [9]byte
}

// named reports whether noise names the member k at lowercased path p.
func (k *matchKeyer) named(member, p string) bool {
	if _, ok := k.global[strings.ToLower(member)]; ok {
		return true
	}
	for _, e := range k.paths {
		if strings.Contains(p, e) {
			return true
		}
	}
	if len(k.idPaths) > 0 {
		c := CollapseIDKeys(p)
		for _, e := range k.idPaths {
			if strings.Contains(c, e) {
				return true
			}
		}
	}
	return false
}

func (k *matchKeyer) tag(b byte, n int) {
	k.buf[0] = b
	binary.LittleEndian.PutUint64(k.buf[1:], uint64(n))
	_, _ = k.h.Write(k.buf[:])
}

func (k *matchKeyer) str(s string) {
	k.tag('s', len(s))
	_, _ = k.h.Write([]byte(s))
}

// walk hashes v, found at lowercased path p. Array elements share their
// array's path and an object member's path is its parent's plus "." plus its
// name, as JSONDiffWithNoiseControl builds them.
func (k *matchKeyer) walk(v interface{}, p string) {
	switch t := v.(type) {
	case nil:
		k.tag('n', 0)
	case bool:
		if t {
			k.tag('t', 0)
		} else {
			k.tag('f', 0)
		}
	case json.Number:
		if r, ok := toBigRat(t); ok {
			s := r.RatString()
			k.tag('d', len(s))
			_, _ = k.h.Write([]byte(s))
			return
		}
		s := t.String()
		k.tag('d', len(s))
		_, _ = k.h.Write([]byte(s))
	case float64:
		if t == 0 {
			t = 0 // -0 == 0 to the matcher
		}
		if math.IsNaN(t) {
			k.tag('N', 0)
			return
		}
		if r, ok := toBigRat(t); ok {
			s := r.RatString()
			k.tag('d', len(s))
			_, _ = k.h.Write([]byte(s))
			return
		}
		f := strconv.FormatFloat(t, 'g', -1, 64)
		k.tag('d', len(f))
		_, _ = k.h.Write([]byte(f))
	case string:
		k.str(t)
	case map[string]interface{}:
		type member struct{ name, path string }
		members := make([]member, 0, len(t))
		for name := range t {
			cp := strings.ToLower(name)
			if p != "" {
				cp = p + "." + cp
			}
			if k.named(name, cp) {
				continue
			}
			members = append(members, member{name, cp})
		}
		sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
		k.tag('{', len(members))
		for _, m := range members {
			k.str(m.name)
			k.walk(t[m.name], m.path)
		}
	case []interface{}:
		k.tag('[', len(t))
		for _, e := range t {
			k.walk(e, p)
		}
	default:
		k.tag('?', 0)
	}
}

// JSONFields is one side of JSONFieldDiffs: a parsed document's leaf fields,
// collected once under a noise set. It keeps a 64-bit hash of each field's
// path and of its type and value, not the strings, so a caller can keep one
// per recorded document it compares against: two hashes that collide (about
// once in 10^19 pairs) would count one difference too few.
type JSONFields struct {
	fields []fieldHash // sorted by path hash
	// covered are the scalars a pattern-guarded noise entry matched, one per
	// path, sorted by path hash. Whether the pattern excuses them depends on
	// the other side (bothSides), so they are counted only against it.
	covered []fieldHash
	// together holds a document collected under noise naming id keys
	// (NoiseIDKey), and fields and covered are then empty: such an entry
	// excuses a node of the live document only where the recorded one has
	// it (noiseIndex.matchAdded), so the two are read together.
	together *jsonFieldsDoc
}

// jsonFieldsDoc is a document kept for CountJSONFieldDiffs, the index it was
// collected under, and the memory it takes, as Bytes counts it.
type jsonFieldsDoc struct {
	doc   interface{}
	ni    noiseIndex
	bytes int
}

type fieldHash struct{ path, value uint64 }

// Bytes is the memory f's fields take: 16 bytes each, or what its document
// takes when it keeps one. A caller that keeps many counts them against its
// budget.
func (f JSONFields) Bytes() int {
	n := (cap(f.fields) + cap(f.covered)) * 16
	if f.together != nil {
		n += f.together.bytes
	}
	return n
}

// jsonDocBytes estimates what a parsed document takes: 16 bytes for each
// value (an interface) and each object member or array slot, and every
// string's and member name's bytes.
func jsonDocBytes(v interface{}) int {
	n := 16
	switch t := v.(type) {
	case string:
		n += len(t)
	case map[string]interface{}:
		for k, e := range t {
			n += 16 + len(k) + jsonDocBytes(e)
		}
	case []interface{}:
		for _, e := range t {
			n += 16 + jsonDocBytes(e)
		}
	}
	return n
}

func fnv64a(parts ...string) uint64 {
	h := uint64(14695981039346656037)
	for i, s := range parts {
		if i > 0 {
			h *= 1099511628211 // a zero byte between parts
		}
		for j := 0; j < len(s); j++ {
			h = (h ^ uint64(s[j])) * 1099511628211
		}
	}
	return h
}

// CollectJSONFields collects doc under known as JSONFieldDiffs collects each
// side it compares. Under noise naming id keys (NoiseIDKey) it keeps doc
// itself, which the caller must not change: what such an entry excuses in the
// live document depends on the recorded one (noiseIndex.matchAdded).
func CollectJSONFields(doc interface{}, known map[string][]string) JSONFields {
	ni := buildNoiseIndex(known, nil)
	if ni.idKeys {
		return JSONFields{together: &jsonFieldsDoc{doc: doc, ni: ni, bytes: jsonDocBytes(doc)}}
	}
	pm := newPathMaps()
	collectJSON(doc, "", ni, &pm, nil)
	return hashFields(pm)
}

// hashFields is pm's fields as JSONFields holds them.
func hashFields(pm pathMaps) JSONFields {
	out := JSONFields{fields: make([]fieldHash, 0, len(pm.types))}
	for p, typ := range pm.types {
		out.fields = append(out.fields, fieldHash{path: fnv64a(p), value: fnv64a(typ, pm.values[p])})
	}
	sortFieldHashes(out.fields)
	for p, typ := range pm.coveredTypes {
		out.covered = append(out.covered, fieldHash{path: fnv64a(p), value: fnv64a(typ, pm.coveredValues[p])})
	}
	sortFieldHashes(out.covered)
	return out
}

func sortFieldHashes(f []fieldHash) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].path != f[j].path {
			return f[i].path < f[j].path
		}
		return f[i].value < f[j].value
	})
}

// alone is f's fields collected without the other document: what a kept
// document's are when only one side kept it, which happens only for two
// documents collected under different noise.
func (f JSONFields) alone() JSONFields {
	if f.together == nil {
		return f
	}
	pm := newPathMaps()
	collectJSON(f.together.doc, "", f.together.ni, &pm, nil)
	return hashFields(pm)
}

// compared is f's fields counted against other as bothSides leaves them:
// its fields, and each scalar a pattern matched at a path f holds no other
// field at, unless other also holds a matched scalar there. bothSides keeps
// f's matched scalar too when other's path also holds an unmatched one; the
// count is the same either way, one difference at that path.
func (f JSONFields) compared(other JSONFields) []fieldHash {
	if len(f.covered) == 0 {
		return f.fields
	}
	plain := make(map[uint64]bool, len(f.fields))
	for _, h := range f.fields {
		plain[h.path] = true
	}
	otherCovered := make(map[uint64]bool, len(other.covered))
	for _, h := range other.covered {
		otherCovered[h.path] = true
	}
	out := append(make([]fieldHash, 0, len(f.fields)+len(f.covered)), f.fields...)
	for _, h := range f.covered {
		if plain[h.path] || otherCovered[h.path] {
			continue
		}
		out = append(out, h)
	}
	sortFieldHashes(out)
	return out
}

// CountJSONFieldDiffs is len(JSONFieldDiffs(exp, act, known, ...)) for two
// documents collected under the same known noise, without building the diffs:
// the fields only one side has, plus those whose type or value differs. exp
// is the recorded document and act the live one.
func CountJSONFieldDiffs(exp, act JSONFields) int {
	if exp.together != nil && act.together != nil {
		// Under noise naming id keys: read together, as JSONFieldDiffs does.
		e, a := collectBoth(exp.together.doc, act.together.doc, exp.together.ni)
		added, removed, typeChanges, valueChanges := diffMaps(e, a)
		return len(added) + len(removed) + len(typeChanges) + len(valueChanges)
	}
	exp, act = exp.alone(), act.alone()
	e, a := exp.compared(act), act.compared(exp)
	n, i, j := 0, 0, 0
	for i < len(e) && j < len(a) {
		switch {
		case e[i].path < a[j].path:
			n++ // removed
			i++
		case e[i].path > a[j].path:
			n++ // added
			j++
		default:
			if e[i].value != a[j].value {
				n++ // type or value changed
			}
			i++
			j++
		}
	}
	return n + len(e) - i + len(a) - j
}

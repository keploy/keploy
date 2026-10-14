package matcher

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"reflect"
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
		isIdentical: reflect.TypeOf(expected) == reflect.TypeOf(actual),
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
		k.paths = append(k.paths, lk)
	}
	k.walk(doc, "")
	return k.h.Sum64()
}

type matchKeyer struct {
	global map[string]struct{}
	paths  []string
	h      interface {
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
	case float64:
		if t == 0 {
			t = 0 // -0 == 0 to the matcher
		}
		if math.IsNaN(t) {
			k.tag('N', 0)
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
}

type fieldHash struct{ path, value uint64 }

// Bytes is the memory f's fields take: 16 bytes each. A caller that keeps many
// counts them against its budget.
func (f JSONFields) Bytes() int { return cap(f.fields) * 16 }

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
// side it compares.
func CollectJSONFields(doc interface{}, known map[string][]string) JSONFields {
	pm := pathMaps{types: map[string]string{}, values: map[string]string{}}
	collectJSON(doc, "", buildNoiseIndex(known, nil), &pm)
	out := JSONFields{fields: make([]fieldHash, 0, len(pm.types))}
	for p, typ := range pm.types {
		out.fields = append(out.fields, fieldHash{path: fnv64a(p), value: fnv64a(typ, pm.values[p])})
	}
	sort.Slice(out.fields, func(i, j int) bool {
		if out.fields[i].path != out.fields[j].path {
			return out.fields[i].path < out.fields[j].path
		}
		return out.fields[i].value < out.fields[j].value
	})
	return out
}

// CountJSONFieldDiffs is len(JSONFieldDiffs(exp, act, known, ...)) for two
// documents collected under the same known noise, without building the diffs:
// the fields only one side has, plus those whose type or value differs.
func CountJSONFieldDiffs(exp, act JSONFields) int {
	e, a := exp.fields, act.fields
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

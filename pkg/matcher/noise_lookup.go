package matcher

import (
	"regexp"
	"strings"
)

// NoiseLookup answers, for one noise configuration, whether a JSON leaf of a
// request is noise. An entry names whole fields: it matches where it occurs
// in the leaf's path as a run of whole fields, at any depth, beginning at the
// path's start or after a ".", and ending at its end, at a "." or at the "[]"
// of an array (so a single-field entry is that field's name at any depth, and
// what lies under a matched field is covered with it). It never matches part
// of a field's name: "id" covers "user.id" and "items[].id", never "paid" or
// "identifier"; "user.id" never covers "superuser.id"; "user" never covers the
// form field "user[id]", one name. Field names compare without case, as
// keploy.yml's keys are read. The most specific entry wins; an entry with no
// patterns excuses the leaf and its subtree, and one with patterns excuses
// only a scalar value a pattern matches. Paths are in the
// request matcher's dialect: object keys dot-joined, an array written "[]"
// ("items[].id").
//
// The request matcher with the proven switch off (ChangedJSONFieldPaths)
// still reads an entry as a substring of the path; this lookup is the strict
// comparison's (design v14 row S1), which must not excuse a field the entry
// does not name.
//
// The learn step (package learn) reads keploy.yml's declared body noise
// through it, so the requests it pairs are compared modulo exactly the noise
// the replay that follows will excuse.
type NoiseLookup struct{ idx noiseIndex }

// NewNoiseLookup builds the lookup for noise (path -> patterns).
func NewNoiseLookup(noise map[string][]string) NoiseLookup {
	return NoiseLookup{idx: buildNoiseIndex(noise, nil)}
}

// Names reports whether an entry names the field at path, whatever its value:
// Covers may excuse it (an entry with patterns excuses only a value they
// match), and excuses nothing an entry does not name.
func (l NoiseLookup) Names(path string) bool {
	_, noisy := l.idx.matchWholeFields(strings.ToLower(path))
	return noisy
}

// Signature is the fields the lookup names, as one string: two lookups of
// one signature name the same fields (Names).
func (l NoiseLookup) Signature() string {
	keys := make([]string, 0, len(l.idx.entries))
	for _, e := range l.idx.entries {
		keys = append(keys, e.keyLower)
	}
	return strings.Join(keys, "\x00")
}

// Covers reports whether the leaf at path is noise. value is its text when it
// is a scalar (scalar true); a container is excused only by an entry with no
// patterns.
func (l NoiseLookup) Covers(path, value string, scalar bool) bool {
	regs, noisy := l.idx.matchWholeFields(strings.ToLower(path))
	if !noisy {
		return false
	}
	if len(regs) == 0 {
		return true
	}
	return scalar && anyRegexpMatchStr(value, regs)
}

// matchWholeFields is match with entries naming whole fields (namesWholeFields):
// the first, most specific, entry that does wins.
func (ni noiseIndex) matchWholeFields(keyLower string) (regs []*regexp.Regexp, isNoisy bool) {
	for _, e := range ni.entries {
		if namesWholeFields(keyLower, e.keyLower) {
			return e.regexps, true
		}
	}
	return nil, false
}

// namesWholeFields reports whether entry occurs in path as a run of whole
// fields: beginning at path's start or after a ".", and ending at its end, at
// a "." or at the "[]" of an array. A "[" that opens no "[]" is part of a
// name (the form field "user[id]"). An empty entry names no field.
func namesWholeFields(path, entry string) bool {
	if entry == "" {
		return false
	}
	for from := 0; from < len(path); {
		i := strings.Index(path[from:], entry)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(entry)
		if (i == 0 || path[i-1] == '.') && (end == len(path) || path[end] == '.' || strings.HasPrefix(path[end:], "[]")) {
			return true
		}
		from = i + 1
	}
	return false
}

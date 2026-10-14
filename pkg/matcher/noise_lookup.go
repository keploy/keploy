package matcher

import "strings"

// NoiseLookup answers, for one noise configuration, whether a JSON leaf of a
// request is noise, with the semantics the request matcher's JSON diff
// (ChangedJSONFieldPaths) gives the same configuration: an entry is matched
// case-insensitively as a substring of the leaf's path, the most specific
// entry first; an entry with no patterns excuses the leaf and its subtree,
// and one with patterns excuses only a scalar value a pattern matches. Paths
// are in that matcher's dialect: object keys dot-joined, an array written
// "[]" ("items[].id").
//
// The learn step (package learn) reads keploy.yml's declared body noise
// through it, so the requests it pairs are compared modulo exactly the noise
// the replay that follows will excuse.
type NoiseLookup struct{ idx noiseIndex }

// NewNoiseLookup builds the lookup for noise (path -> patterns).
func NewNoiseLookup(noise map[string][]string) NoiseLookup {
	return NoiseLookup{idx: buildNoiseIndex(noise, nil)}
}

// Covers reports whether the leaf at path is noise. value is its text when it
// is a scalar (scalar true); a container is excused only by an entry with no
// patterns.
func (l NoiseLookup) Covers(path, value string, scalar bool) bool {
	regs, noisy := l.idx.match(strings.ToLower(path))
	if !noisy {
		return false
	}
	if len(regs) == 0 {
		return true
	}
	return scalar && anyRegexpMatchStr(value, regs)
}

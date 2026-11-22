package matcher

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// PatternWhy is why a field keploy.yml's noise lets vary only within
// patterns is compared: the side's value is outside them. shape names that
// value by its kind and length, never its text, since a value a noise
// pattern guards may be a secret.
func PatternWhy(field string, patterns []string, side, shape string) string {
	quoted := make([]string, len(patterns))
	for i, p := range patterns {
		quoted[i] = strconv.Quote(p)
	}
	return fmt.Sprintf("keploy.yml's noise lets %s vary only within %s; the %s value (%s) is outside it, so it is compared", field, strings.Join(quoted, ", "), side, shape)
}

// StringShape names a string by its length in bytes, and NumberShape a
// number by the characters it is written in: a reason's words for a value a
// noise pattern guards, never its text.
func StringShape(n int) string { return "a string of " + counted(n, "byte") }

// NumberShape: see StringShape.
func NumberShape(n int) string { return "a number of " + counted(n, "character") }

func counted(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// bodyPatternWhys are the reasons for the differences at paths (as diffMaps
// names them) that a pattern-guarded entry decided: the live value is outside
// the pattern unless the live side held only values it matched there (liveIn,
// read before bothSides), and then the recorded one is. A path is named only
// where the response comparison holds it to the same patterns the path maps
// read (matcherPatternsAt), so a reason never names a pattern the comparison
// did not apply.
func bodyPatternWhys(ni noiseIndex, bodyNoise map[string][]string, exp, act pathMaps, liveIn map[string]bool, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	matcherPaths, matcherGlobal := splitJSONNoise(bodyNoise, nil)
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	var out []string
	for _, p := range sorted {
		// No entry this index holds is type-strict: ComputeFailureAssessmentJSON
		// marks none.
		regs, _, _ := ni.matchKind(strings.ToLower(p))
		applied := regexpTexts(matcherPatternsAt(matcherPaths, matcherGlobal, p))
		if len(applied) == 0 || !slices.Equal(applied, regexpTexts(regs)) || holdsContainers(exp, act, p) {
			continue
		}
		side, m := "live", act
		if liveIn[p] {
			side, m = "recorded", exp
		}
		out = append(out, PatternWhy(p, applied, side, pathShape(m, p)))
	}
	return out
}

// matcherPatternsAt are the patterns the response comparison
// (matchJSONWithNoiseHandlingIndexed, under splitJSONNoise's split) holds the
// scalar at path, as collectJSON names it, to; none where it holds it to no
// pattern. A dot-free entry names a field by its name, wherever it is, and
// excuses a container it names whole; a dotted one names a path, and accepts
// every value under an object or a nested array it names. An array's scalar
// elements are compared under the array's own name, each held to its
// pattern.
func matcherPatternsAt(paths noiseIndex, global map[string][]*regexp.Regexp, path string) []*regexp.Regexp {
	key := strings.ToLower(strings.ReplaceAll(path, "[]", ""))
	segs := strings.Split(key, ".")
	for i := 1; i < len(segs); i++ {
		if _, ok := global[segs[i-1]]; ok {
			return nil
		}
		if _, noisy := paths.match(strings.Join(segs[:i], ".")); noisy {
			return nil
		}
	}
	elem := strings.HasSuffix(path, "[]")
	if regs, ok := global[segs[len(segs)-1]]; ok {
		if elem {
			return nil // it names the array, which it excuses whole
		}
		return regs
	}
	if elem && strings.HasSuffix(strings.TrimSuffix(path, "[]"), "[]") {
		return nil // an array of arrays: its values are accepted
	}
	regs, _ := paths.match(key)
	return regs
}

// holdsContainers reports whether either side holds an object or an array
// at path, or, where path names an array's elements ("x[]"), an object
// among them: the comparison then accepts the values under a pattern, as it
// does an object's, or a dot-free entry excuses the container whole, or the
// field changed its type, which no pattern decides.
func holdsContainers(exp, act pathMaps, path string) bool {
	for _, m := range []map[string]string{exp.types, act.types, exp.coveredTypes, act.coveredTypes} {
		for p := range m {
			if strings.HasPrefix(p, path+".") || strings.HasPrefix(p, path+"[]") {
				return true
			}
		}
	}
	return false
}

// pathShape is the shape of m's value at path.
func pathShape(m pathMaps, path string) string {
	typ, ok := m.types[path]
	if !ok {
		return "absent"
	}
	switch typ {
	case "string":
		return StringShape(len(m.values[path]))
	case "number":
		return NumberShape(len(m.values[path]))
	case "boolean":
		return "a boolean"
	}
	return "null"
}

func regexpTexts(regs []*regexp.Regexp) []string {
	out := make([]string, len(regs))
	for i, re := range regs {
		out[i] = re.String()
	}
	return out
}

// HeaderPatternWhys are the reasons for the headers of res (CompareHeaders')
// that differ while keploy.yml's noise lets them vary only within patterns:
// the live values are outside them unless each matches, and then the
// recorded ones are. A header's shape is its first value outside the
// patterns, by length, or absent.
func HeaderPatternWhys(res []models.HeaderResult, noise map[string][]string) []string {
	lowered := lowerHeaderNoise(noise)
	var out []string
	for _, hr := range res {
		if hr.Normal {
			continue
		}
		name := hr.Expected.Key // CompareHeaders names both sides
		pats, _ := SubstringKeyMatch(name, lowered)
		if len(pats) == 0 {
			continue
		}
		side, values := "live", hr.Actual.Value
		if allMatchAnyRegex(values, pats) {
			side, values = "recorded", hr.Expected.Value
		}
		out = append(out, PatternWhy("header."+name, pats, side, headerShape(values, pats)))
	}
	sort.Strings(out)
	return out
}

func headerShape(values, pats []string) string {
	for _, v := range values {
		if ok, _ := MatchesAnyRegex(v, pats); !ok {
			return StringShape(len(v))
		}
	}
	return "absent"
}

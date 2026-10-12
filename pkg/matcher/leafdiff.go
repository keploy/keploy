package matcher

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
)

type leafWalk struct {
	ni       noiseIndex
	global   map[string][]*regexp.Regexp
	ordering bool
	prefix   string
	maxVal   int
	limit    int
	out      []models.MockFieldDiff
	total    int
}

func JSONLeafDiffs(expJSON, actJSON string, noise map[string][]string, ignoreOrdering bool, prefix string, maxVal, limit int) ([]models.MockFieldDiff, int) {
	var exp, act any
	if json.Unmarshal([]byte(expJSON), &exp) != nil || json.Unmarshal([]byte(actJSON), &act) != nil {
		return nil, 0
	}
	w := &leafWalk{global: map[string][]*regexp.Regexp{}, ordering: ignoreOrdering, prefix: prefix, maxVal: maxVal, limit: limit}
	for lk, k := range globalNoiseKeys(noise) {
		w.global[lk] = compilePatterns(noise[k], nil)
	}
	path := map[string][]string{}
	for k, v := range noise {
		if strings.Contains(k, ".") {
			path[k] = v
		}
	}
	w.ni = buildNoiseIndex(path, nil)
	w.walk("", "", shallow(exp, 0), shallow(act, 0), false)
	return w.out, w.total
}

func (w *leafWalk) add(path string, kind models.MockFieldDiffKind, e, a any, inE, inA bool) {
	w.total++
	if w.limit > 0 && len(w.out) >= w.limit {
		return
	}
	d := models.MockFieldDiff{Path: joinPath(w.prefix, path), Kind: kind}
	if inE {
		d.Expected = clip(jsonText(e), w.maxVal)
	}
	if inA {
		d.Actual = clip(jsonText(a), w.maxVal)
	}
	w.out = append(w.out, d)
}

func (w *leafWalk) walk(key, path string, e, a any, ancestor bool) {
	if w.matches(key, e, a, ancestor) {
		return
	}
	_, ce := e.(capped)
	_, ca := a.(capped)
	if ce || ca {
		ue, ue2 := unshallow(e)
		ua, ua2 := unshallow(a)
		if (ue2 || ua2) && w.matches(key, ue, ua, ancestor) {
			return
		}
		w.add(path, models.DiffKindValueChanged, ue, ua, true, true)
		return
	}
	switch em := e.(type) {
	case map[string]any:
		am, ok := a.(map[string]any)
		if !ok {
			break
		}
		_, noisy := w.ni.match(strings.ToLower(key))
		ancestor = ancestor || noisy
		prefix := ""
		if key != "" {
			prefix = key + "."
		}
		for _, k := range sortedKeys(em, am) {
			ev, inE := em[k]
			av, inA := am[k]
			child, childPath := prefix+k, joinKey(path, k)
			gregs, isGlobal := w.global[strings.ToLower(k)]
			pregs, pathNoisy := w.ni.match(strings.ToLower(child))
			switch {
			case !inA:
				if isGlobal && len(gregs) == 0 || pathNoisy && len(pregs) == 0 {
					continue
				}
				w.add(childPath, models.DiffKindMissingInLive, ev, nil, true, false)
			case !inE:
				if isGlobal && noisyForRegexps(gregs, av, false) || pathNoisy && noisyForRegexps(pregs, av, false) {
					continue
				}
				w.add(childPath, models.DiffKindMissingInMock, nil, av, false, true)
			default:
				if isGlobal && noisyForRegexps(gregs, av, true) || pathNoisy && len(pregs) == 0 {
					continue
				}
				w.walk(child, childPath, ev, av, ancestor)
			}
		}
		return
	case []any:
		aa, ok := a.([]any)
		if !ok || len(em) != len(aa) || w.ordering {
			break
		}
		regs, noisy := w.ni.match(strings.ToLower(key))
		if noisy && len(regs) > 0 && !allJSONScalars(em) {
			ancestor = true
		}
		for i := range em {
			w.walk(key, path+"["+strconv.Itoa(i)+"]", em[i], aa[i], ancestor)
		}
		return
	}
	ue, ue2 := unshallow(e)
	ua, ua2 := unshallow(a)
	if (ue2 || ua2) && w.matches(key, ue, ua, ancestor) {
		return
	}
	w.add(path, models.DiffKindValueChanged, ue, ua, true, true)
}

func (w *leafWalk) matches(key string, e, a any, ancestor bool) bool {
	res, err := matchJSONWithNoiseHandlingIndexed(key, e, a, w.ni, w.global, w.ordering, ancestor, nil)
	return err == nil && res.matches
}

type capped struct{ v any }

func shallow(v any, depth int) any {
	switch t := v.(type) {
	case map[string]any:
		if depth >= maxLeafDepth {
			return capped{t}
		}
		out := make(map[string]any, len(t))
		for k, c := range t {
			out[k] = shallow(c, depth+1)
		}
		return out
	case []any:
		if depth >= maxLeafDepth {
			return capped{t}
		}
		out := make([]any, len(t))
		for i, c := range t {
			out[i] = shallow(c, depth+1)
		}
		return out
	}
	return v
}

func unshallow(v any) (any, bool) {
	switch t := v.(type) {
	case capped:
		return t.v, true
	case map[string]any:
		var out map[string]any
		for k, c := range t {
			if u, ok := unshallow(c); ok {
				if out == nil {
					out = make(map[string]any, len(t))
					for k2, c2 := range t {
						out[k2] = c2
					}
				}
				out[k] = u
			}
		}
		if out != nil {
			return out, true
		}
	case []any:
		var out []any
		for i, c := range t {
			if u, ok := unshallow(c); ok {
				if out == nil {
					out = append([]any(nil), t...)
				}
				out[i] = u
			}
		}
		if out != nil {
			return out, true
		}
	}
	return v, false
}

const maxLeafDepth = 64

func joinKey(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func sortedKeys(e, a map[string]any) []string {
	keys := make([]string, 0, len(e)+len(a))
	for k := range e {
		keys = append(keys, k)
	}
	for k := range a {
		if _, ok := e[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func jsonText(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if f, ok := v.(float64); ok {
		return formatJSONNumber(f)
	}
	if enc.Encode(v) != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func joinPath(prefix, p string) string {
	switch {
	case p == "":
		return strings.TrimSuffix(prefix, ".")
	case strings.HasPrefix(p, "["):
		return strings.TrimSuffix(prefix, ".") + p
	}
	return prefix + p
}

func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}

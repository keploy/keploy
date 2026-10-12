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

var arrayIndex = regexp.MustCompile(`\[\d+\]`)

func JSONLeafDiffs(expJSON, actJSON string, noise map[string][]string, prefix string, maxVal, limit int) ([]models.MockFieldDiff, int) {
	var exp, act any
	if decode(expJSON, &exp) != nil || decode(actJSON, &act) != nil {
		return nil, 0
	}
	ni := buildNoiseIndex(noise, nil)
	e, a := map[string]string{}, map[string]string{}
	leaves(exp, "", ni, e)
	leaves(act, "", ni, a)
	paths := map[string]bool{}
	for p := range e {
		paths[p] = true
	}
	for p := range a {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		if e[p] != a[p] {
			sorted = append(sorted, p)
		}
	}
	sort.Strings(sorted)
	var out []models.MockFieldDiff
	for _, p := range sorted {
		if limit > 0 && len(out) == limit {
			break
		}
		ev, inE := e[p]
		av, inA := a[p]
		d := models.MockFieldDiff{Path: joinPath(prefix, p), Expected: clip(ev, maxVal), Actual: clip(av, maxVal), Kind: models.DiffKindValueChanged}
		switch {
		case !inA:
			d.Kind = models.DiffKindMissingInLive
		case !inE:
			d.Kind = models.DiffKindMissingInMock
		}
		out = append(out, d)
	}
	return out, len(sorted)
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

func decode(s string, v *any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	return dec.Decode(v)
}

func leaves(v any, path string, ni noiseIndex, out map[string]string) {
	regs, noisy, typeStrict := ni.matchKind(strings.ToLower(arrayIndex.ReplaceAllString(path, "[]")))
	if noisy && !typeStrict {
		if len(regs) == 0 {
			return
		}
		s, ok := jsonScalarToString(v)
		if n, isNum := v.(json.Number); isNum {
			s, ok = n.String(), true
		}
		if ok && anyRegexpMatchStr(s, regs) {
			return
		}
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[path] = "{}"
		}
		for k, c := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			leaves(c, p, ni, out)
		}
	case []any:
		if len(t) == 0 {
			out[path] = "[]"
		}
		for i, c := range t {
			leaves(c, path+"["+strconv.Itoa(i)+"]", ni, out)
		}
	default:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if enc.Encode(t) == nil {
			out[path] = strings.TrimSuffix(b.String(), "\n")
		}
	}
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

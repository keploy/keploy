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

func JSONLeafDiffs(expJSON, actJSON string, noise map[string][]string, prefix string, maxVal int) []models.MockFieldDiff {
	var exp, act any
	if decode(expJSON, &exp) != nil || decode(actJSON, &act) != nil {
		return nil
	}
	e, a := map[string]string{}, map[string]string{}
	leaves(exp, "", e)
	leaves(act, "", a)
	paths := map[string]bool{}
	for p := range e {
		paths[p] = true
	}
	for p := range a {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		if !noisy(p, noise) {
			sorted = append(sorted, p)
		}
	}
	sort.Strings(sorted)
	var out []models.MockFieldDiff
	for _, p := range sorted {
		ev, inE := e[p]
		av, inA := a[p]
		d := models.MockFieldDiff{Path: prefix + p, Expected: clip(ev, maxVal), Actual: clip(av, maxVal)}
		switch {
		case !inA:
			d.Kind = models.DiffKindMissingInLive
		case !inE:
			d.Kind = models.DiffKindMissingInMock
		case ev == av:
			continue
		default:
			d.Kind = models.DiffKindValueChanged
		}
		out = append(out, d)
	}
	return out
}

func decode(s string, v *any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	return dec.Decode(v)
}

func leaves(v any, path string, out map[string]string) {
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
			leaves(c, p, out)
		}
	case []any:
		if len(t) == 0 {
			out[path] = "[]"
		}
		for i, c := range t {
			leaves(c, path+"["+strconv.Itoa(i)+"]", out)
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

func noisy(path string, noise map[string][]string) bool {
	plain := arrayIndex.ReplaceAllString(path, "")
	for k := range noise {
		k = strings.ReplaceAll(k, "[]", "")
		if plain == k || strings.HasPrefix(plain, k+".") {
			return true
		}
	}
	return false
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

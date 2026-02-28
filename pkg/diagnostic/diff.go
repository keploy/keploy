package diagnostic

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

var (
	uuidRe    = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	ulidRe    = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	isoTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$`)
	dateTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?$`)
)

// ComputeJSONDiff returns a structured diff report for JSON inputs.
func ComputeJSONDiff(expectedJSON, actualJSON string, ignoreOrdering bool) (*models.DiffReport, error) {
	var exp, act interface{}
	if err := json.Unmarshal([]byte(expectedJSON), &exp); err != nil {
		return nil, fmt.Errorf("invalid expected JSON: %w", err)
	}
	if err := json.Unmarshal([]byte(actualJSON), &act); err != nil {
		return nil, fmt.Errorf("invalid actual JSON: %w", err)
	}

	var entries []models.DiffEntry
	diffValues(exp, act, "", ignoreOrdering, &entries)

	category := categorize(entries)
	confidence := score(entries)

	return &models.DiffReport{
		Entries:    entries,
		Category:   category,
		Confidence: confidence,
	}, nil
}

func diffValues(exp, act interface{}, path string, ignoreOrdering bool, entries *[]models.DiffEntry) {
	switch e := exp.(type) {
	case map[string]interface{}:
		amap, ok := act.(map[string]interface{})
		if !ok {
			appendEntry(entries, path, models.DiffTypeChanged, exp, act, models.CategorySchemaChange)
			return
		}
		keys := make(map[string]struct{})
		for k := range e {
			keys[k] = struct{}{}
		}
		for k := range amap {
			keys[k] = struct{}{}
		}
		keyList := make([]string, 0, len(keys))
		for k := range keys {
			keyList = append(keyList, k)
		}
		sort.Strings(keyList)
		for _, k := range keyList {
			p := joinPath(path, k)
			ev, eok := e[k]
			av, aok := amap[k]
			switch {
			case eok && !aok:
				appendEntry(entries, p, models.DiffRemoved, ev, nil, models.CategorySchemaChange)
			case !eok && aok:
				appendEntry(entries, p, models.DiffAdded, nil, av, models.CategorySchemaChange)
			default:
				diffValues(ev, av, p, ignoreOrdering, entries)
			}
		}
	case []interface{}:
		aslice, ok := act.([]interface{})
		if !ok {
			appendEntry(entries, path, models.DiffTypeChanged, exp, act, models.CategorySchemaChange)
			return
		}
		if ignoreOrdering {
			if arraysEqualUnordered(e, aslice) {
				if !arraysEqualOrdered(e, aslice) {
					appendEntry(entries, path+"[]", models.DiffModified, "order", "order", models.CategoryDynamicNoise)
				}
				return
			}
		}
		minLen := len(e)
		if len(aslice) < minLen {
			minLen = len(aslice)
		}
		for i := 0; i < minLen; i++ {
			p := fmt.Sprintf("%s[%d]", path, i)
			diffValues(e[i], aslice[i], p, ignoreOrdering, entries)
		}
		if len(e) > minLen {
			for i := minLen; i < len(e); i++ {
				p := fmt.Sprintf("%s[%d]", path, i)
				appendEntry(entries, p, models.DiffRemoved, e[i], nil, models.CategorySchemaChange)
			}
		}
		if len(aslice) > minLen {
			for i := minLen; i < len(aslice); i++ {
				p := fmt.Sprintf("%s[%d]", path, i)
				appendEntry(entries, p, models.DiffAdded, nil, aslice[i], models.CategorySchemaChange)
			}
		}
	default:
		if !valuesEqual(exp, act) {
			cat := classifyValueChange(act)
			appendEntry(entries, path, models.DiffModified, exp, act, cat)
		}
	}
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func valuesEqual(a, b interface{}) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func arraysEqualOrdered(a, b []interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valuesEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func arraysEqualUnordered(a, b []interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	al := make([]string, 0, len(a))
	bl := make([]string, 0, len(b))
	for _, v := range a {
		al = append(al, canonicalJSON(v))
	}
	for _, v := range b {
		bl = append(bl, canonicalJSON(v))
	}
	sort.Strings(al)
	sort.Strings(bl)
	for i := range al {
		if al[i] != bl[i] {
			return false
		}
	}
	return true
}

func canonicalJSON(v interface{}) string {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString("{")
		for i, k := range keys {
			if i > 0 {
				sb.WriteString(",")
			}
			b, _ := json.Marshal(k)
			sb.Write(b)
			sb.WriteString(":")
			sb.WriteString(canonicalJSON(t[k]))
		}
		sb.WriteString("}")
		return sb.String()
	case []interface{}:
		var sb strings.Builder
		sb.WriteString("[")
		for i, v := range t {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(canonicalJSON(v))
		}
		sb.WriteString("]")
		return sb.String()
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func appendEntry(entries *[]models.DiffEntry, path string, kind models.DiffKind, exp, act interface{}, category models.DiffCategory) {
	*entries = append(*entries, models.DiffEntry{
		Path:     path,
		Kind:     kind,
		Expected: exp,
		Actual:   act,
		Category: category,
	})
}

func classifyValueChange(val interface{}) models.DiffCategory {
	if isDynamicValue(val) {
		return models.CategoryDynamicNoise
	}
	return models.CategoryDataUpdate
}

func isDynamicValue(val interface{}) bool {
	switch v := val.(type) {
	case string:
		if uuidRe.MatchString(v) || ulidRe.MatchString(v) || isoTimeRe.MatchString(v) || dateTimeRe.MatchString(v) {
			return true
		}
		if looksLikeEpoch(v) {
			return true
		}
	case float64:
		if v > 1e9 {
			return true
		}
	}
	return false
}

func looksLikeEpoch(s string) bool {
	if len(s) < 10 || len(s) > 13 {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

func categorize(entries []models.DiffEntry) models.DiffCategory {
	hasSchemaChange := false
	hasDataUpdate := false
	allDynamic := true

	for _, e := range entries {
		switch e.Kind {
		case models.DiffAdded, models.DiffRemoved, models.DiffTypeChanged:
			hasSchemaChange = true
		}
		if e.Category == models.CategoryDataUpdate {
			hasDataUpdate = true
			allDynamic = false
		}
		if e.Category != models.CategoryDynamicNoise && e.Category != models.CategoryDataUpdate {
			allDynamic = false
		}
	}

	switch {
	case hasSchemaChange:
		return models.CategorySchemaChange
	case len(entries) == 0:
		return models.CategoryDataUpdate
	case allDynamic:
		return models.CategoryDynamicNoise
	case hasDataUpdate:
		return models.CategoryDataUpdate
	default:
		return models.CategoryDataUpdate
	}
}

func score(entries []models.DiffEntry) int {
	score := 50
	schemaChange := false
	dynamicCount := 0
	for _, e := range entries {
		switch e.Kind {
		case models.DiffTypeChanged:
			score -= 25
			schemaChange = true
		case models.DiffRemoved:
			score -= 20
			schemaChange = true
		case models.DiffAdded:
			score -= 10
			schemaChange = true
		case models.DiffModified:
			if e.Category == models.CategoryDynamicNoise {
				score += 15
				dynamicCount++
			} else {
				score += 5
			}
		}
	}

	if len(entries) == 0 {
		score = 100
	}
	if len(entries) > 10 {
		score -= 20
	}
	if schemaChange {
		if score > 60 {
			score = 60
		}
	}
	if dynamicCount == len(entries) && len(entries) > 0 {
		if score < 90 {
			score = 90
		}
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}

package matcher

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

type pathMaps struct {
	types  map[string]string // path -> type ("string","number","boolean","null")
	values map[string]string // path -> scalar stringified value (only for primitives)
}

func ComputeFailureAssessmentJSON(expJSON, actJSON string, bodyNoise map[string][]string, ignoreOrdering bool) (*models.FailureAssessment, error) {
	// Quick exit if either side isn't JSON
	if !json.Valid([]byte(expJSON)) || !json.Valid([]byte(actJSON)) {
		return nil, nil
	}

	var exp, act interface{}
	if err := json.Unmarshal([]byte(expJSON), &exp); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(actJSON), &act); err != nil {
		return nil, err
	}

	idx := buildNoiseIndex(bodyNoise) // already in matcher/utils.go

	expMaps := pathMaps{types: map[string]string{}, values: map[string]string{}}
	actMaps := pathMaps{types: map[string]string{}, values: map[string]string{}}

	collectJSON(exp, "", idx, &expMaps)
	collectJSON(act, "", idx, &actMaps)

	added, removed, typeChanges, valueChanges := diffMaps(expMaps, actMaps)

	assess := &models.FailureAssessment{
		AddedFields:   added,
		RemovedFields: removed,
		TypeChanges:   typeChanges,
		ValueChanges:  valueChanges,
	}

	hasAdded := len(added) > 0
	hasRemoved := len(removed) > 0
	hasType := len(typeChanges) > 0
	hasValue := len(valueChanges) > 0

	// Build reasons (human friendly)
	if hasRemoved {
		assess.Reasons = append(assess.Reasons, "Removed fields: "+strings.Join(removed, ", "))
	}
	if hasType {
		assess.Reasons = append(assess.Reasons, "Type changes at: "+strings.Join(typeChanges, ", "))
	}
	if hasAdded && !hasRemoved && !hasType && !hasValue {
		assess.Reasons = append(assess.Reasons, "Backward-compatible: only new fields added.")
	}
	if hasAdded && !hasRemoved && !hasType && hasValue {
		assess.Reasons = append(assess.Reasons, "Backward-compatible (with value differences): new fields plus value changes on existing fields.")
	}
	if !hasAdded && !hasRemoved && !hasType && hasValue {
		assess.Reasons = append(assess.Reasons, "Schema identical; only values changed.")
	}
	if !hasAdded && !hasRemoved && !hasType && !hasValue {
		assess.Reasons = append(assess.Reasons, "Schema and values are identical.")
	}

	// Categorize per spec:
	// - Schema Changes (removed/type change) -> High
	// - Schema Same (value-only)             -> Medium
	// - Schema Addition:
	//     * only new fields                  -> Low
	//     * new + some value changes        -> Medium
	// - No differences at all               -> SchemaUnchanged / None
	switch {
	case hasRemoved || hasType:
		assess.Category = []models.FailureCategory{models.SchemaBroken}
		assess.Risk = models.High

	case !hasAdded && !hasRemoved && !hasType && !hasValue:
		assess.Category = []models.FailureCategory{models.SchemaUnchanged}
		assess.Risk = models.None

	case !hasAdded && !hasRemoved && !hasType && hasValue:
		assess.Category = []models.FailureCategory{models.SchemaUnchanged}
		assess.Risk = models.Medium

	case hasAdded && !hasRemoved && !hasType && !hasValue:
		assess.Category = []models.FailureCategory{models.SchemaAdded}
		assess.Risk = models.Low

	case hasAdded && !hasRemoved && !hasType && hasValue:
		assess.Category = []models.FailureCategory{models.SchemaAdded}
		assess.Risk = models.Medium

	default:
		// Mixed but already-handled breaking cases (added along with removed/type change) fall here defensively.
		assess.Category = []models.FailureCategory{models.SchemaBroken}
		assess.Risk = models.High
	}

	return assess, nil
}

func collectJSON(v interface{}, path string, ni noiseIndex, out *pathMaps) {
	keyLower := strings.ToLower(path)
	if regs, noisy := ni.match(keyLower); noisy && len(regs) == 0 {
		// whole subtree is noisy → ignore
		return
	}

	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			collectJSON(child, p, ni, out)
		}
	case []interface{}:
		// normalize array paths using [] suffix so we don't depend on indices
		p := path
		if p != "" {
			p += "[]"
		} else {
			p = "[]"
		}
		for _, e := range t {
			collectJSON(e, p, ni, out)
		}
	case string:
		out.types[path] = "string"
		out.values[path] = t
	case float64:
		out.types[path] = "number"
		out.values[path] = fmt.Sprintf("%v", t)
	case bool:
		out.types[path] = "boolean"
		out.values[path] = strconv.FormatBool(t)
	case nil:
		out.types[path] = "null"
		out.values[path] = "null"
	default:
		// other JSON forms won't appear here
	}
}

func diffMaps(exp, act pathMaps) (added, removed, typeChanges, valueChanges []string) {
	seen := map[string]struct{}{}

	// look at expected keys
	for k, expType := range exp.types {
		seen[k] = struct{}{}
		actType, ok := act.types[k]
		if !ok {
			removed = append(removed, k)
			continue
		}
		if expType != actType {
			typeChanges = append(typeChanges, k)
			continue
		}
		// same type and it's a primitive leaf we tracked
		ev, eok := exp.values[k]
		av, aok := act.values[k]
		// If both present and differ -> value change
		if eok && aok && ev != av {
			valueChanges = append(valueChanges, k)
		}
	}

	// anything new in actual?
	for k := range act.types {
		if _, ok := seen[k]; !ok {
			added = append(added, k)
		}
	}
	return
}

func MaxRisk(a, b models.RiskLevel) models.RiskLevel {
	rank := map[models.RiskLevel]int{
		models.None:   0,
		models.Low:    1,
		models.Medium: 2,
		models.High:   3,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

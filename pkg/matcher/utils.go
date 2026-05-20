// Package matcher for matching utilities
package matcher

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/7sDream/geko"
	"github.com/fatih/color"
	jsonDiff "github.com/keploy/jsonDiff"
	"github.com/olekukonko/tablewriter"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

var (
	regexCacheMu sync.RWMutex
	regexCache   = make(map[string]*regexp.Regexp)
)

// getCompiled returns a cached compiled regexp for pattern.
// It never panics: on invalid patterns, it returns a "never matches" regex (?!) and caches it.
func getCompiled(pattern string) *regexp.Regexp {
	regexCacheMu.RLock()
	re := regexCache[pattern]
	regexCacheMu.RUnlock()
	if re != nil {
		return re
	}

	// Compile outside the read lock.
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		// Fallback to a regex that never matches to avoid panics / repeated compiles
		compiled, _ = regexp.Compile(`(?!)`)
	}

	regexCacheMu.Lock()
	// Double-check to avoid races.
	if old := regexCache[pattern]; old == nil {
		regexCache[pattern] = compiled
	} else {
		compiled = old
	}
	regexCacheMu.Unlock()
	return compiled
}

func MatchesAnyRegex(str string, regexArray []string) (bool, string) {
	for _, pattern := range regexArray {
		if getCompiled(pattern).MatchString(str) {
			return true, pattern
		}
	}
	return false, ""
}

type noiseEntry struct {
	keyLower string
	regexps  []*regexp.Regexp // empty => ignore subtree
}
type noiseIndex struct {
	entries []noiseEntry
}

func buildNoiseIndex(mp map[string][]string) noiseIndex {
	if mp == nil {
		return noiseIndex{}
	}
	out := noiseIndex{entries: make([]noiseEntry, 0, len(mp))}
	for k, arr := range mp {
		e := noiseEntry{keyLower: strings.ToLower(k)}
		if len(arr) > 0 {
			e.regexps = make([]*regexp.Regexp, 0, len(arr))
			for _, p := range arr {
				e.regexps = append(e.regexps, getCompiled(p))
			}
		}
		out.entries = append(out.entries, e)
	}
	return out
}

func (ni noiseIndex) match(keyLower string) (regs []*regexp.Regexp, isNoisy bool) {
	for _, e := range ni.entries {
		if strings.Contains(keyLower, e.keyLower) {
			return e.regexps, true
		}
	}
	return nil, false
}

// JSONDiffWithNoiseControl compares JSON with support for both Path-based noise (e.g. "body.user.id")
// and Global noise (e.g. "timestamp") to be ignored everywhere.
func JSONDiffWithNoiseControl(validatedJSON ValidatedJSON, noise map[string][]string, ignoreOrdering bool) (JSONComparisonResult, error) {
	// Split noise into Path-based (contains dots) and Global (no dots)
	pathNoise := make(map[string][]string)
	globalKeys := make(map[string]bool)

	for k, v := range noise {
		// If a key has no dots, treat it as a Global Key to be ignored everywhere.
		if !strings.Contains(k, ".") {
			globalKeys[strings.ToLower(k)] = true
		} else {
			// Otherwise, it's a path-specific noise (e.g. "body.data.timestamp")
			pathNoise[k] = v
		}
	}

	idx := buildNoiseIndex(pathNoise)
	return matchJSONWithNoiseHandlingIndexed("", validatedJSON.expected, validatedJSON.actual, idx, globalKeys, ignoreOrdering)
}

// matchJSONWithNoiseHandlingIndexed now accepts globalKeys to skip specific keys at any depth.
func matchJSONWithNoiseHandlingIndexed(key string, expected, actual interface{}, ni noiseIndex, globalKeys map[string]bool, ignoreOrdering bool) (JSONComparisonResult, error) {
	var out JSONComparisonResult
	// Type check fast-path (JSON unmarshal produces these concrete types).
	switch e := expected.(type) {
	case nil:
		if actual == nil {
			out.matches, out.isExact = true, true
		}
		return out, nil

	case string:
		a, ok := actual.(string)
		if !ok {
			return out, errors.New("type not matched")
		}
		regs, noisy := ni.match(strings.ToLower(key))
		if noisy && len(regs) != 0 {
			if anyRegexpMatchStr(a, regs) {
				out.matches, out.isExact = true, true
				return out, nil
			}
		}
		if e == a || noisy {
			out.matches, out.isExact = true, true
		}
		return out, nil

	case bool:
		a, ok := actual.(bool)
		if !ok {
			return out, errors.New("type not matched")
		}
		_, noisy := ni.match(strings.ToLower(key))
		if e == a || noisy {
			out.matches, out.isExact = true, true
		}
		return out, nil

	case float64:
		a, ok := actual.(float64)
		if !ok {
			return out, errors.New("type not matched")
		}
		_, noisy := ni.match(strings.ToLower(key))
		if e == a || noisy {
			out.matches, out.isExact = true, true
		}
		return out, nil

	case map[string]interface{}:
		a, ok := actual.(map[string]interface{})
		if !ok {
			return out, errors.New("type not matched")
		}

		// If whole subtree is noisy (no regex guard), accept.
		if regs, noisy := ni.match(strings.ToLower(key)); noisy && len(regs) == 0 {
			out.matches, out.isExact = true, true
			return out, nil
		}

		// Quick length check — allows early exit if extra/missing keys (ignoring noisy children).
		// We still need to walk to account for noisy exclusions; so we won't return solely on len.
		isExact := true

		// Lowercased prefix once.
		prefix := ""
		if key != "" {
			prefix = key + "."
		}
		prefixLower := strings.ToLower(prefix)

		// 1) All expected keys must be present & match.
		for k, v := range e {
			if globalKeys[strings.ToLower(k)] {
				continue
			}

			val, ok := a[k]
			if !ok {
				return out, nil
			}
			childKeyLower := prefixLower + strings.ToLower(k)

			// If child subtree is entirely noisy via path, skip deep compare.
			if regs, noisy := ni.match(childKeyLower); noisy && len(regs) == 0 {
				continue
			}

			res, err := matchJSONWithNoiseHandlingIndexed(prefix+k, v, val, ni, globalKeys, ignoreOrdering)
			if err != nil || !res.matches {
				return out, nil
			}
			if !res.isExact {
				isExact = false
				out.differences = append(out.differences, k)
				out.differences = append(out.differences, res.differences...)
			}
		}

		// 2) No unexpected non-noisy keys in actual.
		for k := range a {
			if globalKeys[strings.ToLower(k)] {
				continue
			}

			if _, ok := e[k]; ok {
				continue
			}
			childKeyLower := prefixLower + strings.ToLower(k)
			if regs, noisy := ni.match(childKeyLower); noisy && len(regs) == 0 {
				continue // ignore unexpected but noisy subtree
			}
			return out, nil
		}

		out.matches, out.isExact = true, isExact
		return out, nil

	case []interface{}:
		a, ok := actual.([]interface{})
		if !ok {
			return out, errors.New("type not matched")
		}
		if len(e) != len(a) {
			return out, nil
		}

		// If the whole slice is marked noisy-without-regex, accept.
		if regs, noisy := ni.match(strings.ToLower(key)); noisy && len(regs) == 0 {
			out.matches, out.isExact = true, true
			return out, nil
		}

		// Fast path: if ordering matters, avoid O(n²).
		if !ignoreOrdering {
			isExact := true
			for i := 0; i < len(e); i++ {
				res, err := matchJSONWithNoiseHandlingIndexed(key, e[i], a[i], ni, globalKeys, ignoreOrdering)
				if err != nil || !res.matches {
					return out, nil
				}
				if !res.isExact {
					isExact = false
				}
			}
			out.matches, out.isExact = true, isExact
			return out, nil
		}

		// ignoreOrdering == true: greedy matching with "used" flags to avoid reusing elements.
		used := make([]bool, len(a))
		isExact := true

		for i := 0; i < len(e); i++ {
			matched := false
			// Try primitive fast match first to reduce recursion.
			if j, ok := findAndClaimPrimitiveEqual(e[i], a, used); ok {
				used[j] = true
				matched = true
			} else {
				// Fallback to structural match.
				for j := 0; j < len(a); j++ {
					if used[j] {
						continue
					}
					childKey := key
					res, err := matchJSONWithNoiseHandlingIndexed(childKey, e[i], a[j], ni, globalKeys, ignoreOrdering)
					if err == nil && res.matches {
						if !res.isExact {
							isExact = false
							for _, v := range res.differences {
								if childKey != "" {
									v = childKey + "." + v
								}
								out.differences = append(out.differences, v)
							}
						}
						used[j] = true
						matched = true
						break
					}
				}
			}
			if !matched {
				out.matches, out.isExact = false, false
				return out, nil
			}
		}

		out.matches, out.isExact = true, isExact
		return out, nil
	}

	return out, errors.New("type not registered for json")
}

func anyRegexpMatchStr(s string, regs []*regexp.Regexp) bool {
	for _, re := range regs {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func findAndClaimPrimitiveEqual(x interface{}, arr []interface{}, used []bool) (int, bool) {
	switch v := x.(type) {
	case string:
		for i, y := range arr {
			if used[i] {
				continue
			}
			if ys, ok := y.(string); ok && ys == v {
				return i, true
			}
		}
	case float64:
		for i, y := range arr {
			if used[i] {
				continue
			}
			if yf, ok := y.(float64); ok && yf == v {
				return i, true
			}
		}
	case bool:
		for i, y := range arr {
			if used[i] {
				continue
			}
			if yb, ok := y.(bool); ok && yb == v {
				return i, true
			}
		}
	}
	return -1, false
}

type ValidatedJSON struct {
	expected    interface{} // The expected JSON
	actual      interface{} // The actual JSON
	isIdentical bool
}

func (v *ValidatedJSON) IsIdentical() bool {
	return v.isIdentical
}
func (v *ValidatedJSON) Expected() interface{} {
	return v.expected
}
func (v *ValidatedJSON) Actual() interface{} {
	return v.actual
}

type JSONComparisonResult struct {
	matches     bool     // Indicates if the JSON strings match according to the criteria
	isExact     bool     // Indicates if the match is exact, considering ordering and noise
	differences []string // Lists the keys or indices of values that are not the same
}

func (v *JSONComparisonResult) IsExact() bool {
	return v.isExact
}
func (v *JSONComparisonResult) Matches() bool {
	return v.matches
}
func (v *JSONComparisonResult) Differences() []string {
	return v.differences
}
func MarshalRequestBodies(mockOperation, testOperation *models.Operation) (string, string, error) {
	var mockRequestBody []byte
	var testRequestBody []byte
	var err error
	if mockOperation.RequestBody != nil {
		mockRequestBody, err = json.Marshal(mockOperation.RequestBody.Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling mock RequestBody: %v", err)
		}
	}
	if testOperation.RequestBody != nil {
		testRequestBody, err = json.Marshal(testOperation.RequestBody.Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling test RequestBody: %v", err)
		}
	}
	return string(mockRequestBody), string(testRequestBody), nil
}

func MarshalResponseBodies(status string, mockOperation, testOperation *models.Operation) (string, string, error) {
	var mockResponseBody []byte
	var testResponseBody []byte
	var err error
	if mockOperation.Responses[status].Content != nil {
		mockResponseBody, err = json.Marshal(mockOperation.Responses[status].Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling mock ResponseBody: %v", err)
		}
	}
	if testOperation.Responses[status].Content != nil {
		testResponseBody, err = json.Marshal(testOperation.Responses[status].Content["application/json"].Schema.Properties)
		if err != nil {
			return "", "", fmt.Errorf("error marshalling test ResponseBody: %v", err)
		}
	}
	return string(mockResponseBody), string(testResponseBody), nil
}
func FindOperation(item models.PathItem) (*models.Operation, string) {
	operations := map[string]*models.Operation{
		"GET":    item.Get,
		"POST":   item.Post,
		"PUT":    item.Put,
		"DELETE": item.Delete,
		"PATCH":  item.Patch,
	}

	for method, operation := range operations {
		if operation != nil {
			return operation, method
		}
	}
	return nil, ""
}

// ParseIntoJSON Parse the json string into a geko type variable, it will maintain the order of the keys in the json.
func ParseIntoJSON(response string) (interface{}, error) {
	// Parse the response into a json object.
	if response == "" {
		return nil, nil
	}
	result, err := geko.JSONUnmarshal([]byte(response))
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal the response: %v", err)
	}
	return result, nil
}

// CompareResponses compares response1 (expected) and response2 (actual),
// updating utils.TemplatizedValues and mutating response1 where appropriate.
func CompareResponses(response1, response2 *interface{}, key string) {
	rev := reverseMap(utils.TemplatizedValues) // build once
	compareZip(response1, response2, key, rev)
}

// compareZip walks expected & actual in lock-step, updating expected in place.
func compareZip(exp *interface{}, act *interface{}, key string, rev map[interface{}]string) {
	if exp == nil || act == nil {
		return
	}

	switch ev := (*exp).(type) {

	case geko.ObjectItems:
		// Build index for actual object once
		av, ok := (*act).(geko.ObjectItems)
		if !ok {
			// type mismatch; nothing to normalize here
			return
		}
		actIdx := buildGekoIndex(av)
		ekeys := ev.Keys()
		evals := ev.Values()
		for i := range ekeys {
			k := ekeys[i]
			childKey := k
			// find actual by key
			j, ok := actIdx[k]
			if ok {
				// recurse into pair
				compareZip(&evals[i], &av.List[j].Value, childKey, rev)
				ev.SetValueByIndex(i, evals[i])
			}
		}

	case map[string]interface{}:
		am, ok := (*act).(map[string]interface{})
		if !ok {
			return
		}
		for k, v := range ev {
			av, ok := am[k]
			if ok {
				child := v
				compareZip(&child, &av, k, rev)
				ev[k] = child
			}
		}

	case geko.Array:
		aa, ok := (*act).(geko.Array)
		if !ok {
			return
		}
		// Walk by index. We only normalize where both sides have an element.
		n := len(ev.List)
		if len(aa.List) < n {
			n = len(aa.List)
		}
		for i := 0; i < n; i++ {
			child := ev.List[i]
			compareZip(&child, &aa.List[i], "", rev)
			ev.List[i] = child
		}

	case []interface{}:
		aarr, ok := (*act).([]interface{})
		if !ok {
			return
		}
		n := len(ev)
		if len(aarr) < n {
			n = len(aarr)
		}
		for i := 0; i < n; i++ {
			child := ev[i]
			compareZip(&child, &aarr[i], "", rev)
			ev[i] = child
		}

	case string:
		normalizeLeaf(&ev, act, key, rev)
		*exp = ev

	case float64, int64, int, float32, bool:
		// Convert expected to string for matching with template map,
		// then restore original type if we change it.
		orig := *exp
		s := utils.ToString(ev)
		if normalizeLeaf(&s, act, key, rev) {
			// restore original numeric/bool type if possible
			switch orig.(type) {
			case float64:
				*exp = utils.ToFloat(s)
			case float32:
				f := utils.ToFloat(s)
				*exp = float32(f)
			case int, int64:
				*exp = utils.ToInt(s)
			case bool:
				// utils.ToString(true/false) -> "true"/"false"; try parse back
				if s == "true" || s == "false" {
					*exp = (s == "true")
				} else {
					*exp = s // fallback
				}
			default:
				*exp = s
			}
		}
		// else unchanged

	default:
		// Unknown leaf type; nothing to do.
	}
}

// normalizeLeaf tries to update a single expected leaf (as string) from actual.
// Returns true if it mutated the expected leaf string.
func normalizeLeaf(expStr *string, act *interface{}, key string, rev map[interface{}]string) bool {
	switch av := (*act).(type) {
	case geko.ObjectItems:
		// find same field in actual by key
		if key == "" {
			return false
		}
		idx, ok := buildGekoIndex(av)[key]
		if ok {
			val := av.List[idx].Value
			return assignFromActual(expStr, &val, key, rev)
		}
	case map[string]interface{}:
		if key == "" {
			return false
		}
		v, ok := av[key]
		if ok {
			return assignFromActual(expStr, &v, key, rev)
		}
	case geko.Array:
		// arrays don’t have keyed fields; keep behavior: do nothing here
		return false
	case []interface{}:
		return false
	default:
		// actual itself is a primitive at the same path
		return assignFromActual(expStr, act, key, rev)
	}
	return false
}

// assignFromActual updates expStr (expected) from actual if expStr corresponds
// to a template placeholder in utils.TemplatizedValues (via rev map).
func assignFromActual(expStr *string, act *interface{}, key string, rev map[interface{}]string) bool {
	// direct string
	k, ok := rev[*expStr]
	if ok {
		utils.TemplatizedValues[k] = *act
		*expStr = utils.ToString(*act)
		return true
	}

	// Try integer form
	i, err := strconv.Atoi(*expStr)
	if err == nil {
		k, ok := rev[i]
		if ok {
			utils.TemplatizedValues[k] = *act
			*expStr = utils.ToString(*act)
			return true
		}
	}
	// Try float32
	f, err := strconv.ParseFloat(*expStr, 32)
	if err == nil {
		k, ok := rev[float32(f)]
		if ok {
			utils.TemplatizedValues[k] = *act
			*expStr = utils.ToString(*act)
			return true
		}
	}
	// Try float64
	f64, err := strconv.ParseFloat(*expStr, 64)
	if err == nil {
		k, ok := rev[f64]
		if ok {
			utils.TemplatizedValues[k] = *act
			*expStr = utils.ToString(*act)
			return true
		}
	}
	return false
}

// buildGekoIndex builds key->index for a geko.ObjectItems once.
func buildGekoIndex(obj geko.ObjectItems) map[string]int {
	idx := make(map[string]int, len(obj.List))
	keys := obj.Keys()
	for i := range keys {
		idx[keys[i]] = i
	}
	return idx
}

func reverseMap(m map[string]interface{}) map[interface{}]string {
	var reverseMap = make(map[interface{}]string)
	for key, val := range m {
		reverseMap[val] = key
	}
	return reverseMap
}

func ValidateAndMarshalJSON(log *zap.Logger, exp, act *string) (ValidatedJSON, error) {
	var validatedJSON ValidatedJSON
	var expected interface{}
	var actual interface{}
	var err error
	if *exp != "" {
		expected, err = UnmarshallJSON(*exp, log)
		if err != nil {
			return validatedJSON, err
		}
	}
	if *act != "" {
		actual, err = UnmarshallJSON(*act, log)
		if err != nil {
			return validatedJSON, err
		}
	}
	validatedJSON.expected = expected
	validatedJSON.actual = actual
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		validatedJSON.isIdentical = false
		return validatedJSON, nil
	}
	cleanExp, err := json.Marshal(expected)
	if err != nil {
		return validatedJSON, err
	}
	cleanAct, err := json.Marshal(actual)
	if err != nil {
		return validatedJSON, err
	}
	*exp = string(cleanExp)
	*act = string(cleanAct)
	validatedJSON.isIdentical = true
	return validatedJSON, nil
}

// UnmarshallJSON returns unmarshalled JSON object.
func UnmarshallJSON(s string, log *zap.Logger) (interface{}, error) {
	var result interface{}
	if s == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		utils.LogError(log, err, "cannot convert json string into json object", zap.String("json", s))
		return nil, err
	}
	return result, nil
}

// maxLineLength is chars PER expected/actual string. Can be changed no problem
const maxLineLength = 50

// ansiRegex is compiled at init-time; if compilation fails, falls back to a never-matches regex.
var ansiRegex *regexp.Regexp

func init() {
	re, err := regexp.Compile(`\x1b\[[0-9;]*[a-zA-Z]`)
	if err != nil {
		re, _ = regexp.Compile(`(?!)`)
	}
	ansiRegex = re
}

var ansiResetCode = "\x1b[0m"

type DiffsPrinter struct {
	out                   io.Writer
	testCase              string
	statusExp             string
	statusAct             string
	headerExp             map[string]string
	headerAct             map[string]string
	bodyExp               string
	bodyAct               string
	bodyNoise             map[string][]string
	headNoise             map[string][]string
	hasarrayIndexMismatch bool
	text                  string
	typeExp               string
	typeAct               string
}

func (d *DiffsPrinter) SetHasarrayIndexMismatch(has bool) {
	d.hasarrayIndexMismatch = has
}

func NewDiffsPrinter(testCase string) DiffsPrinter {
	return NewDiffsPrinterOut(os.Stdout, testCase)
}

func NewDiffsPrinterOut(out io.Writer, testCase string) DiffsPrinter {
	if out == nil {
		out = os.Stdout
	}
	return DiffsPrinter{
		out:       out,
		testCase:  testCase,
		headerExp: map[string]string{},
		headerAct: map[string]string{},
		bodyNoise: map[string][]string{},
		headNoise: map[string][]string{},
	}
}

func (d *DiffsPrinter) PushTypeDiff(exp, act string) {
	d.typeExp, d.typeAct = exp, act
}
func (d *DiffsPrinter) PushStatusDiff(exp, act string) {
	d.statusExp, d.statusAct = exp, act
}

func (d *DiffsPrinter) PushFooterDiff(key string) {
	d.hasarrayIndexMismatch = true
	d.text = key
}

func (d *DiffsPrinter) PushHeaderDiff(exp, act, key string, noise map[string][]string) {
	d.headerExp[key], d.headerAct[key], d.headNoise = exp, act, noise
}

func (d *DiffsPrinter) PushBodyDiff(exp, act string, noise map[string][]string) {
	d.bodyExp, d.bodyAct, d.bodyNoise = exp, act, noise
}
func (d *DiffsPrinter) Render() error {
	diffs := []string{}

	// Status (only when actually different)
	if d.statusExp != d.statusAct {
		s := sprintDiff(d.statusExp, d.statusAct, "status")
		if s != "" {
			diffs = append(diffs, s)
		}
	}

	// Headers (skip when empty/identical)
	s := sprintDiffHeader(d.headerExp, d.headerAct)
	if s != "" {
		diffs = append(diffs, s)
	}

	// Body (skip when empty/identical)
	if len(d.bodyExp) != 0 || len(d.bodyAct) != 0 {
		bE, bA := []byte(d.bodyExp), []byte(d.bodyAct)
		if json.Valid(bE) && json.Valid(bA) {
			difference, err := sprintJSONDiff(bE, bA, "body", d.bodyNoise)
			if err != nil {
				difference = sprintDiff(d.bodyExp, d.bodyAct, "body")
			}
			if difference != "" {
				diffs = append(diffs, difference)
			}
		} else {
			// Non-JSON; only show when something exists to show
			if !isBlankPair(d.bodyExp, d.bodyAct) {
				difference := expectActualTableWithColors(d.bodyExp, d.bodyAct, "body", false)
				diffs = append(diffs, difference)
			}
		}
	}

	// If nothing to show and no array-index warning, render nothing.
	if len(diffs) == 0 && !d.hasarrayIndexMismatch {
		return nil
	}

	table := tablewriter.NewWriter(d.out)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{fmt.Sprintf("Diffs %v", d.testCase)})

	if !models.IsAnsiDisabled {
		table.SetHeaderColor(tablewriter.Colors{tablewriter.FgHiRedColor})
	}
	table.SetAlignment(tablewriter.ALIGN_CENTER)

	for _, e := range diffs {
		if strings.TrimSpace(e) != "" {
			table.Append([]string{e})
		}
	}

	if d.hasarrayIndexMismatch {
		startPart := " Expected and actual value"
		var midPartpaint string

		if len(d.text) > 0 {
			if !models.IsAnsiDisabled {
				midPartpaint = color.New(color.FgRed).SprintFunc()(d.text)
			} else {
				midPartpaint = d.text
			}
			startPart += " of "
		}

		initalPart := utils.WarningSign + startPart
		endPaint := " are in different order but have the same objects"

		if !models.IsAnsiDisabled {
			yellowPaint := color.New(color.FgYellow).SprintFunc()
			initalPart = yellowPaint(initalPart)
			endPaint = yellowPaint(endPaint)
		}

		table.SetHeader([]string{initalPart + midPartpaint + endPaint})
		table.SetAlignment(tablewriter.ALIGN_CENTER)
		table.Append([]string{initalPart + midPartpaint + endPaint})
	}

	table.Render()
	return nil
}
func (d *DiffsPrinter) TableWriter(diffs []string) error {

	table := tablewriter.NewWriter(d.out)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{fmt.Sprintf("Diffs %v", d.testCase)})

	if !models.IsAnsiDisabled {
		table.SetHeaderColor(tablewriter.Colors{tablewriter.FgHiRedColor})
	}
	table.SetAlignment(tablewriter.ALIGN_CENTER)

	for _, e := range diffs {
		table.Append([]string{e})
	}
	if d.hasarrayIndexMismatch {
		startPart := " Expected and actual value"
		var midPartpaint string

		if len(d.text) > 0 {
			if !models.IsAnsiDisabled {
				midPartpaint = color.New(color.FgRed).SprintFunc()(d.text)
			} else {
				midPartpaint = d.text
			}
			startPart += " of "
		}

		initalPart := utils.WarningSign + startPart
		endPaint := " are in different order but have the same objects"

		if !models.IsAnsiDisabled {
			yellowPaint := color.New(color.FgYellow).SprintFunc()
			initalPart = yellowPaint(initalPart)
			endPaint = yellowPaint(endPaint)
		}

		table.SetHeader([]string{initalPart + midPartpaint + endPaint})
		table.SetAlignment(tablewriter.ALIGN_CENTER)
		table.Append([]string{initalPart + midPartpaint + endPaint})
	}
	table.Render()
	return nil
}

func (d *DiffsPrinter) RenderAppender() error {
	//Only show difference for the response body
	diffs := []string{}
	pass := true

	if d.typeExp != d.typeAct {
		diffs = append(diffs, sprintDiff(d.typeExp, d.typeAct, "request body type"))
		pass = false
	}
	if !pass {
		err := d.TableWriter(diffs)
		if err != nil {
			return err
		}
		return nil
	}

	if len(d.bodyExp) != 0 || len(d.bodyAct) != 0 {
		pass = false
		bE, bA := []byte(d.bodyExp), []byte(d.bodyAct)
		if json.Valid(bE) && json.Valid(bA) {
			difference, err := sprintJSONDiff(bE, bA, "response", d.bodyNoise)
			if err != nil {
				difference = sprintDiff(d.bodyExp, d.bodyAct, "response")
			}
			diffs = append(diffs, difference)
		} else {
			// If either body is not valid JSON, show expected as red and actual as green
			difference := expectActualTableWithColors(d.bodyExp, d.bodyAct, "response", false)
			diffs = append(diffs, difference)
		}
	}
	if !pass {
		err := d.TableWriter(diffs)
		if err != nil {
			return err
		}

	}

	return nil
}

// SchemaError represents a single schema mismatch
type SchemaError struct {
	Reason   string
	Expected string
	Actual   string
}

// SchemaDiffPrinter for printing schema mismatch errors
type SchemaDiffPrinter struct {
	out      io.Writer
	testCase string
	errors   []SchemaError
}

func NewSchemaDiffPrinter(testCase string) SchemaDiffPrinter {
	return NewSchemaDiffPrinterOut(os.Stdout, testCase)
}

func NewSchemaDiffPrinterOut(out io.Writer, testCase string) SchemaDiffPrinter {
	if out == nil {
		out = os.Stdout
	}
	return SchemaDiffPrinter{
		out:      out,
		testCase: testCase,
		errors:   []SchemaError{},
	}
}

func (s *SchemaDiffPrinter) PushError(reason, expected, actual string) {
	s.errors = append(s.errors, SchemaError{
		Reason:   reason,
		Expected: expected,
		Actual:   actual,
	})
}

func (s *SchemaDiffPrinter) Render() error {
	if len(s.errors) == 0 {
		return nil
	}

	table := tablewriter.NewWriter(s.out)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{"Schema Check Failed", "Expected", "Actual"})

	if !models.IsAnsiDisabled {
		table.SetHeaderColor(
			tablewriter.Colors{tablewriter.FgHiRedColor},
			tablewriter.Colors{tablewriter.FgHiGreenColor},
			tablewriter.Colors{tablewriter.FgHiRedColor},
		)
	}
	table.SetAlignment(tablewriter.ALIGN_LEFT)

	for _, e := range s.errors {
		exp := e.Expected
		act := e.Actual
		reason := e.Reason

		if !models.IsAnsiDisabled {
			// Colorize reason if needed, or keep plain
		}
		table.Append([]string{reason, exp, act})
	}

	fmt.Fprintf(s.out, "\nTestrun failed for testcase with id: %s\n", s.testCase)
	table.Render()
	fmt.Fprintln(s.out) // Add a newline after
	return nil
}

// stripANSI removes ANSI escape sequences for accurate emptiness checks.
func stripANSI(s string) string {
	if s == "" {
		return s
	}
	return ansiRegex.ReplaceAllString(s, "")
}

// isBlankPair returns true when both strings are effectively empty (ignoring ANSI and whitespace).
func isBlankPair(a, b string) bool {
	ta := strings.TrimSpace(stripANSI(a))
	tb := strings.TrimSpace(stripANSI(b))
	return ta == "" && tb == ""
}

/*
 * Returns a nice diff table where the left is the expect and the right
 * is the actual. each entry in expect and actual will contain the key
 * and the corresponding value.
 */
func sprintDiffHeader(expect, actual map[string]string) string {
	diff := jsonDiff.CompareHeaders(expect, actual)

	// If both sides render to nothing, skip the row entirely.
	if isBlankPair(diff.Expected, diff.Actual) {
		return ""
	}

	if len(diff.Expected) > maxLineLength || len(diff.Actual) > maxLineLength {
		return expectActualTable(diff.Expected, diff.Actual, "header", false) // Don't centerize
	}
	return expectActualTable(diff.Expected, diff.Actual, "header", true)
}

/*
 * Returns a nice diff table where the left is the expect and the right
 * is the actual. For JSON-based diffs use SprintJSONDiff
 * field: body, status...
 */
func sprintDiff(expect, actual, field string) string {
	// Fast skip if identical (including ANSI/whitespace-insensitive no-op)
	if expect == actual || isBlankPair(expect, actual) {
		return ""
	}

	diff := jsonDiff.Compare(expect, actual)

	// If the computed diff is blank, also skip.
	if isBlankPair(diff.Expected, diff.Actual) {
		return ""
	}

	if len(expect) > maxLineLength || len(actual) > maxLineLength {
		return expectActualTable(diff.Expected, diff.Actual, field, false)
	}
	return expectActualTable(diff.Expected, diff.Actual, field, true)
}

/* This will return the json diffs in a beautifull way. It will in fact
 * create a colorized table-based expect-response string and return it.
 * on the left-side there'll be the expect and on the right the actual
 * response. Its important to mention the inputs must to be a json. If
 * the body isnt in the rest-api formats (what means it is not json-based)
 * its better to use a generic diff output as the SprintDiff.
 */
func sprintJSONDiff(json1 []byte, json2 []byte, field string, noise map[string][]string) (string, error) {
	diff, err := jsonDiff.CompareJSON(json1, json2, noise, false)
	if err != nil {
		return "", err
	}
	// Skip if both sides render blank
	if isBlankPair(diff.Expected, diff.Actual) {
		return "", nil
	}
	result := expectActualTable(diff.Expected, diff.Actual, field, false)
	return result, nil
}

func wrapTextWithAnsi(input string) string {
	scanner := bufio.NewScanner(strings.NewReader(input)) // Create a scanner to read the input string line by line.
	var wrappedBuilder strings.Builder                    // Builder for the resulting wrapped text.
	currentAnsiCode := ""                                 // Variable to hold the current ANSI escape sequence.
	lastAnsiCode := ""                                    // Variable to hold the last ANSI escape sequence.

	// Iterate over each line in the input string.
	for scanner.Scan() {
		line := scanner.Text() // Get the current line.

		// If there is a current ANSI code, append it to the builder.
		if currentAnsiCode != "" {
			wrappedBuilder.WriteString(currentAnsiCode)
		}

		// Find all ANSI escape sequences in the current line.
		startAnsiCodes := ansiRegex.FindAllString(line, -1)
		if len(startAnsiCodes) > 0 {
			// Update the last ANSI escape sequence to the last one found in the line.
			lastAnsiCode = startAnsiCodes[len(startAnsiCodes)-1]
		}

		// Append the current line to the builder.
		wrappedBuilder.WriteString(line)

		// Check if the current ANSI code needs to be reset or updated.
		if (currentAnsiCode != "" && !strings.HasSuffix(line, ansiResetCode)) || len(startAnsiCodes) > 0 {
			// If the current line does not end with a reset code or if there are ANSI codes, append a reset code.
			wrappedBuilder.WriteString(ansiResetCode)
			// Update the current ANSI code to the last one found in the line.
			currentAnsiCode = lastAnsiCode
		} else {
			// If no ANSI codes need to be maintained, reset the current ANSI code.
			currentAnsiCode = ""
		}

		// Append a newline character to the builder.
		wrappedBuilder.WriteString("\n")
	}

	// Return the processed string with properly wrapped ANSI escape sequences.
	return wrappedBuilder.String()
}

// truncateStrings applies truncation logic to expected and actual strings
// If both exceed 10000 bytes, truncate both to 10000 bytes
// If difference between them is more than 1000 bytes, limit the larger one
func truncateStrings(exp, act string) (string, string) {
	const maxBytes = 10000
	const maxDiff = 1000

	expLen := len(exp)
	actLen := len(act)
	expTruncated := false
	actTruncated := false

	// If both exceed max bytes, truncate both to max bytes
	if expLen > maxBytes && actLen > maxBytes {
		exp = exp[:maxBytes]
		act = act[:maxBytes]
		expTruncated = true
		actTruncated = true
	} else {
		// Calculate the difference
		diff := expLen - actLen
		if diff < 0 {
			diff = -diff // Get absolute difference
		}

		// If difference is more than maxDiff, adjust the larger one
		if diff > maxDiff {
			if expLen > actLen {
				// Expected is larger, truncate it
				newExpLen := actLen + maxDiff
				if newExpLen > maxBytes {
					newExpLen = maxBytes
				}
				if newExpLen < expLen {
					exp = exp[:newExpLen]
					expTruncated = true
				}
			} else {
				// Actual is larger, truncate it
				newActLen := expLen + maxDiff
				if newActLen > maxBytes {
					newActLen = maxBytes
				}
				if newActLen < actLen {
					act = act[:newActLen]
					actTruncated = true
				}
			}
		} else {
			// If either exceeds maxBytes individually, truncate it
			if expLen > maxBytes {
				exp = exp[:maxBytes]
				expTruncated = true
			}
			if actLen > maxBytes {
				act = act[:maxBytes]
				actTruncated = true
			}
		}
	}

	// Add truncation indicators
	if expTruncated {
		exp += "\n...(truncated, original length " + strconv.Itoa(expLen) + " bytes)"
	}
	if actTruncated {
		act += "\n...(truncated, original length " + strconv.Itoa(actLen) + " bytes)"
	}

	return exp, act
}
func expectActualTable(exp string, act string, field string, centerize bool) string {
	buf := &bytes.Buffer{}
	table := tablewriter.NewWriter(buf)

	if centerize {
		table.SetAlignment(tablewriter.ALIGN_CENTER)
	} else {
		table.SetAlignment(tablewriter.ALIGN_LEFT)
	}

	// Apply truncation logic
	exp, act = truncateStrings(exp, act)

	if models.IsAnsiDisabled {
		exp = stripANSI(exp)
		act = stripANSI(act)
	}

	table.SetHeader([]string{fmt.Sprintf("Expect %v", field), fmt.Sprintf("Actual %v", field)})
	table.SetAutoWrapText(false)
	table.SetBorder(false)
	table.SetColMinWidth(0, maxLineLength)
	table.SetColMinWidth(1, maxLineLength)
	table.Append([]string{exp, act})
	table.Render()
	return buf.String()
}

// expectActualTableWithColors creates a table with colored expected (red) and actual (green) values
func expectActualTableWithColors(exp string, act string, field string, centerize bool) string {
	buf := &bytes.Buffer{}
	table := tablewriter.NewWriter(buf)

	if centerize {
		table.SetAlignment(tablewriter.ALIGN_CENTER)
	} else {
		table.SetAlignment(tablewriter.ALIGN_LEFT)
	}

	// Apply truncation logic before processing
	exp, act = truncateStrings(exp, act)

	if models.IsAnsiDisabled {
		exp = stripANSI(exp)
		act = stripANSI(act)
	} else {
		greenPaint := color.New(color.FgGreen)
		greenPaint.EnableColor()
		redPaint := color.New(color.FgRed)
		redPaint.EnableColor()

		exp = redPaint.SprintFunc()(exp)
		act = greenPaint.SprintFunc()(act)
	}

	exp = wrapTextWithAnsi(exp)
	act = wrapTextWithAnsi(act)

	table.SetHeader([]string{fmt.Sprintf("Expect %v", field), fmt.Sprintf("Actual %v", field)})
	table.SetAutoWrapText(false)
	table.SetBorder(false)
	table.SetColMinWidth(0, maxLineLength)
	table.SetColMinWidth(1, maxLineLength)
	table.Append([]string{exp, act})
	table.Render()
	return buf.String()
}
func Contains(elems []string, v string) bool {
	for _, s := range elems {
		if v == s {
			return true
		}
	}
	return false
}

func checkKey(res *[]models.HeaderResult, key string) bool {
	for _, v := range *res {
		if key == v.Expected.Key {
			return false
		}
	}
	return true
}

func CompareHeaders(h1 http.Header, h2 http.Header, res *[]models.HeaderResult, noise map[string][]string) bool {
	if res == nil {
		return false
	}
	match := true
	_, isHeaderNoisy := noise["header"]
	for k, v := range h1 {
		regexArr, isNoisy := SubstringKeyMatch(strings.ToLower(k), noise)
		if isNoisy && len(regexArr) != 0 {
			isNoisy, _ = MatchesAnyRegex(v[0], regexArr)
		}
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h2[k]
		if !isNoisy {
			if !ok {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: nil,
						},
					})
				}

				match = false
				continue
			}
			if len(v) != len(val) {
				if checkKey(res, k) {
					*res = append(*res, models.HeaderResult{
						Normal: false,
						Expected: models.Header{
							Key:   k,
							Value: v,
						},
						Actual: models.Header{
							Key:   k,
							Value: val,
						},
					})
				}
				match = false
				continue
			}
			for i, e := range v {
				if val[i] != e {
					if checkKey(res, k) {
						*res = append(*res, models.HeaderResult{
							Normal: false,
							Expected: models.Header{
								Key:   k,
								Value: v,
							},
							Actual: models.Header{
								Key:   k,
								Value: val,
							},
						})
					}
					match = false
					continue
				}
			}
		}
		if checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: v,
				},
				Actual: models.Header{
					Key:   k,
					Value: val,
				},
			})
		}
	}
	for k, v := range h2 {
		regexArr, isNoisy := SubstringKeyMatch(strings.ToLower(k), noise)
		if isNoisy && len(regexArr) != 0 {
			isNoisy, _ = MatchesAnyRegex(v[0], regexArr)
		}
		isNoisy = isNoisy || isHeaderNoisy
		val, ok := h1[k]
		if isNoisy && checkKey(res, k) {
			*res = append(*res, models.HeaderResult{
				Normal: true,
				Expected: models.Header{
					Key:   k,
					Value: val,
				},
				Actual: models.Header{
					Key:   k,
					Value: v,
				},
			})
			continue
		}
		if !ok {
			if checkKey(res, k) {
				*res = append(*res, models.HeaderResult{
					Normal: false,
					Expected: models.Header{
						Key:   k,
						Value: nil,
					},
					Actual: models.Header{
						Key:   k,
						Value: v,
					},
				})
			}

			match = false
		}
	}
	return match
}

func MapToArray(mp map[string][]string) []string {
	var result []string
	for k := range mp {
		result = append(result, k)
	}
	return result
}

func SubstringKeyMatch(s string, mp map[string][]string) ([]string, bool) {
	for key, val := range mp {
		if strings.Contains(s, key) {
			return val, true
		}
	}
	return []string{}, false
}

// func CheckStringExist(s string, mp map[string][]string) ([]string, bool) {
// 	if val, ok := mp[s]; ok {
// 		return val, ok
// 	}
// 	return []string{}, false
// }

func AddHTTPBodyToMap(body string, m map[string][]string) error {
	// add body
	if json.Valid([]byte(body)) {
		var result interface{}

		err := json.Unmarshal([]byte(body), &result)
		if err != nil {
			return err
		}
		j := Flatten(result)
		for k, v := range j {
			nk := "body"
			if k != "" {
				nk = nk + "." + k
			}
			m[nk] = v
		}
	} else {
		// add it as raw text
		m["body"] = []string{body}
	}
	return nil
}

// Flatten takes a map and returns a new one where nested maps are replaced
// by dot-delimited keys.
// examples of valid jsons - https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/JSON/parse#examples
func Flatten(j interface{}) map[string][]string {
	if j == nil {
		return map[string][]string{"": {""}}
	}
	o := make(map[string][]string)
	x := reflect.ValueOf(j)
	switch x.Kind() {
	case reflect.Map:
		m, ok := j.(map[string]interface{})
		if !ok {
			return map[string][]string{}
		}
		for k, v := range m {
			nm := Flatten(v)
			for nk, nv := range nm {
				fk := k
				if nk != "" {
					fk = fk + "." + nk
				}
				o[fk] = nv
			}
		}
	case reflect.Bool:
		o[""] = []string{strconv.FormatBool(x.Bool())}
	case reflect.Float64:
		o[""] = []string{strconv.FormatFloat(x.Float(), 'E', -1, 64)}
	case reflect.String:
		o[""] = []string{x.String()}
	case reflect.Slice:
		child, ok := j.([]interface{})
		if !ok {
			return map[string][]string{}
		}
		for _, av := range child {
			nm := Flatten(av)
			for nk, nv := range nm {
				if ov, exists := o[nk]; exists {
					o[nk] = append(ov, nv...)
				} else {
					o[nk] = nv
				}
			}
		}
	}
	return o
}

func ArrayToMap(arr []string) map[string]bool {
	res := map[string]bool{}
	for i := range arr {
		res[arr[i]] = true
	}
	return res
}

func InterfaceToString(val interface{}) string {
	switch v := val.(type) {
	case int:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%f", v)
	case bool:
		return fmt.Sprintf("%t", v)
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

func JsonContains(actualJSON string, expectedJSON map[string]interface{}) (bool, error) {
	var actual interface{}
	err := json.Unmarshal([]byte(actualJSON), &actual)
	if err != nil {
		return false, fmt.Errorf("failed to unmarshal actual JSON: %v", err)
	}

	return containsRecursive(actual, expectedJSON), nil
}

// containsRecursive recursively checks if the expected data is in the actual data.
func containsRecursive(actual interface{}, expected map[string]interface{}) bool {
	actualMap, ok := actual.(map[string]interface{})
	if !ok {
		return false
	}
	for key, expectedValue := range expected {
		actualValue, exists := actualMap[key]
		if !exists {
			return false
		}

		switch v := expectedValue.(type) {
		case map[string]interface{}:
			if actualMapVal, ok := actualValue.(map[string]interface{}); ok {
				if !containsRecursive(actualMapVal, v) {
					return false
				}
			} else {
				return false
			}
		default:
			if !reflect.DeepEqual(actualValue, expectedValue) {
				return false
			}
		}
	}
	return true
}

// lowerMap returns a lower-cased copy of parameter keys.
func lowerMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

// parseContentType tries strict parsing, then a tolerant fallback.
// okStrict indicates whether strict parsing succeeded.
func ParseContentType(raw string) (typ string, params map[string]string, okStrict bool, err error) {
	if raw == "" {
		return "", nil, false, nil
	}
	t, p, e := mime.ParseMediaType(raw)
	if e == nil {
		return strings.ToLower(t), lowerMap(p), true, nil
	}
	// tolerant fallback: take token before ';', trim, lowercase
	token := strings.ToLower(strings.TrimSpace(strings.Split(raw, ";")[0]))
	if token == "" || !strings.Contains(token, "/") {
		// no usable fallback
		return "", nil, false, e
	}
	return token, map[string]string{}, false, e
}

// compareSlicesIgnoreOrder checks if two string slices contain the same elements,
// regardless of their order. It returns true if they do, and false otherwise.
func CompareSlicesIgnoreOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	// Create a frequency map of elements in the first slice.
	freq := make(map[string]int, len(a))
	for _, item := range a {
		freq[item]++
	}

	// Decrement the frequency for each element in the second slice.
	for _, item := range b {
		if freq[item] == 0 {
			// If an element is not in the map or its count is already zero,
			// the slices are not identical.
			return false
		}
		freq[item]--
	}

	// If the loop completes, the slices have the same elements.
	return true
}

// NormalizeNestedJSONForNoise rewrites a JSON string so that any string fields
// which themselves contain JSON are parsed into real JSON objects/arrays.
// It only does this for fields that are hinted by the noise configuration,
// i.e. when noise keys contain dotted paths like "json_response.timestamp".
//
// This lets noise entries such as "json_response.timestamp" work even when
// the original payload encodes that inner JSON as a string.
//
// If raw is not JSON, noise is empty, or any error occurs, it returns raw
// unchanged so it is safe to call in all conditions.
func NormalizeNestedJSONForNoise(raw string, noise map[string][]string, log *zap.Logger) string {
	if raw == "" || len(noise) == 0 {
		return raw
	}
	// Fast rejection: not JSON at all → nothing to normalize
	if !json.Valid([]byte(raw)) {
		return raw
	}

	// Collect "root" keys from noise that indicate nested paths.
	// Example: "json_response.timestamp" → root "json_response".
	rootKeys := make(map[string]bool)
	for k := range noise {
		parts := strings.SplitN(k, ".", 2)
		if len(parts) > 1 {
			root := strings.ToLower(parts[0])
			rootKeys[root] = true
		}
	}
	if len(rootKeys) == 0 {
		// No dotted paths → no need to rewrite nested JSON
		return raw
	}

	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		utils.LogError(log, err, "failed to unmarshal decoded gRPC data for normalization")
		return raw
	}

	normalizeNestedJSONValue(v, rootKeys)

	buf, err := json.Marshal(v)
	if err != nil {
		utils.LogError(log, err, "failed to re-marshal decoded gRPC data after normalization")
		return raw
	}
	return string(buf)
}

// normalizeNestedJSONValue walks the parsed JSON value and replaces any
// string fields whose key is in rootKeys and whose value is itself valid
// JSON with the parsed inner JSON.
func normalizeNestedJSONValue(v interface{}, rootKeys map[string]bool) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			lowerKey := strings.ToLower(k)

			// If this key is in rootKeys and the value is a JSON string, unwrap it.
			if rootKeys[lowerKey] {
				if s, ok := val.(string); ok && pkg.LooksLikeJSON(s) && json.Valid([]byte(s)) {
					var inner interface{}
					if err := json.Unmarshal([]byte(s), &inner); err == nil {
						t[k] = inner
						val = inner
					}
				}
			}

			// Recurse into children to handle deeper nested wrappers.
			normalizeNestedJSONValue(val, rootKeys)
		}

	case []interface{}:
		for i := range t {
			normalizeNestedJSONValue(t[i], rootKeys)
		}
	}
}

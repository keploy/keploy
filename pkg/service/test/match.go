package test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"go.uber.org/zap"
)

// unmarshallJson returns unmarshalled JSON object.
func UnmarshallJson(s string, log *zap.Logger) (interface{}, error) {
	var result interface{}
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		log.Error("cannot convert json string into json object", zap.Error(err))
		return nil, err
	} else {
		return result, nil
	}
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

func JsonDiffWithNoiseControl(log *zap.Logger, validatedJSON validatedJSON, noise map[string][]string, ignoreOrdering bool) (jsonComparisonResult, error) {
	var matchJsonComparisonResult jsonComparisonResult
	matchJsonComparisonResult, err := matchJsonWithNoiseHandling("", validatedJSON.expected, validatedJSON.actual, noise, ignoreOrdering)
	if err != nil {
		return matchJsonComparisonResult, err
	}

	return matchJsonComparisonResult, nil
}

func ValidateAndMarshalJson(log *zap.Logger, exp, act *string) (validatedJSON, error) {
	var validatedJSON validatedJSON
	expected, err := UnmarshallJson(*exp, log)
	if err != nil {
		return validatedJSON, err
	}
	actual, err := UnmarshallJson(*act, log)
	if err != nil {
		return validatedJSON, err
	}
	validatedJSON.expected = expected
	validatedJSON.actual = actual
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
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

	return validatedJSON, nil
}

// matchJsonWithNoiseHandling returns strcut if expected and actual JSON objects matches(are equal) and in exact order(isExact).
func matchJsonWithNoiseHandling(key string, expected, actual interface{}, noiseMap map[string][]string, ignoreOrdering bool) (jsonComparisonResult, error) {
	var matchJsonComparisonResult jsonComparisonResult
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return matchJsonComparisonResult, typeNotMatch
	}
	if expected == nil && actual == nil {
		matchJsonComparisonResult.isExact = true
		matchJsonComparisonResult.matches = true
		return matchJsonComparisonResult, nil
	}
	x := reflect.ValueOf(expected)
	prefix := ""
	if key != "" {
		prefix = key + "."
	}
	switch x.Kind() {
	case reflect.Float64, reflect.String, reflect.Bool:
		regexArr, isNoisy := CheckStringExist(key, noiseMap)
		if isNoisy && len(regexArr) != 0 {
			isNoisy, _ = MatchesAnyRegex(InterfaceToString(expected), regexArr)
		}
		if expected != actual && !isNoisy {
			return matchJsonComparisonResult, nil
		}

	case reflect.Map:
		expMap := expected.(map[string]interface{})
		actMap := actual.(map[string]interface{})
		copiedExpMap := make(map[string]interface{})
		copiedActMap := make(map[string]interface{})

		// Copy each key-value pair from expMap to copiedExpMap
		for key, value := range expMap {
			copiedExpMap[key] = value
		}

		// Repeat the same process for actual map
		for key, value := range actMap {
			copiedActMap[key] = value
		}
		isExact := true
		differences := []string{}
		for k, v := range expMap {
			val, ok := actMap[k]
			if !ok {
				return matchJsonComparisonResult, nil
			}
			if valueMatchJsonComparisonResult, er := matchJsonWithNoiseHandling(prefix+k, v, val, noiseMap, ignoreOrdering); !valueMatchJsonComparisonResult.matches || er != nil {
				return valueMatchJsonComparisonResult, nil
			} else if !valueMatchJsonComparisonResult.isExact {
				isExact = false
				differences = append(differences, k)
				differences = append(differences, valueMatchJsonComparisonResult.differences...)
			}
			// remove the noisy key from both expected and actual JSON.
			if _, ok := CheckStringExist(prefix+k, noiseMap); ok {
				delete(copiedExpMap, prefix+k)
				delete(copiedActMap, k)
				continue
			}
		}
		// checks if there is a key which is not present in expMap but present in actMap.
		for k := range actMap {
			_, ok := expMap[k]
			if !ok {
				return matchJsonComparisonResult, nil
			}
		}
		matchJsonComparisonResult.matches = true
		matchJsonComparisonResult.isExact = isExact
		matchJsonComparisonResult.differences = append(matchJsonComparisonResult.differences, differences...)
		return matchJsonComparisonResult, nil
	case reflect.Slice:
		if regexArr, isNoisy := CheckStringExist(key, noiseMap); isNoisy && len(regexArr) != 0 {
			break
		}
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)
		if expSlice.Len() != actSlice.Len() {
			return matchJsonComparisonResult, nil
		}
		isMatched := true
		isExact := true
		for i := 0; i < expSlice.Len(); i++ {
			matched := false
			for j := 0; j < actSlice.Len(); j++ {
				if valMatchJsonComparisonResult, err := matchJsonWithNoiseHandling(key, expSlice.Index(i).Interface(), actSlice.Index(j).Interface(), noiseMap, ignoreOrdering); err == nil && valMatchJsonComparisonResult.matches {
					if !valMatchJsonComparisonResult.isExact {
						for _, val := range valMatchJsonComparisonResult.differences {
							prefixedVal := key + "[" + fmt.Sprint(j) + "]." + val // Prefix the value
							matchJsonComparisonResult.differences = append(matchJsonComparisonResult.differences, prefixedVal)
						}
					}
					matched = true
					break
				}
			}

			if !matched {
				isMatched = false
				isExact = false
				break
			}
		}
		if !isMatched {
			matchJsonComparisonResult.matches = isMatched
			matchJsonComparisonResult.isExact = isExact
			return matchJsonComparisonResult, nil
		}
		if !ignoreOrdering {
			for i := 0; i < expSlice.Len(); i++ {
				if valMatchJsonComparisonResult, er := matchJsonWithNoiseHandling(key, expSlice.Index(i).Interface(), actSlice.Index(i).Interface(), noiseMap, ignoreOrdering); er != nil || !valMatchJsonComparisonResult.isExact {
					isExact = false
					break
				}
			}
		}
		matchJsonComparisonResult.matches = isMatched
		matchJsonComparisonResult.isExact = isExact

		return matchJsonComparisonResult, nil
	default:
		return matchJsonComparisonResult, errors.New("type not registered for json")
	}
	matchJsonComparisonResult.matches = true
	matchJsonComparisonResult.isExact = true
	return matchJsonComparisonResult, nil
}

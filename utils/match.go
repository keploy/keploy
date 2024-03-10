package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"

	"go.uber.org/zap"
)

type validatedJSON struct {
	expected    interface{} // The expected JSON
	actual      interface{} // The actual JSON
	IsIdentical bool
}

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

func JsonDiffWithNoiseControl(log *zap.Logger, validatedJSON validatedJSON, noise map[string][]string, ignoreOrdering bool) (JsonComparisonResult, error) {
	var matchJsonComparisonResult JsonComparisonResult
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
		validatedJSON.IsIdentical = false
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
	validatedJSON.IsIdentical = true
	return validatedJSON, nil
}

// matchJsonWithNoiseHandling returns strcut if expected and actual JSON objects matches(are equal) and in exact order(isExact).
func matchJsonWithNoiseHandling(key string, expected, actual interface{}, noiseMap map[string][]string, ignoreOrdering bool) (JsonComparisonResult, error) {
	var matchJsonComparisonResult JsonComparisonResult
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return matchJsonComparisonResult, errors.New("type not matched")
	}
	if expected == nil && actual == nil {
		matchJsonComparisonResult.IsExact = true
		matchJsonComparisonResult.Matches = true
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
			if valueMatchJsonComparisonResult, er := matchJsonWithNoiseHandling(prefix+k, v, val, noiseMap, ignoreOrdering); !valueMatchJsonComparisonResult.Matches || er != nil {
				return valueMatchJsonComparisonResult, nil
			} else if !valueMatchJsonComparisonResult.IsExact {
				isExact = false
				differences = append(differences, k)
				differences = append(differences, valueMatchJsonComparisonResult.Differences...)
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
		matchJsonComparisonResult.Matches = true
		matchJsonComparisonResult.IsExact = isExact
		matchJsonComparisonResult.Differences = append(matchJsonComparisonResult.Differences, differences...)
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
				if valMatchJsonComparisonResult, err := matchJsonWithNoiseHandling(key, expSlice.Index(i).Interface(), actSlice.Index(j).Interface(), noiseMap, ignoreOrdering); err == nil && valMatchJsonComparisonResult.Matches {
					if !valMatchJsonComparisonResult.IsExact {
						for _, val := range valMatchJsonComparisonResult.Differences {
							prefixedVal := key + "[" + fmt.Sprint(j) + "]." + val // Prefix the value
							matchJsonComparisonResult.Differences = append(matchJsonComparisonResult.Differences, prefixedVal)
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
			matchJsonComparisonResult.Matches = isMatched
			matchJsonComparisonResult.IsExact = isExact
			return matchJsonComparisonResult, nil
		}
		if !ignoreOrdering {
			for i := 0; i < expSlice.Len(); i++ {
				if valMatchJsonComparisonResult, er := matchJsonWithNoiseHandling(key, expSlice.Index(i).Interface(), actSlice.Index(i).Interface(), noiseMap, ignoreOrdering); er != nil || !valMatchJsonComparisonResult.IsExact {
					isExact = false
					break
				}
			}
		}
		matchJsonComparisonResult.Matches = isMatched
		matchJsonComparisonResult.IsExact = isExact

		return matchJsonComparisonResult, nil
	default:
		return matchJsonComparisonResult, errors.New("type not registered for json")
	}
	matchJsonComparisonResult.Matches = true
	matchJsonComparisonResult.IsExact = true
	return matchJsonComparisonResult, nil
}

func MatchesAnyRegex(str string, regexArray []string) (bool, string) {
	for _, pattern := range regexArray {
		re := regexp.MustCompile(pattern)
		if re.MatchString(str) {
			return true, pattern
		}
	}
	return false, ""
}


func CheckStringExist(s string, mp map[string][]string) ([]string, bool) {
	if val, ok := mp[s]; ok {
		return val, ok
	}
	ok, val := MatchesAnyRegex(s, MapToArray(mp))
	if ok {
		return mp[val], ok
	}
	return []string{}, false
}
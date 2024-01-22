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

func Match(exp, act string, noise map[string][]string, log *zap.Logger, ignoreOrdering bool) (string, string, bool, bool, error) {
	expected, err := UnmarshallJson(exp, log)
	if err != nil {
		return exp, act, false, false, err
	}
	actual, err := UnmarshallJson(act, log)
	if err != nil {
		return exp, act, false, false, err
	}
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return exp, act, false, false, nil
	}
	match, isSame, err := jsonMatch("", expected, actual, noise, ignoreOrdering)
	if err != nil {
		return exp, act, false, false, err
	}
	cleanExp, err := json.Marshal(expected)
	if err != nil {
		return exp, act, false, false, err
	}
	cleanAct, err := json.Marshal(actual)
	if err != nil {
		return exp, act, false, false, err
	}

	return string(cleanExp), string(cleanAct), match, isSame, nil
}

// jsonMatch returns true if expected and actual JSON objects matches(are equal).
func jsonMatch(key string, expected, actual interface{}, noiseMap map[string][]string, ignoreOrdering bool) (bool, bool, error) {

	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return false, false, errors.New("type not matched ")
	}
	if expected == nil && actual == nil {
		return true, false, nil
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
			return false, false, nil
		}

	case reflect.Map:
		expMap := expected.(map[string]interface{})
		actMap := actual.(map[string]interface{})
		for k, v := range expMap {
			val, ok := actMap[k]
			if !ok {
				return false, false, nil
			}
			if x, _, er := jsonMatch(prefix+k, v, val, noiseMap, ignoreOrdering); !x || er != nil {
				return false, false, nil
			}
			// remove the noisy key from both expected and actual JSON.
			if _, ok := CheckStringExist(prefix+k, noiseMap); ok {
				delete(expMap, prefix+k)
				delete(actMap, k)
				continue
			}
		}
		// checks if there is a key which is not present in expMap but present in actMap.
		for k := range actMap {
			_, ok := expMap[k]
			if !ok {
				return false, false, nil
			}
		}

	case reflect.Slice:
		if regexArr, isNoisy := CheckStringExist(key, noiseMap); isNoisy && len(regexArr) != 0 {
			break
		}
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)
		if expSlice.Len() != actSlice.Len() {
			return false, false, nil
		}
		isMatched := true
		isSame := true
		for i := 0; i < expSlice.Len(); i++ {
			matched := false
			for j := 0; j < actSlice.Len(); j++ {
				if x, _, err := jsonMatch(key, expSlice.Index(i).Interface(), actSlice.Index(j).Interface(), noiseMap, ignoreOrdering); err == nil && x {
					matched = true
					break
				}
			}

			if !matched {
				isMatched = false
				isSame = false
				break
			}
		}
		if !isMatched {
			return isMatched, isSame, nil
		}
		if !ignoreOrdering {
			for i := 0; i < expSlice.Len(); i++ {
				if x, _, err := jsonMatch(key, expSlice.Index(i).Interface(), actSlice.Index(i).Interface(), noiseMap, ignoreOrdering); err == nil && !x {
					isMatched = false
					break
				}
			}
		}
		return isMatched, isSame, nil
	default:
		return false, false, errors.New("type not registered for json")
	}
	return true, true, nil

}

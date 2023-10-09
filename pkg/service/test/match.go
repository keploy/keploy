package test

import (
	"encoding/json"
	"errors"
	"reflect"

	"go.uber.org/zap"
)

// unmarshallJson returns unmarshalled JSON object.
func unmarshallJson(s string, log *zap.Logger) (interface{}, error) {
	var result interface{}
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		log.Error("cannot convert json string into json object", zap.Error(err))
		return nil, err
	} else {
		return result, nil
	}
}

func arrayToMap(arr []string) map[string]bool {
	res := map[string]bool{}
	for i := range arr {
		res[arr[i]] = true
	}
	return res
}

func Match(exp, act string, noise []string, log *zap.Logger) (string, string, bool, error) {

	noiseMap := arrayToMap(noise)
	expected, err := unmarshallJson(exp, log)
	if err != nil {
		return exp, act, false, err
	}
	actual, err := unmarshallJson(act, log)
	if err != nil {
		return exp, act, false, err
	}
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return exp, act, false, nil
	}
	match, err := jsonMatch("", expected, actual, noiseMap)
	if err != nil {
		return exp, act, false, err
	}
	cleanExp, err := json.Marshal(expected)
	if err != nil {
		return exp, act, false, err
	}
	cleanAct, err := json.Marshal(actual)
	if err != nil {
		return exp, act, false, err
	}
	return string(cleanExp), string(cleanAct), match, nil
}

// jsonMatch returns true if expected and actual JSON objects matches(are equal).
func jsonMatch(key string, expected, actual interface{}, noiseMap map[string]bool) (bool, error) {

	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return false, errors.New("type not matched ")
	}
	if expected == nil && actual == nil {
		return true, nil
	}
	x := reflect.ValueOf(expected)
	prefix := ""
	if key != "" {
		prefix = key + "."
	}
	switch x.Kind() {
	case reflect.Float64, reflect.String, reflect.Bool:
		if expected != actual && !noiseMap[key] {
			return false, nil
		}

	case reflect.Map:
		expMap := expected.(map[string]interface{})
		actMap := actual.(map[string]interface{})
		for k, v := range expMap {
			val, ok := actMap[k]
			if !ok {
				return false, nil
			}
			if x, er := jsonMatch(prefix+k, v, val, noiseMap); !x || er != nil {
				return false, nil
			}
			// remove the noisy key from both expected and actual JSON.
			if noiseMap[prefix+k] {
				delete(expMap, prefix+k)
				delete(actMap, k)
				continue
			}
		}
		// checks if there is a key which is not present in expMap but present in actMap.
		for k := range actMap {
			_, ok := expMap[k]
			if !ok {
				return false, nil
			}
		}

	case reflect.Slice:
		if noiseMap[key] {
			return true, nil
		}
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)
		if expSlice.Len() != actSlice.Len() {
			return false, nil
		}
		isMatched := true
		for i := 0; i < expSlice.Len(); i++ {

			isMatchedElement := false
			for j := 0; j < actSlice.Len(); j++ {
				if x, err := jsonMatch(key, expSlice.Index(i).Interface(), actSlice.Index(j).Interface(), noiseMap); err == nil && x {
					isMatchedElement = true
					break
				}
			}
			isMatched = isMatchedElement && isMatched

		}
		return isMatched, nil
	default:
		return false, errors.New("type not registered for json")
	}
	return true, nil

}

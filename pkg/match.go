package pkg

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"go.uber.org/zap"
)

// mapClone returns a copy of given src map.
func mapClone(src map[string][]string) map[string][]string {
	clone := make(map[string][]string, len(src))
	for k, v := range src {
		clone[k] = v
	}
	return clone
}

// convertJson returns unmarshalled JSON object.
func convertJson(s string, log *zap.Logger) (interface{}, error) {
	var result interface{}

	if err := json.Unmarshal([]byte(s), &result); err != nil {
		log.Error("cannot convert json string into json object", zap.Error(err))
		return nil, err
	} else {
		return result, nil
	}
}

func Match(exp, act string, noise []string, log *zap.Logger) (bool, error) {
	noiseMap := convertToMap(noise)
	expected, err := convertJson(exp, log)
	if err != nil {
		return false, err
	}
	actual, err := convertJson(act, log)
	if err != nil {
		return false, err
	}
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return false, nil
	}
	tmp := mapClone(noiseMap)
	expected = removeNoisy(expected, tmp)

	tmp = mapClone(noiseMap)
	actual = removeNoisy(actual, tmp)
	return jsonMatch(expected, actual)
}

// removeNoisy removes the noisy key-value fields(storend in noise map) from given element JSON. It is a recursive function.
func removeNoisy(element interface{}, noise map[string][]string) interface{} {
	
	y := reflect.ValueOf(element)
	switch y.Kind() {
	case reflect.Map:
		el := element.(map[string]interface{})
		for k, v := range noise {
			key := k
			seperatorIndx := strings.IndexByte(k, '.')
			// set key string to k[0: (indx of ".")+1] in order to check if there exists a key in
			// element JSON.
			if seperatorIndx != -1 {
				key = k[:seperatorIndx]
			}
			val, ok := el[key]
			if ok {
				// reached the noisy field and it should be deleted.
				if len(v) == 0 {
					delete(el, k)
					delete(noise, k)
					continue
				}
				// update key of noisy to match heirarchy of noisy field.
				strArr := noise[k][1:]
				delete(noise, k)
				if seperatorIndx != -1 {
					noise[k[seperatorIndx+1:]] = strArr
				}
				el[key] = removeNoisy(val, noise)
			}
		}
		return el
	case reflect.Slice:
		x := reflect.ValueOf(element)
		var res []interface{}
		// remove noisy fields from every array element.
		for i := 0; i < x.Len(); i++ {
			tmp := mapClone(noise)
			res = append(res, removeNoisy(x.Index(i).Interface(), tmp))
		}
		return res
	default:
		return element
	}
}

// convertToMap converts array of string into map with key as str(string element of given array)
// and value as array of string formed by seperating str into substrings (using "." as seperator).
func convertToMap(arr []string) map[string][]string {
	res := map[string][]string{}
	for i := range arr {
		x := strings.Split(arr[i], ".")
		res[arr[i]] = x[1:]
	}
	return res
}

// jsonMatch returns true if expected and actual JSON objects matches(are equal).
func jsonMatch(expected, actual interface{}) (bool, error) {
	if reflect.TypeOf(expected) != reflect.TypeOf(actual) {
		return false, errors.New("type not matched ")
	}
	if expected == nil && actual == nil {
		return true, nil
	}
	x := reflect.ValueOf(expected)
	switch x.Kind() {
	case reflect.Float64:
		if expected != actual {
			return false, nil
		}

	case reflect.String:
		if expected != actual {
			return false, nil
		}
	case reflect.Bool:
		if expected != actual {
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
			if x, er := jsonMatch(v, val); !x || er != nil {
				return false, nil
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
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)
		if expSlice.Len() != actSlice.Len() {
			return false, nil
		}
		isMatched := true
		for i := 0; i < expSlice.Len(); i++ {

			isMatchedElement := false
			for j := 0; j< actSlice.Len() ;j++{
				if x, err := jsonMatch(expSlice.Index(i).Interface(), actSlice.Index(j).Interface()); err == nil && x {
					isMatchedElement = true
					break
				}
			}
			isMatched = isMatchedElement && isMatched
			// if x, err := jsonMatch(expSlice.Index(i).Interface(), actSlice.Index(i).Interface()); err != nil || !x {
			// 	return false, nil
			// }

		}
		return isMatched, nil
	default:
		return false, errors.New("type not registered for json")
	}
	return true, nil

}

package pkg

import (
	"encoding/json"
	"errors"
	"go.uber.org/zap"
	"reflect"
	"strings"
)

func mapClone(src map[string][]string) map[string][]string {
	clone := make(map[string][]string, len(src))
	for k, v := range src {
		clone[k] = v
	}
	return clone
}

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
	return jMatch(expected, actual)
}

func removeNoisy(element interface{}, noise map[string][]string) interface{} {
	y := reflect.ValueOf(element)
	switch y.Kind() {
	case reflect.Map:
		el := element.(map[string]interface{})
		for k, v := range noise {
			str := k
			if strings.IndexByte(k, '.') != -1 {
				str = k[:strings.IndexByte(k, '.')]
			}
			val, ok := el[str]
			if ok {
				if len(v) == 0 {
					delete(el, k)
					delete(noise, k)
					continue
				}
				strArr := noise[k][1:]
				delete(noise, k)
				if strings.IndexByte(k, '.') != -1 {
					noise[k[strings.IndexByte(k, '.')+1:]] = strArr
				}
				el[str] = removeNoisy(val, noise)
			}
		}
		return el
	case reflect.Slice:
		x := reflect.ValueOf(element)
		var res []interface{}
		for i := 0; i < x.Len(); i++ {
			tmp := mapClone(noise)
			res = append(res, removeNoisy(x.Index(i).Interface(), tmp))
		}
		return res
	default:
		return element
	}
}

func convertToMap(arr []string) map[string][]string {
	res := map[string][]string{}
	for i := range arr {
		x := strings.Split(arr[i], ".")
		res[arr[i]] = x[1:]
	}
	return res
}

func jMatch(expected, actual interface{}) (bool, error) {
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
			if x, er := jMatch(v, val); !x || er != nil {
				return false, nil
			}

		}

	case reflect.Slice:
		expSlice := reflect.ValueOf(expected)
		actSlice := reflect.ValueOf(actual)
		for i := 0; i < expSlice.Len(); i++ {

			if x, err := jMatch(expSlice.Index(i).Interface(), actSlice.Index(i).Interface()); err != nil || !x {
				return false, nil
			}

		}
	default:
		return false, errors.New("type not registered for json")
	}
	return true, nil

}

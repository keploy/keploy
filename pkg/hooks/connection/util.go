package connection

import (
	"fmt"
	"reflect"
	"sort"

	"go.uber.org/zap"
)

// find the noisy labels in the given data
func findNoisyLabels(data1, data2 interface{}, logger *zap.Logger) []string {
	switch d1 := data1.(type) {
	case map[string]interface{}:
		if d2, ok := data2.(map[string]interface{}); ok {
			return findNoisyLabelsUtil(d1, d2, "")
		}
		logger.Error("responses are of different types")
		return []string{""}

	case []interface{}:
		if d2, ok := data2.([]interface{}); ok {
			res := compareSlices(d1, d2)
			if res {
				return []string{}
			}
			return []string{""}
		}
		logger.Error("responses are of different types")
		return []string{""}
	default:
		if reflect.DeepEqual(data1, data2) {
			return []string{}
		}
		return []string{""}
	}
}

// check the noisy labels if the datatype is map
func findNoisyLabelsUtil(map1, map2 map[string]interface{}, curPrefix string) []string {
	diffKeys := make([]string, 0)

	for key, val1 := range map1 {
		prefix := curPrefix
		if val2, ok := map2[key]; ok {
			switch v1 := val1.(type) {
			case map[string]interface{}:
				if v2, ok := val2.(map[string]interface{}); ok {
					prefix = prefix + "." + key
					subDiffKeys := findNoisyLabelsUtil(v1, v2, prefix)
					diffKeys = append(diffKeys, subDiffKeys...)
				}
			case []interface{}:
				if v2, ok := val2.([]interface{}); ok {
					if !compareSlices(v1, v2) {
						diffKeys = append(diffKeys, prefix+"."+key)
					}
				} else {
					diffKeys = append(diffKeys, prefix+"."+key)
				}
			default:
				if !reflect.DeepEqual(val1, val2) {
					diffKeys = append(diffKeys, prefix+"."+key)
				}
			}
		} else {
			diffKeys = append(diffKeys, prefix+"."+key)
		}
	}

	return diffKeys
}

// check if the slices are equal or not
func compareSlices(slice1, slice2 []interface{}) bool {
	if len(slice1) != len(slice2) {
		return false
	}

	sort.Slice(slice1, func(i, j int) bool {
		return fmt.Sprintf("%v", slice1[i]) < fmt.Sprintf("%v", slice1[j])
	})
	sort.Slice(slice2, func(i, j int) bool {
		return fmt.Sprintf("%v", slice2[i]) < fmt.Sprintf("%v", slice2[j])
	})

	return reflect.DeepEqual(slice1, slice2)
}

package http

import (
	"encoding/json"
	"fmt"
	"strconv"

	"strings"

	"go.keploy.io/server/v2/pkg/models"
)

func toInt(v interface{}) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case float64:
		return int(x), nil
	case json.Number:
		i64, err := x.Int64()
		return int(i64), err
	case string:
		i64, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0, err
		}
		const maxInt = int64(^uint(0) >> 1) // Maximum value for int
		const minInt = -maxInt - 1          // Minimum value for int
		if i64 > maxInt || i64 < minInt {
			return 0, fmt.Errorf("value out of range for int: %d", i64)
		}
		return int(i64), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}

func toString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func toStringSlice(v interface{}) []string {
	var out []string
	switch x := v.(type) {
	case []interface{}:
		for _, e := range x {
			out = append(out, toString(e))
		}
	case string:
		for _, part := range strings.Split(x, ",") {
			out = append(out, strings.TrimSpace(part))
		}
	}
	return out
}

func toStringMap(val interface{}) map[string]string {
	out := make(map[string]string)
	switch m := val.(type) {
	case map[string]interface{}:
		for k, v := range m {
			out[k] = fmt.Sprint(v)
		}

	case map[string]string:
		// already the right shape
		for k, v := range m {
			out[k] = v
		}

	case map[models.AssertionType]interface{}:
		for kType, v := range m {
			out[string(kType)] = fmt.Sprint(v)
		}

	case map[models.AssertionType]string:
		for kType, v := range m {
			out[string(kType)] = v
		}

	case map[interface{}]interface{}:
		// sometimes YAML v3 gives you this
		for ki, vi := range m {
			key := fmt.Sprint(ki)
			out[key] = fmt.Sprint(vi)
		}

	default:
		// not a map we know—return empty
	}
	return out
}

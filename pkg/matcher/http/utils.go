package http

import (
    "encoding/json"
    "fmt"
    "strconv"
    "strings"
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
        return int(i64), err
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

func toStringMap(v interface{}) map[string]string {
    out := make(map[string]string)
    switch m := v.(type) {
    case map[string]interface{}:
        for k, iv := range m {
            out[k] = toString(iv)
        }
    case map[string]string:
        for k, s := range m {
            out[k] = s
        }
    }
    return out
}
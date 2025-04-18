//go:build linux

package redisv2

import (
	"bytes"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

// SerializeRedisBodyType emits the RESP3 encoding for a single RedisBodyType.
func SerializeRedisBodyType(b models.RedisBodyType) ([]byte, error) {
	var buf bytes.Buffer

	switch b.Type {
	case "string":
		s, ok := b.Data.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", b.Data)
		}
		buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s))

	case "integer":
		// accept int64, int, or float64
		var i64 int64
		switch v := b.Data.(type) {
		case int64:
			i64 = v
		case int:
			i64 = int64(v)
		case float64:
			i64 = int64(v)
		default:
			return nil, fmt.Errorf("expected integer type, got %T", b.Data)
		}
		buf.WriteString(fmt.Sprintf(":%d\r\n", i64))

	case "array":
		elems, ok := b.Data.([]models.RedisBodyType)
		if !ok {
			return nil, fmt.Errorf("expected []RedisBodyType, got %T", b.Data)
		}
		buf.WriteString(fmt.Sprintf("*%d\r\n", len(elems)))
		for _, e := range elems {
			eb, err := SerializeRedisBodyType(e)
			if err != nil {
				return nil, err
			}
			buf.Write(eb)
		}

	case "map":
		entries, ok := b.Data.([]models.RedisMapBody)
		if !ok {
			return nil, fmt.Errorf("expected []RedisMapBody, got %T", b.Data)
		}
		buf.WriteString(fmt.Sprintf("%%%d\r\n", len(entries)))
		for _, ent := range entries {
			// Key (always string)
			keyStr, ok := ent.Key.Value.(string)
			if !ok {
				return nil, fmt.Errorf("map key is %T, want string", ent.Key.Value)
			}
			buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(keyStr), keyStr))

			// Value (could be string, integer, nested BodyType)
			switch v := ent.Value.Value.(type) {
			case string:
				buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(v), v))
			case int64:
				buf.WriteString(fmt.Sprintf(":%d\r\n", v))
			case int:
				buf.WriteString(fmt.Sprintf(":%d\r\n", int64(v)))
			case float64:
				buf.WriteString(fmt.Sprintf(":%d\r\n", int64(v)))
			case models.RedisBodyType:
				vb, err := SerializeRedisBodyType(v)
				if err != nil {
					return nil, err
				}
				buf.Write(vb)
			default:
				return nil, fmt.Errorf("unsupported map value type %T", v)
			}
		}

	default:
		return nil, fmt.Errorf("unsupported type %q", b.Type)
	}

	return buf.Bytes(), nil
}

// SerializeAll concatenates the RESP3 encodings of a slice of RedisBodyType.
func SerializeAll(bodies []models.RedisBodyType) ([]byte, error) {
	var out bytes.Buffer
	for _, b := range bodies {
		bb, err := SerializeRedisBodyType(b)
		if err != nil {
			return nil, err
		}
		out.Write(bb)
	}
	return out.Bytes(), nil
}

// normalizeBody rewrites b.Data from generic interface{} into
// the concrete types your serializer expects.
// normalizeBody rewrites b.Data for arrays & maps into concrete types.
func normalizeBody(b models.RedisBodyType) (models.RedisBodyType, error) {
	switch b.Type {
	case "array":
		rawArr, ok := b.Data.([]interface{})
		if !ok {
			return b, fmt.Errorf("array.Data is %T", b.Data)
		}
		elems, err := convertArrayData(rawArr)
		if err != nil {
			return b, err
		}
		b.Data = elems

	case "map":
		rawMap, ok := b.Data.([]interface{})
		if !ok {
			return b, fmt.Errorf("map.Data is %T", b.Data)
		}
		entries, err := convertMapData(rawMap)
		if err != nil {
			return b, err
		}
		b.Data = entries
	}
	return b, nil
}

func convertArrayData(raw []interface{}) ([]models.RedisBodyType, error) {
	out := make([]models.RedisBodyType, len(raw))
	for i, elem := range raw {
		m := elem.(map[string]interface{}) // assume well-formed
		t, _ := m["type"].(string)
		sizeF, _ := m["size"].(float64)
		dataRaw := m["data"]

		b := models.RedisBodyType{Type: t, Size: int(sizeF), Data: dataRaw}
		b, err := normalizeBody(b) // recurse for nested arrays/maps
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

func convertMapData(raw []interface{}) ([]models.RedisMapBody, error) {
	out := make([]models.RedisMapBody, len(raw))
	for i, entry := range raw {
		em := entry.(map[string]interface{})
		kr := em["key"].(map[string]interface{})
		vr := em["value"].(map[string]interface{})

		kLenF, _ := kr["length"].(float64)
		kVal := kr["value"].(string)

		vLenF, _ := vr["length"].(float64)
		vRaw := vr["value"]

		// ---- ADD THESE CASES ----
		var final interface{}
		switch v := vRaw.(type) {
		case string:
			final = v
		case float64:
			final = int64(v)
		case int:
			final = int64(v)
		case int64:
			final = v
		case []interface{}:
			nested, err := convertArrayData(v)
			if err != nil {
				return nil, err
			}
			final = nested
		case map[string]interface{}:
			nt, _ := v["type"].(string)
			nsF, _ := v["size"].(float64)
			nd := v["data"]
			inner := models.RedisBodyType{Type: nt, Size: int(nsF), Data: nd}
			inner, err := normalizeBody(inner)
			if err != nil {
				return nil, err
			}
			final = inner
		default:
			return nil, fmt.Errorf("unsupported map value type %T", v)
		}
		// -------------------------

		out[i] = models.RedisMapBody{
			Key: models.RedisElement{
				Length: int(kLenF),
				Value:  kVal,
			},
			Value: models.RedisElement{
				Length: int(vLenF),
				Value:  final,
			},
		}
	}
	return out, nil
}

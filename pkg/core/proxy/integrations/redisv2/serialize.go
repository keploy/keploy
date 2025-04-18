package redisv2

import (
	"bytes"
	"fmt"
	"strconv"

	"go.keploy.io/server/v2/pkg/models"
)

// SerializeRedisBodyType emits the RESP3 encoding for a single RedisBodyType.
func SerializeRedisBodyType(b models.RedisBodyType) ([]byte, error) {
    var buf bytes.Buffer

    switch b.Type {
    case "string":
        // b.Data must be a Go string
        s, ok := b.Data.(string)
        if !ok {
            return nil, fmt.Errorf("expected string, got %T", b.Data)
        }
        // **recompute** the length
        buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s))

    case "integer":
        // b.Data must be int64
        i, ok := b.Data.(int64)
        if !ok {
            return nil, fmt.Errorf("expected int64, got %T", b.Data)
        }
        buf.WriteString(fmt.Sprintf(":%d\r\n", i))

    case "array":
        // b.Data must be []RedisBodyType
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
        // b.Data must be []RedisMapBody
        entries, ok := b.Data.([]models.RedisMapBody)
        if !ok {
            return nil, fmt.Errorf("expected []RedisMapBody, got %T", b.Data)
        }
        buf.WriteString(fmt.Sprintf("%%%d\r\n", len(entries)))
        for _, ent := range entries {
            // **key** is always a string
            keyStr, ok := ent.Key.Value.(string)
            if !ok {
                return nil, fmt.Errorf("expected map key string, got %T", ent.Key.Value)
            }
            buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(keyStr), keyStr))

            // **value** can be string, integer, array, map, etc.
            switch v := ent.Value.Value.(type) {
            case string:
                buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(v), v))
            case int64:
                buf.WriteString(fmt.Sprintf(":%d\r\n", v))
            case models.RedisBodyType:
                // nested single element (array or map)
                nb, err := SerializeRedisBodyType(v)
                if err != nil {
                    return nil, err
                }
                buf.Write(nb)
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

// serializeArray handles a raw []interface{} (from generic Data) as an RESP array.
func serializeArray(elems []interface{}) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("*%d\r\n", len(elems)))

	for _, el := range elems {
		m, ok := el.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("array element is %T", el)
		}
		t, _ := m["type"].(string)
		raw := m["data"]

		switch t {
		case "string":
			s, _ := raw.(string)
			buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s))

		case "integer":
			numF, _ := m["size"].(float64)
			buf.WriteString(fmt.Sprintf(":%d\r\n", int64(numF)))

		case "array":
			nested, _ := raw.([]interface{})
			b, err := serializeArray(nested)
			if err != nil {
				return nil, err
			}
			buf.Write(b)

		case "map":
			nestedMap, _ := raw.([]interface{})
			b, err := serializeMap(nestedMap)
			if err != nil {
				return nil, err
			}
			buf.Write(b)

		default:
			return nil, fmt.Errorf("unsupported array type %q", t)
		}
	}
	return buf.Bytes(), nil
}

// serializeMap handles a raw []interface{} (from generic Data) as an RESP map.
func serializeMap(entries []interface{}) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("%%%d\r\n", len(entries)))

	for _, entry := range entries {
		em, ok := entry.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("map element is %T", entry)
		}
		keyRaw, _ := em["key"].(map[string]interface{})
		keyStr, _ := keyRaw["value"].(string)
		buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(keyStr), keyStr))

		valRaw, _ := em["value"].(map[string]interface{})
		val := valRaw["value"]

		switch v := val.(type) {
		case string:
			buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(v), v))
		case float64:
			buf.WriteString(fmt.Sprintf(":%d\r\n", int64(v)))
		case []interface{}:
			b, err := serializeArray(v)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		case map[string]interface{}:
			// nested single RedisBodyType
			nt, _ := v["type"].(string)
			nsF, _ := v["size"].(float64)
			nd := v["data"]
			inner := models.RedisBodyType{
				Type: nt,
				Size: int(nsF),
				Data: nd,
			}
			bb, err := SerializeRedisBodyType(inner)
			if err != nil {
				return nil, err
			}
			buf.Write(bb)
		default:
			return nil, fmt.Errorf("unsupported map value type %T", v)
		}
	}
	return buf.Bytes(), nil
}

// convertDataToBytes picks the right path for your RESP type + Data field.
func convertDataToBytes(tp string, raw interface{}) ([]byte, error) {
	switch tp {
	case "string", "integer":
		// wrap it in a one‑element RedisBodyType and serialize
		size := 0
		switch v := raw.(type) {
		case string:
			size = len(v)
		case int64:
			size = len(strconv.FormatInt(v, 10))
		}
		b := models.RedisBodyType{Type: tp, Size: size, Data: raw}
		return SerializeRedisBodyType(b)

	case "array":
		// raw is []interface{} of map[string]interface{}
		return serializeArray(raw.([]interface{}))

	case "map":
		// raw is []interface{} of map entries
		return serializeMap(raw.([]interface{}))

	default:
		return nil, fmt.Errorf("unsupported top‑level type %q", tp)
	}
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

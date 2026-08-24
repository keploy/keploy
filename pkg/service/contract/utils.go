package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type Response struct {
	Code    int
	Message string
	Schema  models.Schema
	Body    interface{}
}

// normalizeJSONNumbers rewrites the json.Number values produced by a decoder
// with UseNumber: an integer literal that fits becomes int64, anything else
// becomes float64. Returns an error for a literal that fits neither.
//
// Decoding straight into interface{} makes every JSON number a float64, which
// costs two things. The schema says "number" for a field that is plainly an
// integer, so a client generated from the contract gets a float where the API
// returns an int - getType's int/int32/int64 case was unreachable for exactly
// this reason. And the example loses precision above 2^53: an id of
// 9007199254740993 was being written to the contract as 9.007199254740992e+15.
//
// json.Number itself cannot be carried through: it is a string type, so YAML
// marshals it quoted and the example turns into "1" instead of 1.
func normalizeJSONNumbers(v interface{}) (interface{}, error) {
	switch t := v.(type) {
	case json.Number:
		// An integer literal that fits becomes int64, which is what makes
		// getType report "integer" and keeps the example exact.
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		// Anything else - fractional, exponent notation, or an integer wider
		// than int64 - becomes float64. That last case is still lossy above
		// 2^53, so the exactly-representable range widens from 2^53 to int64
		// rather than becoming unbounded.
		f, err := t.Float64()
		if err != nil {
			// A literal that overflows float64 (1e400). Plain json.Unmarshal
			// rejected this too; keep it an error rather than silently
			// recording the number as a string field.
			return nil, fmt.Errorf("cannot represent JSON number %s: %w", t.String(), err)
		}
		return f, nil
	case map[string]interface{}:
		for k, val := range t {
			n, err := normalizeJSONNumbers(val)
			if err != nil {
				return nil, err
			}
			t[k] = n
		}
		return t, nil
	case []interface{}:
		for i, val := range t {
			n, err := normalizeJSONNumbers(val)
			if err != nil {
				return nil, err
			}
			t[i] = n
		}
		return t, nil
	}
	return v, nil
}

// decodeJSONBody decodes a recorded request/response body the way contract
// generation needs it: numbers kept exact and integer literals distinguishable
// from fractional ones. An empty body decodes to an empty object so a body-less
// doc still serializes its example as {}.
func decodeJSONBody(body string) (interface{}, error) {
	if body == "" {
		return map[string]interface{}{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// json.Unmarshal rejects trailing data; Decoder does not. Without this a
	// concatenated or NDJSON body would silently yield a contract describing
	// only its first document.
	if dec.More() {
		return nil, fmt.Errorf("unexpected trailing data after JSON body")
	}
	return normalizeJSONNumbers(v)
}

// Inference distinguishes three states that OpenAPI 3.0 itself cannot spell,
// so they are carried as internal type markers and resolved by finalizeSchema.
// Both are deliberately illegal type names: they can never collide with a type
// inferred from data, and a leak into a generated contract is unmistakable.
//
// Keeping them explicit - rather than encoding "unknown" as a missing type key -
// is what makes finalizeSchema idempotent. Absence of a type would otherwise
// mean both "nothing observed yet" and "already resolved to untyped", and a
// second pass over a resolved node would turn an untyped schema into a string.
const (
	// unknownType means no element has been observed yet: an empty array, or a
	// JSON null, which carries no type of its own. It is the identity element
	// of mergeSchemas, so uninformative elements contribute nothing but
	// nullability instead of pinning down a type there is no evidence for.
	unknownType = "\x00unknown"
	// anyType means the observations conflict, so no single OpenAPI 3.0 type
	// describes them all.
	anyType = "\x00conflict"
)

// getType maps a decoded JSON value onto an OpenAPI 3.0 type name.
//
// int64 is produced by decodeJSONBody for integer literals; plain
// encoding/json would make every number a float64 and this case unreachable.
func getType(value interface{}) string {
	switch value.(type) {
	case float64:
		return "number"
	case int, int32, int64:
		return "integer"
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	default:
		return "string"
	}
}

// schemaForValue returns a map-form schema describing exactly v.
//
// The map form is canonical throughout inference because models.Schema's own
// Properties field already is a map[string]map[string]interface{} - every
// schema below the root is a map anyway, and the struct exists only because
// models.MediaType.Schema is typed. Converting once at the root (schemaFromMap)
// avoids maintaining the same merge rules over two representations.
//
// A JSON null carries no type, so it yields the unknownType marker plus
// nullable: "unknown but nullable". See the marker constants for why that is
// spelled out rather than left implicit.
func schemaForValue(v interface{}) map[string]interface{} {
	switch val := v.(type) {
	case nil:
		return map[string]interface{}{"type": unknownType, "nullable": true}
	case map[string]interface{}:
		return map[string]interface{}{"type": "object", "properties": extractVariableTypes(val)}
	case []interface{}:
		return map[string]interface{}{"type": "array", "items": itemSchema(val)}
	default:
		return map[string]interface{}{"type": getType(v)}
	}
}

// itemSchema folds every element of arr into a single item schema.
//
// Inferring from one chosen element and patching the result afterwards cannot
// work: an empty array, a null, or an element that simply lacks a key reveals
// nothing, and no amount of patching recovers a type that was never observed.
// [[], [1]] inferred "string" from the empty first element and then emitted a
// schema its own example violated, which aborts `keploy contract generate`.
// Folding instead makes "no evidence" the identity element, so uninformative
// elements are skipped rather than believed.
//
// A nil accumulator means "nothing observed yet", which is why merging with it
// yields the other side untouched.
func itemSchema(arr []interface{}) map[string]interface{} {
	var acc map[string]interface{}
	for _, el := range arr {
		acc = mergeSchemas(acc, schemaForValue(el))
	}
	if acc == nil {
		// An empty array: no element was ever observed.
		acc = map[string]interface{}{"type": unknownType}
	}
	return acc
}

// mergeSchemas returns the least schema that describes everything both a and b
// describe. Nullability is the union of both sides.
func mergeSchemas(a, b map[string]interface{}) map[string]interface{} {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}

	nullable := a["nullable"] == true || b["nullable"] == true

	aType, _ := a["type"].(string)
	bType, _ := b["type"].(string)

	var out map[string]interface{}
	switch {
	case aType == unknownType:
		// Nothing was observed on this side, so it constrains nothing.
		out = cloneSchema(b)
	case bType == unknownType:
		out = cloneSchema(a)
	case aType == anyType || bType == anyType:
		out = map[string]interface{}{"type": anyType}
	case aType == bType:
		switch aType {
		case "object":
			out = mergeObjectSchemas(a, b)
		case "array":
			out = map[string]interface{}{
				"type":  "array",
				"items": mergeSchemas(itemsOf(a), itemsOf(b)),
			}
		default:
			out = map[string]interface{}{"type": aType}
		}
	case isNumericType(aType) && isNumericType(bType):
		// The only widening OpenAPI 3.0 can express: every integer is a
		// number, so [1, 1.5] is an array of number.
		out = map[string]interface{}{"type": "number"}
	default:
		// string vs integer and friends. OpenAPI 3.0 has no single type for
		// this; oneOf would be the honest encoding and is out of scope here,
		// so record the conflict and let finalizeSchema emit an untyped schema
		// rather than a typed one the recorded example would violate.
		out = map[string]interface{}{"type": anyType}
	}

	if nullable {
		out["nullable"] = true
	} else {
		delete(out, "nullable")
	}
	return out
}

// mergeObjectSchemas merges the properties of two object schemas.
//
// Only keys already present in a are kept, which is the historical behaviour:
// an item schema describes the keys the first object showed, and OpenAPI allows
// undeclared properties, so a later element with extra keys is still valid
// against it. Unioning the keys would be better inference but changes the
// schema emitted for bodies that generate fine today.
func mergeObjectSchemas(a, b map[string]interface{}) map[string]interface{} {
	aProps := propertiesOf(a)
	bProps := propertiesOf(b)
	merged := make(map[string]map[string]interface{}, len(aProps))
	for key, aProp := range aProps {
		if bProp, ok := bProps[key]; ok {
			merged[key] = mergeSchemas(aProp, bProp)
			continue
		}
		merged[key] = aProp
	}
	return map[string]interface{}{"type": "object", "properties": merged}
}

func propertiesOf(s map[string]interface{}) map[string]map[string]interface{} {
	props, _ := s["properties"].(map[string]map[string]interface{})
	return props
}

func itemsOf(s map[string]interface{}) map[string]interface{} {
	items, _ := s["items"].(map[string]interface{})
	return items
}

func isNumericType(t string) bool { return t == "integer" || t == "number" }

func cloneSchema(s map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// finalizeSchema fills in what the fold could not determine, in place.
//
// It must run exactly once, on the completed schema, never inside the
// recursion. Materialising a default early turns "unknown" back into a real
// type, and merging that with the first informative element then reads as a
// conflict - which is precisely the bug this replaces.
func finalizeSchema(s map[string]interface{}) {
	if s == nil {
		return
	}
	switch t, _ := s["type"].(string); t {
	case anyType:
		// An untyped schema accepts any kind. Emitting a concrete type here
		// would knowingly produce a document its own example violates.
		delete(s, "type")
	case unknownType:
		// Zero evidence: [] or [null] never showed an element type. "string"
		// is the historical default and is vacuously correct - there is no
		// element for it to be wrong about.
		s["type"] = "string"
	case "object":
		for _, prop := range propertiesOf(s) {
			finalizeSchema(prop)
		}
	case "array":
		items := itemsOf(s)
		if items == nil {
			// OpenAPI 3.0 rejects an array schema without items.
			items = map[string]interface{}{"type": "string"}
			s["items"] = items
		}
		finalizeSchema(items)
	}
}

// schemaFromMap converts a finalized map-form schema to the struct form
// models.MediaType requires. Only the root needs it; everything below stays a
// map, which is what models.Schema.Properties already holds.
func schemaFromMap(s map[string]interface{}) models.Schema {
	out := models.Schema{}
	if t, ok := s["type"].(string); ok {
		out.Type = t
	}
	if n, ok := s["nullable"].(bool); ok {
		out.Nullable = n
	}
	if props := propertiesOf(s); props != nil {
		out.Properties = props
	}
	if items := itemsOf(s); items != nil {
		nested := schemaFromMap(items)
		out.Items = &nested
	}
	return out
}

// ExtractVariableTypes returns the type of each variable in the object.
func ExtractVariableTypes(obj map[string]interface{}) map[string]map[string]interface{} {
	types := extractVariableTypes(obj)
	for _, prop := range types {
		finalizeSchema(prop)
	}
	return types
}

// extractVariableTypes is ExtractVariableTypes without finalization, for use
// inside the fold. See finalizeSchema for why the two must stay separate.
func extractVariableTypes(obj map[string]interface{}) map[string]map[string]interface{} {
	types := make(map[string]map[string]interface{}, len(obj))
	for key, value := range obj {
		types[key] = schemaForValue(value)
	}
	return types
}

// SchemaForBody builds an OpenAPI schema for a decoded JSON body whose root
// may be either an object or an array. For an object root it returns a
// {type: object, properties: ...} schema (properties precomputed by the
// caller via ExtractVariableTypes). For an array root it returns a
// {type: array, items: ...} schema whose item schema is folded over every
// element. A scalar root is described by its own type.
func SchemaForBody(body interface{}, objectProps map[string]map[string]interface{}) models.Schema {
	arr, ok := body.([]interface{})
	if !ok {
		// An object root keeps the properties the caller already extracted, and
		// a nil body (no body recorded) stays an empty object so it still
		// serializes its example as {}.
		if _, isObject := body.(map[string]interface{}); isObject || body == nil {
			return models.Schema{Type: "object", Properties: objectProps}
		}
		// A scalar root - 5, "ok", true - was previously described as an
		// object, producing a schema its own example violates and aborting
		// generation the same way the array shapes above did. JSON allows any
		// value as a document, and an API answering `5` or `true` is ordinary.
		scalar := schemaForValue(body)
		finalizeSchema(scalar)
		return schemaFromMap(scalar)
	}

	items := itemSchema(arr)
	finalizeSchema(items)
	nested := schemaFromMap(items)
	return models.Schema{Type: "array", Items: &nested}
}

func GenerateResponse(response Response) map[string]models.ResponseItem {
	byCode := map[string]models.ResponseItem{
		fmt.Sprintf("%d", response.Code): {
			Description: response.Message,
			Content: map[string]models.MediaType{
				"application/json": {
					Schema:  response.Schema,
					Example: (response.Body),
				},
			},
		},
	}
	return byCode
}

func ExtractURLPath(URL string) (string, string) {
	parsedURL, err := url.Parse(URL)

	if err != nil {
		return "", ""
	}
	return parsedURL.Path, parsedURL.Host
}

func GenerateHeader(header map[string]string) []models.Parameter {
	var parameters []models.Parameter
	for key, value := range header {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       "header",
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}
	return parameters
}

// isNumeric checks if a string is a valid numeric value (integer or float).
func isNumeric(s string) bool {
	if _, err := strconv.Atoi(s); err == nil {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}

// ExtractIdentifiers extracts numeric identifiers (integers or floats) from the path.
func ExtractIdentifiers(path string) []string {
	segments := strings.Split(path, "/")
	segments2 := strings.Split(segments[len(segments)-1], "?")
	segments = append(segments[:len(segments)-1], segments2[0])
	var identifiers []string
	for _, segment := range segments {
		if segment != "" {
			// Check if the segment is numeric (integer or float)
			if isNumeric(segment) {
				identifiers = append(identifiers, segment)
			}
		}
	}

	return identifiers
}

// ExtractQueryParams extracts the query parameters and their names from the URL.
func ExtractQueryParams(rawURL string) (map[string]string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	queryParams := parsedURL.Query()
	params := make(map[string]string)
	for key, values := range queryParams {
		if len(values) > 0 {
			// Take the first value if multiple are present
			params[key] = values[0]
		}
	}
	return params, nil
}

// GenerateDummyNamesForIdentifiers generates dummy names for the path identifiers.
func GenerateDummyNamesForIdentifiers(identifiers []string) map[string]string {
	dummyNames := make(map[string]string)
	for i, id := range identifiers {
		dummyName := fmt.Sprintf("param%d", i+1)
		dummyNames[dummyName] = id
	}
	return dummyNames
}
func AppendInParameters(parameters []models.Parameter, inParameters map[string]string, paramType string) []models.Parameter {

	for key, value := range inParameters {
		parameters = append(parameters, models.Parameter{
			Name:     key,
			In:       paramType,
			Required: true,
			Schema:   models.ParamSchema{Type: "string"},
			Example:  value,
		})
	}

	return parameters
}

// ReplacePathIdentifiers replaces numeric identifiers in the path with their corresponding dummy names.
func ReplacePathIdentifiers(path string, dummyNames map[string]string) string {
	segments := strings.Split(path, "/")
	var replacedPath []string
	for _, segment := range segments {
		if segment != "" {
			// Check if the segment is numeric (integer or float)
			if isNumeric(segment) {
				dummyName := ""
				for key, value := range dummyNames {
					if value == segment {
						// i want to put '{' and '}' around the key
						dummyName = "{" + key + "}"
						break
					}
				}
				if dummyName != "" {
					replacedPath = append(replacedPath, dummyName)
				} else {
					replacedPath = append(replacedPath, segment)
				}
			} else {
				replacedPath = append(replacedPath, segment)
			}
		}
	}
	finalPath := strings.Join(replacedPath, "/")
	// Add slash at the beginning of the path
	finalPath = "/" + finalPath
	return finalPath
}

func generateUniqueID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		// handle error
		return ""
	}
	return hex.EncodeToString(b) + "-" + time.Now().Format("20060102150405")
}

func checkConfigFile(servicesMapping map[string][]string) error {
	// Check if the size of servicesMapping is less than 1
	if len(servicesMapping) < 1 {
		return fmt.Errorf("services mapping must contain at least 1 services")
	}
	return nil
}

func saveServiceMappings(servicesMapping config.Mappings, filePath string) error {
	// Marshal the services mapping to YAML
	servicesMappingYAML, err := yamlLib.Marshal(servicesMapping)
	if err != nil {
		return fmt.Errorf("failed to marshal services mapping: %w", err)
	}

	// Write the services mapping to the specified file path
	err = yaml.WriteFile(context.Background(), zap.NewNop(), filePath, "serviceMappings", servicesMappingYAML, false)
	if err != nil {
		return fmt.Errorf("failed to write services mapping to file: %w", err)
	}

	return nil
}

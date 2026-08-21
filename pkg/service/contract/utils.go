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

// widenSchemaForValue widens an item schema, inferred from one element of an
// array, so that it also describes v.
//
// Item schemas are inferred from a single element while MediaType.Example
// carries the whole body, and kin-openapi validates the example against the
// schema. Without widening, [1, 1.5] infers items:integer from the first
// element and then fails validation on the second - and a validateSchema
// failure aborts the whole `keploy contract generate` run, so one
// {"prices":[10, 9.99]} in a recorded response would take the command down.
//
// Widening the schema rather than rewriting the decoded body keeps every
// recorded value exact, and does not depend on sibling elements lining up
// positionally with the one the schema came from - a null anywhere in either
// array would break that alignment.
func widenSchemaForValue(s *models.Schema, v interface{}) {
	if s == nil {
		return
	}
	switch val := v.(type) {
	case float64:
		if s.Type == "integer" {
			s.Type = "number"
		}
	case map[string]interface{}:
		for k, vv := range val {
			if prop, ok := s.Properties[k]; ok {
				widenPropertyForValue(prop, vv)
			}
		}
	case []interface{}:
		for _, el := range val {
			widenSchemaForValue(s.Items, el)
		}
	}
}

// widenPropertyForValue is widenSchemaForValue for the map-shaped property
// schemas ExtractVariableTypes produces.
func widenPropertyForValue(prop map[string]interface{}, v interface{}) {
	if prop == nil {
		return
	}
	switch val := v.(type) {
	case float64:
		if prop["type"] == "integer" {
			prop["type"] = "number"
		}
	case map[string]interface{}:
		sub, ok := prop["properties"].(map[string]map[string]interface{})
		if !ok {
			return
		}
		for k, vv := range val {
			if p, ok := sub[k]; ok {
				widenPropertyForValue(p, vv)
			}
		}
	case []interface{}:
		items, ok := prop["items"].(map[string]interface{})
		if !ok {
			return
		}
		for _, el := range val {
			widenPropertyForValue(items, el)
		}
	}
}

// ExtractVariableTypes returns the type of each variable in the object.
func ExtractVariableTypes(obj map[string]interface{}) map[string]map[string]interface{} {
	types := make(map[string]map[string]interface{}, len(obj))

	getType := func(value interface{}) string {
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

	for key, value := range obj {
		// A JSON null carries no type. OpenAPI 3.0 still requires one, so keep
		// the historical "string" default but mark the property nullable -
		// without it the untouched example holds a null that kin-openapi
		// rejects with `Value is not nullable`, aborting contract generation
		// for any payload with a null field.
		if value == nil {
			types[key] = map[string]interface{}{"type": "string", "nullable": true}
			continue
		}

		valueType := getType(value)
		responseType := map[string]interface{}{
			"type": valueType,
		}

		switch valueType {
		case "object":
			responseType["properties"] = ExtractVariableTypes(value.(map[string]interface{}))
		case "array":
			arrayItems := value.([]interface{})
			arrayType := "string" // Default to string if array is empty

			// Infer from the first non-null element; a null anywhere in the
			// array only means the items are nullable.
			firstElement, nullable := firstNonNil(arrayItems)
			if firstElement != nil {
				arrayType = getType(firstElement)
				if arrayType == "object" {
					items := map[string]interface{}{
						"type":       arrayType,
						"properties": ExtractVariableTypes(firstElement.(map[string]interface{})),
					}
					if nullable {
						items["nullable"] = true
					}
					for _, el := range arrayItems {
						widenPropertyForValue(items, el)
					}
					responseType["items"] = items
					types[key] = responseType
					continue
				}
			}
			items := map[string]interface{}{
				"type": arrayType,
			}
			if nullable {
				items["nullable"] = true
			}
			for _, el := range arrayItems {
				widenPropertyForValue(items, el)
			}
			responseType["items"] = items
		}

		types[key] = responseType
	}

	return types
}

// firstNonNil returns the first non-null element of items along with whether
// any element was null, so callers can infer an item type from real data while
// still marking the schema nullable.
func firstNonNil(items []interface{}) (interface{}, bool) {
	var first interface{}
	nullable := false
	for _, item := range items {
		if item == nil {
			nullable = true
			continue
		}
		if first == nil {
			first = item
		}
	}
	return first, nullable
}

// SchemaForBody builds an OpenAPI schema for a decoded JSON body whose root
// may be either an object or an array. For an object root it returns a
// {type: object, properties: ...} schema (properties precomputed by the
// caller via ExtractVariableTypes). For an array root it returns a
// {type: array, items: ...} schema, inferring the item schema from the first
// element. Any other root (or an empty body) falls back to an object schema so
// existing behaviour is preserved.
func SchemaForBody(body interface{}, objectProps map[string]map[string]interface{}) models.Schema {
	arr, ok := body.([]interface{})
	if !ok {
		// Object root (or nil/empty body): keep the original object schema.
		return models.Schema{Type: "object", Properties: objectProps}
	}

	items := &models.Schema{Type: "string"} // default for an empty array
	// Infer from the first non-null element; a null anywhere in the array only
	// means the items are nullable, and inferring "string" from it would make
	// kin-openapi reject the untouched example.
	first, nullable := firstNonNil(arr)
	switch v := first.(type) {
	case map[string]interface{}:
		items = &models.Schema{
			Type:       "object",
			Properties: ExtractVariableTypes(v),
		}
	case []interface{}:
		nested := SchemaForBody(v, nil)
		items = &nested
	case int64:
		items = &models.Schema{Type: "integer"}
	case float64:
		items = &models.Schema{Type: "number"}
	case bool:
		items = &models.Schema{Type: "boolean"}
	case string:
		items = &models.Schema{Type: "string"}
	}
	items.Nullable = nullable
	// The schema came from one element; widen it until it describes them all.
	for _, el := range arr {
		widenSchemaForValue(items, el)
	}
	return models.Schema{Type: "array", Items: items}
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

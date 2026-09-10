package contract

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"go.keploy.io/server/v3/pkg/models"
)

// InferSchema derives an OpenAPI 3.0 document from recorded HTTP test cases.
//
// Bodies are described by the same fold HTTPDocToOpenAPI uses (schemaForValue /
// mergeSchemas / finalizeSchema) rather than by a second inference routine.
// Keeping one implementation is the point: this used to decode with plain
// encoding/json, so every JSON number arrived as a float64 and every integer
// field was typed "number" - the same body run through the two surfaces of one
// command produced two different contracts.
//
// Observations of the same operation are merged rather than one overwriting the
// other. Request bodies used to be first-wins and responses last-wins, two
// opposite policies in one function, which was invisible only while every
// number was a float64: with integers typed as integers, a price of 10 in one
// test case and 9.99 in another would otherwise produce a contract that depends
// on the order the test cases happened to be walked in.
func InferSchema(testCases []models.TestCase) (*openapi3.T, error) {
	doc := &openapi3.T{
		OpenAPI: "3.0.0",
		Info: &openapi3.Info{
			Title:   "Inferred API Contract",
			Version: "1.0.0",
		},
		Paths: openapi3.NewPaths(),
	}

	// Raw, unfinalized schemas accumulated across every test case that touched
	// the operation. They stay unfinalized until the fold is complete: filling
	// in a default early turns "not observed yet" into a real type, and merging
	// that with a later observation then reads as a conflict. The operation
	// pointer identifies a path and method uniquely, which is exactly the key
	// these are accumulated per.
	requestSchemas := make(map[*openapi3.Operation]map[string]interface{})
	responseSchemas := make(map[*openapi3.Operation]map[string]map[string]interface{})

	for _, tc := range testCases {
		method := strings.ToUpper(string(tc.HTTPReq.Method))
		if method == "" || !isSupportedMethod(method) {
			continue
		}

		path, err := extractPath(tc.HTTPReq.URL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse request URL %q: %w", tc.HTTPReq.URL, err)
		}

		pathItem := doc.Paths.Value(path)
		if pathItem == nil {
			pathItem = &openapi3.PathItem{}
			doc.Paths.Set(path, pathItem)
		}

		op := pathItem.GetOperation(method)
		if op == nil {
			op = openapi3.NewOperation()
			op.Responses = openapi3.NewResponsesWithCapacity(1)
			pathItem.SetOperation(method, op)
		}

		if requestSchema, ok := inferSchemaFromBody(tc.HTTPReq.Body); ok {
			requestSchemas[op] = mergeSchemas(requestSchemas[op], requestSchema)
		}

		statusCode := strconv.Itoa(tc.HTTPResp.StatusCode)
		if op.Responses.Value(statusCode) == nil {
			desc := tc.HTTPResp.StatusMessage
			if desc == "" {
				desc = http.StatusText(tc.HTTPResp.StatusCode)
			}
			if desc == "" {
				desc = "response"
			}
			// Copy to avoid pointer aliasing across loop iterations
			description := desc
			op.Responses.Set(statusCode, &openapi3.ResponseRef{Value: &openapi3.Response{Description: &description}})
		}

		if responseSchema, ok := inferSchemaFromBody(tc.HTTPResp.Body); ok {
			if responseSchemas[op] == nil {
				responseSchemas[op] = make(map[string]map[string]interface{})
			}
			responseSchemas[op][statusCode] = mergeSchemas(responseSchemas[op][statusCode], responseSchema)
		}
	}

	if doc.Paths.Len() == 0 {
		return nil, errors.New("no HTTP test cases found to infer schema")
	}

	// Every observation is in; resolve the placeholders and attach the result.
	for op, schema := range requestSchemas {
		finalizeSchema(schema)
		op.RequestBody = &openapi3.RequestBodyRef{
			Value: &openapi3.RequestBody{
				Required: false,
				Content: openapi3.Content{
					"application/json": &openapi3.MediaType{Schema: openAPISchemaRef(schema)},
				},
			},
		}
	}
	for op, byStatus := range responseSchemas {
		for statusCode, schema := range byStatus {
			finalizeSchema(schema)
			op.Responses.Value(statusCode).Value.Content = openapi3.Content{
				"application/json": &openapi3.MediaType{Schema: openAPISchemaRef(schema)},
			}
		}
	}

	return doc, nil
}

func isSupportedMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func extractPath(rawURL string) (string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	path := parsedURL.Path
	if path == "" {
		path = "/"
	}
	return path, nil
}

// inferSchemaFromBody returns a raw, unfinalized map-form schema for a recorded
// body, or ok=false when the body is empty or not JSON this generator can
// describe.
func inferSchemaFromBody(body string) (map[string]interface{}, bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, false
	}

	// decodeJSONBody, not encoding/json: it keeps integer literals as int64, so
	// an integer field is typed "integer" instead of "number", and an id above
	// 2^53 stays exact. It is also stricter about trailing data, where a
	// concatenated body would otherwise be described by its first document
	// alone.
	value, err := decodeJSONBody(trimmed)
	if err != nil {
		return nil, false
	}

	return schemaForValue(value), true
}

// openAPISchemaRef converts a finalized map-form schema to the kin-openapi
// representation this document is built from. A schema with no type is left
// untyped, which is how a conflict between observations is expressed.
func openAPISchemaRef(s map[string]interface{}) *openapi3.SchemaRef {
	schema := openapi3.NewSchema()
	if t, ok := s["type"].(string); ok && t != "" {
		schema.Type = &openapi3.Types{t}
	}
	if s["nullable"] == true {
		schema.Nullable = true
	}
	if props := propertiesOf(s); props != nil {
		schema.Properties = make(openapi3.Schemas, len(props))
		for key, prop := range props {
			schema.Properties[key] = openAPISchemaRef(prop)
		}
	}
	if items := itemsOf(s); items != nil {
		schema.Items = openAPISchemaRef(items)
	}
	// Fields are deliberately not marked Required: inference cannot tell an
	// optional field that happened to be present from a mandatory one.
	return openapi3.NewSchemaRef("", schema)
}

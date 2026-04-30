package contract

import (
	"encoding/json"
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
func InferSchema(testCases []models.TestCase) (*openapi3.T, error) {
	doc := &openapi3.T{
		OpenAPI: "3.0.0",
		Info: &openapi3.Info{
			Title:   "Inferred API Contract",
			Version: "1.0.0",
		},
		Paths: openapi3.NewPaths(),
	}

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

		if requestSchema, ok := inferSchemaFromBody(tc.HTTPReq.Body); ok && op.RequestBody == nil {
			op.RequestBody = &openapi3.RequestBodyRef{
				Value: &openapi3.RequestBody{
					Required: false,
					Content: openapi3.Content{
						"application/json": &openapi3.MediaType{Schema: requestSchema},
					},
				},
			}
		}

		statusCode := strconv.Itoa(tc.HTTPResp.StatusCode)
		desc := tc.HTTPResp.StatusMessage
		if desc == "" {
			desc = http.StatusText(tc.HTTPResp.StatusCode)
		}
		if desc == "" {
			desc = "response"
		}
		// Copy to avoid pointer aliasing across loop iterations
		description := desc

		response := &openapi3.Response{Description: &description}
		if responseSchema, ok := inferSchemaFromBody(tc.HTTPResp.Body); ok {
			response.Content = openapi3.Content{
				"application/json": &openapi3.MediaType{Schema: responseSchema},
			}
		}

		op.Responses.Set(statusCode, &openapi3.ResponseRef{Value: response})
	}

	if doc.Paths.Len() == 0 {
		return nil, errors.New("no HTTP test cases found to infer schema")
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

func inferSchemaFromBody(body string) (*openapi3.SchemaRef, bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, false
	}

	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return nil, false
	}

	return inferSchemaRef(value), true
}

func inferSchemaRef(value any) *openapi3.SchemaRef {
	switch v := value.(type) {
	case nil:
		s := openapi3.NewSchema()
		s.Nullable = true
		return openapi3.NewSchemaRef("", s)
	case string:
		return openapi3.NewSchemaRef("", openapi3.NewStringSchema())
	case bool:
		return openapi3.NewSchemaRef("", openapi3.NewBoolSchema())
	case float64:
		return openapi3.NewSchemaRef("", openapi3.NewFloat64Schema())
	case []any:
		arraySchema := openapi3.NewArraySchema()
		if len(v) > 0 {
			arraySchema.Items = inferSchemaRef(v[0])
		} else {
			// OpenAPI requires Items on array schemas; default to empty object.
			arraySchema.Items = openapi3.NewSchemaRef("", openapi3.NewObjectSchema())
		}
		return openapi3.NewSchemaRef("", arraySchema)
	case map[string]any:
		objectSchema := openapi3.NewObjectSchema()
		properties := make(openapi3.Schemas, len(v))

		for key, val := range v {
			properties[key] = inferSchemaRef(val)
		}
		objectSchema.Properties = properties
		// Do not mark all fields Required from a single sample; inference
		// from one observation cannot determine which fields are optional.
		return openapi3.NewSchemaRef("", objectSchema)
	default:
		return openapi3.NewSchemaRef("", openapi3.NewStringSchema())
	}
}

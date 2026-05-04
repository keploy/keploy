package contract

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"go.keploy.io/server/v3/pkg/models"
)

func TestInferSchemaSingleGETEndpoint(t *testing.T) {
	doc, err := InferSchema([]models.TestCase{
		{
			HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://example.com/users/1"},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"id":"1","name":"John"}`},
		},
	})
	if err != nil {
		t.Fatalf("InferSchema returned error: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("InferSchema produced invalid OpenAPI doc: %v", err)
	}

	pathItem := doc.Paths.Value("/users/1")
	if pathItem == nil || pathItem.Get == nil {
		t.Fatalf("expected GET operation for /users/1")
	}
	if pathItem.Get.RequestBody != nil {
		t.Fatalf("expected no request body for GET")
	}

	response := pathItem.Get.Responses.Value("200")
	if response == nil || response.Value == nil {
		t.Fatalf("expected 200 response")
	}

	schema := response.Value.Content["application/json"].Schema
	assertSchemaType(t, schema, "object")
	assertSchemaType(t, schema.Value.Properties["id"], "string")
	assertSchemaType(t, schema.Value.Properties["name"], "string")
}

func TestInferSchemaPOSTEndpointWithJSONBody(t *testing.T) {
	doc, err := InferSchema([]models.TestCase{
		{
			HTTPReq:  models.HTTPReq{Method: "POST", URL: "http://example.com/users", Body: `{"name":"John","age":30}`},
			HTTPResp: models.HTTPResp{StatusCode: 201, Body: `{"id":"u-1"}`},
		},
	})
	if err != nil {
		t.Fatalf("InferSchema returned error: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("InferSchema produced invalid OpenAPI doc: %v", err)
	}

	pathItem := doc.Paths.Value("/users")
	if pathItem == nil || pathItem.Post == nil {
		t.Fatalf("expected POST operation for /users")
	}

	requestBody := pathItem.Post.RequestBody
	if requestBody == nil || requestBody.Value == nil {
		t.Fatalf("expected request body for POST")
	}

	schema := requestBody.Value.Content["application/json"].Schema
	assertSchemaType(t, schema, "object")
	assertSchemaType(t, schema.Value.Properties["name"], "string")
	assertSchemaType(t, schema.Value.Properties["age"], "number")
}

func TestInferSchemaMultipleEndpoints(t *testing.T) {
	testCases := []models.TestCase{
		{HTTPReq: models.HTTPReq{Method: "GET", URL: "http://example.com/users"}, HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"users":[]}`}},
		{HTTPReq: models.HTTPReq{Method: "POST", URL: "http://example.com/users", Body: `{"name":"Jane"}`}, HTTPResp: models.HTTPResp{StatusCode: 201, Body: `{"id":"2"}`}},
		{HTTPReq: models.HTTPReq{Method: "GET", URL: "http://example.com/health"}, HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`}},
	}

	doc, err := InferSchema(testCases)
	if err != nil {
		t.Fatalf("InferSchema returned error: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("InferSchema produced invalid OpenAPI doc: %v", err)
	}

	users := doc.Paths.Value("/users")
	if users == nil || users.Get == nil || users.Post == nil {
		t.Fatalf("expected both GET and POST for /users")
	}

	health := doc.Paths.Value("/health")
	if health == nil || health.Get == nil {
		t.Fatalf("expected GET for /health")
	}
}

func TestInferSchemaTypeInferenceNestedObjects(t *testing.T) {
	doc, err := InferSchema([]models.TestCase{
		{
			HTTPReq:  models.HTTPReq{Method: "POST", URL: "http://example.com/profile"},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"name":"John","age":30,"active":true,"profile":{"city":"SF"},"tags":["go"]}`},
		},
	})
	if err != nil {
		t.Fatalf("InferSchema returned error: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("InferSchema produced invalid OpenAPI doc: %v", err)
	}

	response := doc.Paths.Value("/profile").Post.Responses.Value("200")
	schema := response.Value.Content["application/json"].Schema

	assertSchemaType(t, schema.Value.Properties["name"], "string")
	assertSchemaType(t, schema.Value.Properties["age"], "number")
	assertSchemaType(t, schema.Value.Properties["active"], "boolean")

	profile := schema.Value.Properties["profile"]
	assertSchemaType(t, profile, "object")
	assertSchemaType(t, profile.Value.Properties["city"], "string")

	tags := schema.Value.Properties["tags"]
	assertSchemaType(t, tags, "array")
	assertSchemaType(t, tags.Value.Items, "string")
}

func TestInferSchemaResponseDescriptionPointerAliasing(t *testing.T) {
	// Regression: each response must have its own description string, not a
	// shared pointer that gets overwritten on the next loop iteration.
	doc, err := InferSchema([]models.TestCase{
		{HTTPReq: models.HTTPReq{Method: "GET", URL: "http://example.com/a"}, HTTPResp: models.HTTPResp{StatusCode: 200, StatusMessage: "OK"}},
		{HTTPReq: models.HTTPReq{Method: "GET", URL: "http://example.com/b"}, HTTPResp: models.HTTPResp{StatusCode: 404, StatusMessage: "Not Found"}},
	})
	if err != nil {
		t.Fatalf("InferSchema returned error: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("InferSchema produced invalid OpenAPI doc: %v", err)
	}

	aResp := doc.Paths.Value("/a").Get.Responses.Value("200")
	bResp := doc.Paths.Value("/b").Get.Responses.Value("404")

	if *aResp.Value.Description != "OK" {
		t.Errorf("expected /a 200 description 'OK', got %q", *aResp.Value.Description)
	}
	if *bResp.Value.Description != "Not Found" {
		t.Errorf("expected /b 404 description 'Not Found', got %q", *bResp.Value.Description)
	}
}

func TestInferSchemaObjectFieldsNotRequired(t *testing.T) {
	// Inference from a single sample should not mark all fields as required.
	doc, err := InferSchema([]models.TestCase{
		{HTTPReq: models.HTTPReq{Method: "GET", URL: "http://example.com/user"}, HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"id":"1","name":"John"}`}},
	})
	if err != nil {
		t.Fatalf("InferSchema returned error: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("InferSchema produced invalid OpenAPI doc: %v", err)
	}

	schema := doc.Paths.Value("/user").Get.Responses.Value("200").Value.Content["application/json"].Schema
	if len(schema.Value.Required) > 0 {
		t.Errorf("expected no Required fields from single-sample inference, got %v", schema.Value.Required)
	}
}

func assertSchemaType(t *testing.T, schemaRef *openapi3.SchemaRef, expected string) {
	t.Helper()

	if schemaRef == nil || schemaRef.Value == nil || schemaRef.Value.Type == nil || !schemaRef.Value.Type.Is(expected) {
		t.Fatalf("expected type %q", expected)
	}
}

func TestSafeJoinWithinRoot(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{"relative within root", "assets/body.json", false},
		{"dot path", ".", false},
		{"absolute path rejected", "/etc/passwd", true},
		{"traversal rejected", "../../etc/passwd", true},
		{"nested traversal rejected", "assets/../../etc/passwd", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeJoinWithinRoot(root, tc.rel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got path %q", tc.rel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.rel, err)
			}
			absRoot, _ := filepath.Abs(root)
			if got != absRoot && !filepath.IsAbs(got) {
				t.Fatalf("expected absolute path, got %q", got)
			}
		})
	}
}

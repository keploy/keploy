package contract

import (
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

func assertSchemaType(t *testing.T, schemaRef *openapi3.SchemaRef, expected string) {
	t.Helper()

	if schemaRef == nil || schemaRef.Value == nil || schemaRef.Value.Type == nil || !schemaRef.Value.Type.Is(expected) {
		t.Fatalf("expected type %q", expected)
	}
}

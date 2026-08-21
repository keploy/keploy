package contract

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func httpDocWith(method, reqBody, respBody string) models.HTTPDoc {
	return models.HTTPDoc{
		Version: "api.keploy.io/v1beta1",
		Kind:    "Http",
		Name:    "gen",
		Spec: models.HTTPSchema{
			Request: models.HTTPReq{Method: models.Method(method), URL: "http://example.com/users", Body: reqBody},
			Response: models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/json"},
				Body:       respBody,
			},
		},
	}
}

// Regression test for #4445: a valid JSON body whose root is an array must not
// crash the contract generator, and must produce a valid OpenAPI `type: array`
// schema with an `items` schema.
func TestHTTPDocToOpenAPI_TopLevelArrayResponse(t *testing.T) {
	svc := &contract{}
	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `[{"id":1,"name":"Utkarsh"}]`))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI failed on top-level array response body: %v", err)
	}

	got := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
	if got.Type != "array" {
		t.Fatalf("response schema type = %q, want \"array\"", got.Type)
	}
	if got.Items == nil {
		t.Fatal("array response schema has nil Items")
	}
	if got.Items.Type != "object" {
		t.Fatalf("array items type = %q, want \"object\"", got.Items.Type)
	}
	idProp, ok := got.Items.Properties["id"]
	if !ok {
		t.Fatalf("array items schema missing inferred property \"id\": %+v", got.Items.Properties)
	}
	// Pin the inferred type, not just the key: an items schema that names the
	// property but types it wrong still produces a spec that lies about the
	// endpoint. JSON numbers decode to float64, so "number" is the expected
	// inference for {"id":1}.
	if idProp["type"] != "number" {
		t.Errorf("array items property \"id\" type = %v, want \"number\"", idProp["type"])
	}

	// The generated document must be a valid OpenAPI spec.
	if err := validateSchema(oapi); err != nil {
		t.Fatalf("generated OpenAPI for array body is invalid: %v", err)
	}
}

// A top-level array request body must be handled the same way.
func TestHTTPDocToOpenAPI_TopLevelArrayRequest(t *testing.T) {
	svc := &contract{}
	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("POST", `[1,2,3]`, `{"ok":true}`))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI failed on top-level array request body: %v", err)
	}
	got := oapi.Paths["/users"].Post.RequestBody.Content["application/json"].Schema
	if got.Type != "array" || got.Items == nil || got.Items.Type != "number" {
		t.Fatalf("request schema = %+v, want type array with number items", got)
	}
	if err := validateSchema(oapi); err != nil {
		t.Fatalf("generated OpenAPI for array request body is invalid: %v", err)
	}
}

// An object root must keep producing an object schema (no behaviour change).
func TestHTTPDocToOpenAPI_ObjectResponseUnchanged(t *testing.T) {
	svc := &contract{}
	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `{"id":1,"name":"Utkarsh"}`))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI failed on object response body: %v", err)
	}
	got := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
	if got.Type != "object" {
		t.Fatalf("response schema type = %q, want \"object\"", got.Type)
	}
	if _, ok := got.Properties["id"]; !ok {
		t.Fatalf("object schema missing property \"id\": %+v", got.Properties)
	}
	if err := validateSchema(oapi); err != nil {
		t.Fatalf("generated OpenAPI for object body is invalid: %v", err)
	}
}

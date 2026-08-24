package contract

import (
	"testing"

	"go.uber.org/zap"
)

// TestHTTPDocToOpenAPI_NullValuesProduceValidSchemas covers bodies containing
// JSON nulls. getType mapped a null to "string" while the generated example
// kept the null, so kin-openapi rejected the document with `Value is not
// nullable` and contract generation aborted. A list endpoint returning
// [{"id":1,"deleted_at":null}] is the canonical case, and it failed even after
// top-level arrays were supported.
func TestHTTPDocToOpenAPI_NullValuesProduceValidSchemas(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "null field in array of objects", body: `[{"id":1,"deleted_at":null}]`},
		{name: "array of nulls", body: `[null]`},
		{name: "null before a real element", body: `[null,{"id":1}]`},
		{name: "null field in object", body: `{"a":null}`},
		{name: "null in nested object", body: `{"a":{"b":null}}`},
		{name: "null inside a nested array", body: `{"tags":[null,"x"]}`},
		{name: "null field in nested array of objects", body: `{"rows":[{"v":null}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &contract{}
			oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", tt.body))
			if err != nil {
				t.Fatalf("HTTPDocToOpenAPI failed on %s: %v", tt.body, err)
			}
			if err := validateSchema(oapi); err != nil {
				t.Fatalf("generated OpenAPI for %s is invalid: %v", tt.body, err)
			}
		})
	}
}

// TestHTTPDocToOpenAPI_NullItemsInferFromRealElement pins that a null does not
// poison item-type inference: the type comes from the first non-null element
// and the null only makes the items nullable.
func TestHTTPDocToOpenAPI_NullItemsInferFromRealElement(t *testing.T) {
	svc := &contract{}
	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `[null,{"id":1}]`))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI failed: %v", err)
	}

	got := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
	if got.Type != "array" {
		t.Fatalf("schema type = %q, want \"array\"", got.Type)
	}
	if got.Items == nil {
		t.Fatal("array schema has nil Items")
	}
	if got.Items.Type != "object" {
		t.Errorf("items type = %q, want \"object\" inferred from the non-null element", got.Items.Type)
	}
	if !got.Items.Nullable {
		t.Error("items should be marked nullable because the array contains a null")
	}
	if _, ok := got.Items.Properties["id"]; !ok {
		t.Errorf("items schema missing property \"id\": %+v", got.Items.Properties)
	}
}

// TestHTTPDocToOpenAPI_NoNullsStayNonNullable guards against marking everything
// nullable: a body without nulls must not gain the flag.
func TestHTTPDocToOpenAPI_NoNullsStayNonNullable(t *testing.T) {
	svc := &contract{}
	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `[{"id":1}]`))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI failed: %v", err)
	}
	got := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
	if got.Items == nil {
		t.Fatal("array schema has nil Items")
	}
	if got.Items.Nullable {
		t.Error("items marked nullable for an array with no nulls")
	}
	if _, ok := got.Items.Properties["id"]["nullable"]; ok {
		t.Error("non-null property gained a nullable flag")
	}
}

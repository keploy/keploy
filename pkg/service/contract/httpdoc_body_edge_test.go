package contract

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// TestHTTPDocToOpenAPI_EmptyBodiesKeepObjectExample guards the serialization of
// body-less docs. Supporting array roots meant decoding into interface{} rather
// than map[string]interface{}, and a nil interface marshals as `example: null`
// where the nil map used to marshal as `example: {}`. Body-less docs are the
// common case for GET endpoints, so that would have churned every regenerated
// contract for no reason.
func TestHTTPDocToOpenAPI_EmptyBodiesKeepObjectExample(t *testing.T) {
	svc := &contract{}
	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", ""))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI failed on empty bodies: %v", err)
	}

	media := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"]
	if media.Schema.Type != "object" {
		t.Errorf("response schema type = %q, want \"object\"", media.Schema.Type)
	}
	if media.Schema.Items != nil {
		t.Errorf("object schema must not carry Items, got %+v", media.Schema.Items)
	}

	out, err := yaml.Marshal(media)
	if err != nil {
		t.Fatalf("failed to marshal media type: %v", err)
	}
	if got, want := string(out), "example: {}"; !strings.Contains(got, want) {
		t.Errorf("serialized media type = %q, want it to contain %q", got, want)
	}

	if err := validateSchema(oapi); err != nil {
		t.Fatalf("generated OpenAPI for empty bodies is invalid: %v", err)
	}
}

// TestHTTPDocToOpenAPI_ArrayBodyEdgeCases covers the array roots the happy-path
// tests do not: an empty array (no element to infer from) and an array of
// arrays. OpenAPI requires every array schema to carry `items`, so a nil Items
// here would produce a spec that fails validation.
func TestHTTPDocToOpenAPI_ArrayBodyEdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantItems string
		nested    string // expected Items.Items type, "" when not an array of arrays
	}{
		{name: "empty array", body: `[]`, wantItems: "string"},
		{name: "array of strings", body: `["a","b"]`, wantItems: "string"},
		{name: "array of booleans", body: `[true]`, wantItems: "boolean"},
		{name: "array of arrays", body: `[[1,2]]`, wantItems: "array", nested: "integer"},
		{name: "array of floats", body: `[1.5]`, wantItems: "number"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &contract{}
			oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", tt.body))
			if err != nil {
				t.Fatalf("HTTPDocToOpenAPI failed on %s: %v", tt.name, err)
			}

			got := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
			if got.Type != "array" {
				t.Fatalf("schema type = %q, want \"array\"", got.Type)
			}
			if got.Items == nil {
				t.Fatal("array schema has nil Items; OpenAPI requires items on an array schema")
			}
			if got.Items.Type != tt.wantItems {
				t.Errorf("items type = %q, want %q", got.Items.Type, tt.wantItems)
			}
			if tt.nested != "" {
				if got.Items.Items == nil {
					t.Fatal("nested array schema has nil Items")
				}
				if got.Items.Items.Type != tt.nested {
					t.Errorf("nested items type = %q, want %q", got.Items.Items.Type, tt.nested)
				}
			}

			if err := validateSchema(oapi); err != nil {
				t.Fatalf("generated OpenAPI for %s is invalid: %v", tt.name, err)
			}
		})
	}
}

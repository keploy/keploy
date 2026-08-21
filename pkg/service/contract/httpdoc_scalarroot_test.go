package contract

import (
	"testing"

	"go.uber.org/zap"
)

// TestHTTPDocToOpenAPI_ScalarRoots covers JSON documents whose root is neither
// an object nor an array. SchemaForBody used to describe every non-array root
// as {type: object}, so the generated schema was violated by its own example
// and validateSchema aborted the whole contract generate run - the same failure
// mode as the array shapes, one root type over.
//
// An API answering `5`, `"ok"` or `true` is ordinary: JSON permits any value as
// a document.
func TestHTTPDocToOpenAPI_ScalarRoots(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "integer root", body: `5`, want: "integer"},
		{name: "float root", body: `1.5`, want: "number"},
		{name: "string root", body: `"hello"`, want: "string"},
		{name: "boolean root", body: `true`, want: "boolean"},
		{name: "large integer root", body: `9007199254740993`, want: "integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &contract{}
			oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", tt.body))
			if err != nil {
				t.Fatalf("HTTPDocToOpenAPI failed on %s: %v", tt.body, err)
			}
			got := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
			if got.Type != tt.want {
				t.Errorf("root schema type = %q, want %q", got.Type, tt.want)
			}
			if err := validateSchema(oapi); err != nil {
				t.Fatalf("generated OpenAPI for %s is invalid: %v", tt.body, err)
			}
		})
	}
}

// TestHTTPDocToOpenAPI_ObjectAndEmptyRootsUnchanged pins the two roots that must
// keep their old shape: an object root still carries the caller's extracted
// properties, and a body-less doc still serializes its example as {}.
func TestHTTPDocToOpenAPI_ObjectAndEmptyRootsUnchanged(t *testing.T) {
	svc := &contract{}

	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `{"id":1}`))
	if err != nil {
		t.Fatalf("object root failed: %v", err)
	}
	got := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
	if got.Type != "object" || got.Properties["id"]["type"] != "integer" {
		t.Errorf("object root = %+v, want type object with id integer", got)
	}

	empty, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", ""))
	if err != nil {
		t.Fatalf("empty body failed: %v", err)
	}
	gotEmpty := empty.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
	if gotEmpty.Type != "object" {
		t.Errorf("empty body root type = %q, want object", gotEmpty.Type)
	}
}

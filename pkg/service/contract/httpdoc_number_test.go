package contract

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// TestHTTPDocToOpenAPI_NumberInference pins how JSON numbers are typed.
// Decoding into interface{} makes every number a float64, so getType's
// int/int32/int64 case was unreachable and every integer field in every
// generated contract came out as "number" - a client generated from the
// contract got a float where the API returns an int.
func TestHTTPDocToOpenAPI_NumberInference(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
		want  string
	}{
		{name: "integer literal", body: `{"id":1}`, field: "id", want: "integer"},
		{name: "negative integer", body: `{"delta":-7}`, field: "delta", want: "integer"},
		{name: "zero", body: `{"count":0}`, field: "count", want: "integer"},
		{name: "fractional literal", body: `{"price":1.5}`, field: "price", want: "number"},
		// 1.0 is written as a float, so it stays a float: the literal is the
		// only signal available about the field's intent.
		{name: "explicit float zero fraction", body: `{"ratio":1.0}`, field: "ratio", want: "number"},
		{name: "exponent notation", body: `{"big":1e10}`, field: "big", want: "number"},
		// Above int64 there is no exact representation available, so it
		// falls back to the float64 every decode produced before this change.
		// Precise-integer range widens from 2^53 to 2^63; it is not unbounded.
		{name: "beyond int64 falls back to number", body: `{"huge":123456789012345678901234567890}`, field: "huge", want: "number"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &contract{}
			oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", tt.body))
			if err != nil {
				t.Fatalf("HTTPDocToOpenAPI failed on %s: %v", tt.body, err)
			}
			got := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
			if got.Properties[tt.field]["type"] != tt.want {
				t.Errorf("%s type = %v, want %q", tt.field, got.Properties[tt.field]["type"], tt.want)
			}
			if err := validateSchema(oapi); err != nil {
				t.Fatalf("generated OpenAPI for %s is invalid: %v", tt.body, err)
			}
		})
	}
}

// TestHTTPDocToOpenAPI_LargeIntegerPrecision guards the example, not the
// schema. Every JSON number used to become a float64, so an id above 2^53 was
// silently rounded on its way into the contract: 9007199254740993 was written
// out as 9.007199254740992e+15. That is a corrupted record of what the API
// actually returned.
func TestHTTPDocToOpenAPI_LargeIntegerPrecision(t *testing.T) {
	const id = "9007199254740993" // 2^53 + 1, not representable as a float64

	svc := &contract{}
	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `{"id":`+id+`}`))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI failed: %v", err)
	}

	media := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"]
	out, err := yaml.Marshal(media)
	if err != nil {
		t.Fatalf("failed to marshal media type: %v", err)
	}
	if !strings.Contains(string(out), id) {
		t.Errorf("example lost precision; serialized media type = %q, want it to contain %s", string(out), id)
	}
	if got := media.Schema.Properties["id"]["type"]; got != "integer" {
		t.Errorf("id type = %v, want \"integer\"", got)
	}

	if err := validateSchema(oapi); err != nil {
		t.Fatalf("generated OpenAPI is invalid: %v", err)
	}
}

// TestHTTPDocToOpenAPI_MixedNumericArrays covers arrays that mix integer and
// fractional literals. Item schemas are inferred from one element while the
// example carries the whole body, so inferring "integer" from a leading 1 makes
// kin-openapi reject the 1.5 that follows - and a validateSchema failure aborts
// the entire contract generate run, so one {"prices":[10, 9.99]} would take the
// whole command down.
func TestHTTPDocToOpenAPI_MixedNumericArrays(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "integer before float at array root", body: `[1,1.5]`},
		{name: "float before integer at array root", body: `[1.5,1]`},
		{name: "mixed inside an object field", body: `{"prices":[10,9.99]}`},
		{name: "mixed across objects in an array", body: `[{"a":1},{"a":1.5}]`},
		{name: "mixed in a nested object", body: `[{"a":{"b":1}},{"a":{"b":1.5}}]`},
		{name: "mixed in a nested array", body: `[{"v":[1]},{"v":[1.5]}]`},
		{name: "null before the mixed pair", body: `[null,1,1.5]`},
		// A null shifts the sibling's fractional element to a different index
		// than the sample's integer. Anything that widens by walking the two
		// arrays positionally misses these.
		{name: "null misaligns nested arrays", body: `[[null,1],[1.5]]`},
		{name: "nulls on both sides of nested arrays", body: `[[null,1],[1.5,null]]`},
		{name: "null misaligns an array inside objects", body: `[{"v":[null,1]},{"v":[1.5]}]`},
		{name: "null misaligns a nested array in an object field", body: `{"r":[{"v":[null,1]},{"v":[1.5]}]}`},
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

// TestHTTPDocToOpenAPI_UnrepresentableNumberIsAnError pins that a literal which
// overflows float64 stays a hard error. Recording it as a string field would be
// a quieter but worse outcome than the error plain json.Unmarshal already gave.
func TestHTTPDocToOpenAPI_UnrepresentableNumberIsAnError(t *testing.T) {
	svc := &contract{}
	if _, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `{"x":1e400}`)); err == nil {
		t.Fatal("HTTPDocToOpenAPI returned nil error for a number that overflows float64")
	}
}

// TestHTTPDocToOpenAPI_TrailingDataIsAnError pins that a concatenated body is
// rejected. json.Unmarshal rejected it; json.Decoder does not, so without an
// explicit check the contract would silently describe only the first document.
func TestHTTPDocToOpenAPI_TrailingDataIsAnError(t *testing.T) {
	svc := &contract{}
	if _, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `{"a":1}{"b":2}`)); err == nil {
		t.Fatal("HTTPDocToOpenAPI returned nil error for a body with trailing data")
	}
}

// TestHTTPDocToOpenAPI_WideningKeepsValuesExact guards the interaction between
// the two fixes in this change: widening an array's item schema must not be
// done by rewriting the recorded numbers, or the large-integer precision this
// change exists to preserve is lost again on any array that mixes the two.
func TestHTTPDocToOpenAPI_WideningKeepsValuesExact(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1

	svc := &contract{}
	oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("GET", "", `[1.5,`+big+`]`))
	if err != nil {
		t.Fatalf("HTTPDocToOpenAPI failed: %v", err)
	}

	media := oapi.Paths["/users"].Get.Responses["200"].Content["application/json"]
	if media.Schema.Items == nil || media.Schema.Items.Type != "number" {
		t.Fatalf("items = %+v, want type number (widened by the 1.5)", media.Schema.Items)
	}
	out, err := yaml.Marshal(media)
	if err != nil {
		t.Fatalf("failed to marshal media type: %v", err)
	}
	if !strings.Contains(string(out), big) {
		t.Errorf("widening rewrote the recorded value; serialized = %q, want it to contain %s", string(out), big)
	}
	if err := validateSchema(oapi); err != nil {
		t.Fatalf("generated OpenAPI is invalid: %v", err)
	}
}

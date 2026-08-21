package contract

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func postCase(url, reqBody, respBody string, status int) models.TestCase {
	return models.TestCase{
		HTTPReq:  models.HTTPReq{Method: "POST", URL: url, Body: reqBody},
		HTTPResp: models.HTTPResp{StatusCode: status, Body: respBody},
	}
}

// inferredSchemas runs bodies through InferSchema and returns the request and
// response schemas for POST /y, after asserting the document is valid.
func inferredSchemas(t *testing.T, cases ...models.TestCase) (req, resp *openapi3.Schema) {
	t.Helper()
	doc, err := InferSchema(cases)
	if err != nil {
		t.Fatalf("InferSchema failed: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("InferSchema produced an invalid OpenAPI document: %v", err)
	}
	op := doc.Paths.Value("/y").Post
	if op.RequestBody != nil {
		req = op.RequestBody.Value.Content["application/json"].Schema.Value
	}
	if r := op.Responses.Value("200"); r != nil && r.Value.Content != nil {
		resp = r.Value.Content["application/json"].Schema.Value
	}
	return req, resp
}

func typeOf(t *testing.T, s *openapi3.Schema) string {
	t.Helper()
	if s == nil {
		return "<nil>"
	}
	if s.Type == nil {
		return ""
	}
	return s.Type.Slice()[0]
}

// TestInferSchema_IntegersAreIntegers covers the defect directly: InferSchema
// decoded with encoding/json, so every JSON number arrived as a float64 and
// hit `case float64` -> NewFloat64Schema. An id of 1 was typed "number".
func TestInferSchema_IntegersAreIntegers(t *testing.T) {
	req, resp := inferredSchemas(t, postCase("http://x/y", `{"id":1,"price":1.5}`, `{"id":1,"price":1.5}`, 200))

	for name, schema := range map[string]*openapi3.Schema{"request": req, "response": resp} {
		if got := typeOf(t, schema.Properties["id"].Value); got != "integer" {
			t.Errorf("%s id type = %q, want \"integer\"", name, got)
		}
		if got := typeOf(t, schema.Properties["price"].Value); got != "number" {
			t.Errorf("%s price type = %q, want \"number\"", name, got)
		}
	}
}

// TestInferSchema_LargeIntegerStaysInteger guards the decoder swap rather than
// the type switch: an id above 2^53 is only distinguishable from a float if the
// body was decoded with UseNumber in the first place.
func TestInferSchema_LargeIntegerStaysInteger(t *testing.T) {
	_, resp := inferredSchemas(t, postCase("http://x/y", "", `{"id":9007199254740993}`, 200))
	if got := typeOf(t, resp.Properties["id"].Value); got != "integer" {
		t.Errorf("id type = %q, want \"integer\"", got)
	}
}

// TestInferSchema_AgreesWithHTTPDocToOpenAPI is the reason this fix exists.
//
// `keploy contract generate` has two schema surfaces: HTTPDocToOpenAPI writes
// the schema/ directory the matcher consumes, and InferSchema (behind --infer)
// writes keploy/contract.yaml. They were separate implementations, and they
// disagreed on the same body. This test fails the moment they drift apart
// again, which no test of either one on its own can do.
func TestInferSchema_AgreesWithHTTPDocToOpenAPI(t *testing.T) {
	bodies := []string{
		`{"id":1,"price":1.5,"name":"x","ok":true}`,
		`{"id":9007199254740993}`,
		`{"nested":{"count":2,"ratio":0.5}}`,
		`{"prices":[10,9.99]}`,
		`{"ids":[1,2,3]}`,
		`{"empty":[]}`,
		`{"maybe":null}`,
		`{"rows":[{"id":1},{"id":1.5}]}`,
	}

	svc := &contract{}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			_, inferred := inferredSchemas(t, postCase("http://x/y", "", body, 200))

			oapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), httpDocWith("POST", "", body))
			if err != nil {
				t.Fatalf("HTTPDocToOpenAPI failed: %v", err)
			}
			generated := oapi.Paths["/users"].Post.Responses["200"].Content["application/json"].Schema

			for name, prop := range inferred.Properties {
				got := typeOf(t, prop.Value)
				want, _ := generated.Properties[name]["type"].(string)
				if got != want {
					t.Errorf("property %q: InferSchema says %q, HTTPDocToOpenAPI says %q", name, got, want)
				}
			}
		})
	}
}

// TestInferSchema_ArrayItemsFoldOverEveryElement asserts InferSchema no longer
// describes an array from its first element alone.
func TestInferSchema_ArrayItemsFoldOverEveryElement(t *testing.T) {
	tests := []struct {
		body      string
		wantItems string
	}{
		{`[1,1.5]`, "number"},
		// Must not over-widen: a homogeneous integer array stays integer.
		{`[1,2]`, "integer"},
		{`[1.5,1]`, "number"},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			_, resp := inferredSchemas(t, postCase("http://x/y", "", tt.body, 200))
			if got := typeOf(t, resp.Items.Value); got != tt.wantItems {
				t.Errorf("items type = %q, want %q", got, tt.wantItems)
			}
		})
	}

	t.Run("object items widen per property", func(t *testing.T) {
		_, resp := inferredSchemas(t, postCase("http://x/y", "", `[{"a":1},{"a":1.5}]`, 200))
		if got := typeOf(t, resp.Items.Value.Properties["a"].Value); got != "number" {
			t.Errorf("items.a type = %q, want \"number\"", got)
		}
	})

	t.Run("null makes items nullable without pinning a type", func(t *testing.T) {
		_, resp := inferredSchemas(t, postCase("http://x/y", "", `[null,1]`, 200))
		if got := typeOf(t, resp.Items.Value); got != "integer" {
			t.Errorf("items type = %q, want \"integer\"", got)
		}
		if !resp.Items.Value.Nullable {
			t.Error("items are not nullable, but the array contained a null")
		}
	})
}

// TestInferSchema_IsOrderIndependent is the assertion that pins the merge fix.
//
// Request bodies were first-wins and responses last-wins - two opposite
// policies in one function. That was invisible while every number was a
// float64; once integers are typed as integers, a price of 10 in one test case
// and 9.99 in another produces a different contract depending on which test
// case is walked first, and contract.go only sorts test-set IDs, so ordering
// within a set decides.
func TestInferSchema_IsOrderIndependent(t *testing.T) {
	first := postCase("http://x/y", `{"n":1}`, `{"n":1}`, 200)
	second := postCase("http://x/y", `{"n":1.5}`, `{"n":1.5}`, 200)

	forward, err := InferSchema([]models.TestCase{first, second})
	if err != nil {
		t.Fatalf("InferSchema failed: %v", err)
	}
	backward, err := InferSchema([]models.TestCase{second, first})
	if err != nil {
		t.Fatalf("InferSchema failed: %v", err)
	}

	forwardYAML, err := yamlLib.Marshal(forward)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	backwardYAML, err := yamlLib.Marshal(backward)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if string(forwardYAML) != string(backwardYAML) {
		t.Errorf("contract depends on test case order:\n--- first then second ---\n%s\n--- second then first ---\n%s", forwardYAML, backwardYAML)
	}

	// And the merged answer must be the one that describes both bodies.
	req, resp := inferredSchemas(t, first, second)
	if got := typeOf(t, req.Properties["n"].Value); got != "number" {
		t.Errorf("request n type = %q, want \"number\" (1 and 1.5 were both observed)", got)
	}
	if got := typeOf(t, resp.Properties["n"].Value); got != "number" {
		t.Errorf("response n type = %q, want \"number\" (1 and 1.5 were both observed)", got)
	}
}

// TestInferSchema_LaterObservationDoesNotEraseEarlierOne pins the other half of
// the overwrite bug: a response was Set unconditionally, so a test case with no
// body replaced the schema a previous one had contributed.
func TestInferSchema_LaterObservationDoesNotEraseEarlierOne(t *testing.T) {
	req, resp := inferredSchemas(t,
		postCase("http://x/y", `{"n":1}`, `{"n":1}`, 200),
		postCase("http://x/y", "", "", 200),
	)
	if req == nil || req.Properties["n"] == nil {
		t.Error("request schema was erased by a later body-less test case")
	}
	if resp == nil || resp.Properties["n"] == nil {
		t.Error("response schema was erased by a later body-less test case")
	}
}

// TestInferSchema_ConflictingObservations pins what a genuine type conflict
// produces. OpenAPI 3.0 cannot express "string or integer" without oneOf, so an
// untyped schema is emitted - a deliberate choice, and changing it should be a
// deliberate test edit rather than a silent drift.
func TestInferSchema_ConflictingObservations(t *testing.T) {
	_, resp := inferredSchemas(t,
		postCase("http://x/y", "", `{"v":1}`, 200),
		postCase("http://x/y", "", `{"v":"one"}`, 200),
	)
	if got := typeOf(t, resp.Properties["v"].Value); got != "" {
		t.Errorf("conflicting observations gave type %q, want an untyped schema", got)
	}
}

// TestInferSchema_UndescribableBodyIsSkipped asserts a body the decoder rejects
// contributes no content rather than aborting or being recorded as a string.
// decodeJSONBody rejects a number that overflows float64, as encoding/json did,
// and additionally rejects trailing data after the first JSON document.
func TestInferSchema_UndescribableBodyIsSkipped(t *testing.T) {
	for _, body := range []string{`{"a":1e400}`, `{"a":1}{"b":2}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			doc, err := InferSchema([]models.TestCase{postCase("http://x/y", body, body, 200)})
			if err != nil {
				t.Fatalf("InferSchema failed: %v", err)
			}
			if err := doc.Validate(context.Background()); err != nil {
				t.Fatalf("InferSchema produced an invalid OpenAPI document: %v", err)
			}
			op := doc.Paths.Value("/y").Post
			if op.RequestBody != nil {
				t.Error("an undescribable request body produced request content")
			}
			if r := op.Responses.Value("200"); r == nil || r.Value.Content != nil {
				t.Error("an undescribable response body produced response content")
			}
		})
	}
}

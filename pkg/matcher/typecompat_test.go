package matcher

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func opWithField(typ string) *models.Operation {
	props := func() map[string]map[string]interface{} {
		return map[string]map[string]interface{}{"id": {"type": typ}}
	}
	return &models.Operation{
		RequestBody: &models.RequestBody{
			Content: map[string]models.MediaType{
				"application/json": {Schema: models.Schema{Type: "object", Properties: props()}},
			},
		},
		Responses: map[string]models.ResponseItem{
			"200": {Content: map[string]models.MediaType{
				"application/json": {Schema: models.Schema{Type: "object", Properties: props()}},
			}},
		},
	}
}

// TestMarshalBodies_NormalizesIntegerAndNumber covers all four normalisation
// sites - mock and test, request and response - by asserting on the marshalled
// strings the comparators actually diff.
//
// Asserting through Match() alone is not enough: in IdentifyMode the response
// path scores via calculateSimilarityScore, which reads Schema.Properties
// directly, so the response marshalling is only exercised by the CompareMode
// diff - and CompareMode reports the diff by rendering it, not through its
// return value, so a Match()-level test cannot observe it.
func TestMarshalBodies_NormalizesIntegerAndNumber(t *testing.T) {
	tests := []struct {
		name      string
		mockType  string
		testType  string
		wantEqual bool
	}{
		{name: "old number provider vs new integer consumer", mockType: "number", testType: "integer", wantEqual: true},
		{name: "new integer provider vs old number consumer", mockType: "integer", testType: "number", wantEqual: true},
		{name: "same version", mockType: "integer", testType: "integer", wantEqual: true},
		{name: "a real type difference still differs", mockType: "number", testType: "string", wantEqual: false},
		{name: "integer vs string still differs", mockType: "integer", testType: "string", wantEqual: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockOp, testOp := opWithField(tt.mockType), opWithField(tt.testType)

			mockReq, testReq, err := MarshalRequestBodies(mockOp, testOp)
			if err != nil {
				t.Fatalf("MarshalRequestBodies failed: %v", err)
			}
			if (mockReq == testReq) != tt.wantEqual {
				t.Errorf("request bodies: mock=%s test=%s, equal=%v want %v", mockReq, testReq, mockReq == testReq, tt.wantEqual)
			}

			mockResp, testResp, err := MarshalResponseBodies("200", mockOp, testOp)
			if err != nil {
				t.Fatalf("MarshalResponseBodies failed: %v", err)
			}
			if (mockResp == testResp) != tt.wantEqual {
				t.Errorf("response bodies: mock=%s test=%s, equal=%v want %v", mockResp, testResp, mockResp == testResp, tt.wantEqual)
			}
		})
	}
}

// TestNormalizeSchemaProperties_DeepCopiesAndPreservesNil guards the two
// properties the comparators rely on: the caller's loaded contract must not be
// mutated, and a schema with no properties must keep marshalling as null rather
// than {}.
func TestNormalizeSchemaProperties_DeepCopiesAndPreservesNil(t *testing.T) {
	if got := normalizeSchemaProperties(nil); got != nil {
		t.Errorf("normalizeSchemaProperties(nil) = %v, want nil", got)
	}

	in := map[string]map[string]interface{}{
		"id":  {"type": "integer"},
		"obj": {"type": "object", "properties": map[string]map[string]interface{}{"n": {"type": "integer"}}},
		"arr": {"type": "array", "items": map[string]interface{}{"type": "integer"}},
	}
	out := normalizeSchemaProperties(in)

	if out["id"]["type"] != "number" {
		t.Errorf("id type = %v, want number", out["id"]["type"])
	}
	if in["id"]["type"] != "integer" {
		t.Errorf("input was mutated: id type = %v, want integer", in["id"]["type"])
	}

	nested, _ := out["obj"]["properties"].(map[string]map[string]interface{})
	if nested == nil || nested["n"]["type"] != "number" {
		t.Errorf("nested property not normalized: %v", out["obj"]["properties"])
	}
	inNested, _ := in["obj"]["properties"].(map[string]map[string]interface{})
	if inNested["n"]["type"] != "integer" {
		t.Errorf("input nested property was mutated: %v", inNested["n"]["type"])
	}

	items, _ := out["arr"]["items"].(map[string]interface{})
	if items == nil || items["type"] != "number" {
		t.Errorf("array items not normalized: %v", out["arr"]["items"])
	}
	inItems, _ := in["arr"]["items"].(map[string]interface{})
	if inItems["type"] != "integer" {
		t.Errorf("input array items were mutated: %v", inItems["type"])
	}
}

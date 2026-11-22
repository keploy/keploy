package schema

import (
	"math"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// objectSchema, arraySchema and scalarSchema build the response schema shapes
// `keploy contract generate` emits for an object root, an array root and a
// scalar root respectively.
func objectSchema(props map[string]string) models.Schema {
	s := models.Schema{Type: "object"}
	if props != nil {
		s.Properties = make(map[string]map[string]interface{}, len(props))
		for name, typ := range props {
			s.Properties[name] = map[string]interface{}{"type": typ}
		}
	}
	return s
}

func arraySchema(items models.Schema) models.Schema {
	return models.Schema{Type: "array", Items: &items}
}

func scalarSchema(typ string) models.Schema { return models.Schema{Type: typ} }

// opWithResponse builds an operation whose request body is fixed (so
// compareRequestBodies always admits the candidate and the response score is
// what is under test) and whose 200 response carries the given schema.
func opWithResponse(resp models.Schema) *models.Operation {
	req := objectSchema(map[string]string{"q": "string"})
	return &models.Operation{
		RequestBody: &models.RequestBody{
			Content: map[string]models.MediaType{"application/json": {Schema: req}},
		},
		Responses: map[string]models.ResponseItem{
			"200": {Content: map[string]models.MediaType{"application/json": {Schema: resp}}},
		},
	}
}

// classify mirrors the trichotomy in consumer.ValidateMockAgainstTests: a mock
// is PASSED at exactly 1.0, FAILED above 0, and MISSED at 0 - and the gate in
// scoresForMocks only ever records a score that is both passing and strictly
// better than the current one, so anything it rejects leaves the mock at its
// initial 0.0 and it prints as MISSED.
//
// The product's trichotomy is not total: a NaN matches none of its three
// branches. This helper is total on purpose, so a score that falls through
// every branch is reported as "unclassified" instead of quietly vanishing.
func classify(score float64, pass bool) string {
	if !pass || !(score > 0.0) {
		// Includes NaN, which loses every comparison.
		return "missed"
	}
	switch {
	case score == 1.0:
		return "passed"
	case score > 0.0 && score < 1.0:
		return "failed"
	default:
		return "unclassified"
	}
}

// TestMatch_ScoreIsAlwaysFinite is the regression test for the NaN score.
//
// The score used to be differencesCount/len(mockSchema.Properties). An array
// root, a scalar root, a body-less response and an object with no fields all
// have zero properties, so that expression was 0/0 for every one of them. NaN
// loses every comparison, including the `candidateScore > best` gate in
// consumer.scoresForMocks, so those mocks could never be recorded as matched:
// they were reported MISSED against every test case, however well they matched.
//
// Each case asserts the score is finite before comparing it. NaN != want is
// true, so a plain inequality check does fail on NaN - but it reports
// "want 1, got NaN", which reads like an ordinary scoring change rather than
// the arithmetic defect it is.
func TestMatch_ScoreIsAlwaysFinite(t *testing.T) {
	tests := []struct {
		name       string
		mock, test models.Schema
		wantScore  float64
		wantClass  string
	}{
		{
			name: "array root matching itself",
			mock: arraySchema(objectSchema(map[string]string{"id": "integer"})),
			test: arraySchema(objectSchema(map[string]string{"id": "integer"})),
			// The whole point of #4447: array-root contracts are reachable, and
			// a perfect match has to be reportable as one.
			wantScore: 1, wantClass: "passed",
		},
		{
			name: "array root where the test carries an extra field",
			mock: arraySchema(objectSchema(map[string]string{"id": "integer"})),
			test: arraySchema(objectSchema(map[string]string{"id": "integer", "name": "string"})),
			// The mock is what must be satisfied; extra fields on the test side
			// are not a defect, exactly as for object roots.
			wantScore: 1, wantClass: "passed",
		},
		{
			name: "array root missing one of the mock's item fields",
			mock: arraySchema(objectSchema(map[string]string{"id": "integer", "name": "string"})),
			test: arraySchema(objectSchema(map[string]string{"id": "integer"})),
			// Must discriminate: a flat 1.0 for "no properties at the root"
			// would make every array-root mock tie with every other.
			wantScore: 0.5, wantClass: "failed",
		},
		{
			name:      "array root with disjoint item fields",
			mock:      arraySchema(objectSchema(map[string]string{"id": "integer"})),
			test:      arraySchema(objectSchema(map[string]string{"name": "string"})),
			wantScore: 0, wantClass: "missed",
		},
		{
			name:      "array of integer vs array of string",
			mock:      arraySchema(scalarSchema("integer")),
			test:      arraySchema(scalarSchema("string")),
			wantScore: 0, wantClass: "missed",
		},
		{
			name: "array of integer vs array of number across keploy versions",
			mock: arraySchema(scalarSchema("integer")),
			test: arraySchema(scalarSchema("number")),
			// Same version-skew allowance sameSchemaType makes for properties;
			// it has to survive the recursion into Items.
			wantScore: 1, wantClass: "passed",
		},
		{
			name:      "nested array roots",
			mock:      arraySchema(arraySchema(scalarSchema("integer"))),
			test:      arraySchema(arraySchema(scalarSchema("integer"))),
			wantScore: 1, wantClass: "passed",
		},
		{
			name:      "object with no fields on both sides",
			mock:      objectSchema(nil),
			test:      objectSchema(nil),
			wantScore: 1, wantClass: "passed",
		},
		{
			name: "no JSON response body on either side",
			mock: models.Schema{},
			test: models.Schema{},
			// A 204, or a DELETE with an empty body. Path, method and status
			// all agree and there is no body to disagree about.
			wantScore: 1, wantClass: "passed",
		},
		{
			name: "object mock vs array test",
			mock: objectSchema(map[string]string{"id": "integer"}),
			test: arraySchema(objectSchema(map[string]string{"id": "integer"})),
			// This one used to score 1.0 and report PASSED - a false green. The
			// two schemas are not the same response shape.
			wantScore: 0, wantClass: "missed",
		},
		{
			name:      "array mock vs object test",
			mock:      arraySchema(objectSchema(map[string]string{"id": "integer"})),
			test:      objectSchema(map[string]string{"id": "integer"}),
			wantScore: 0, wantClass: "missed",
		},
		{
			name: "object mock vs test with no response body",
			mock: objectSchema(map[string]string{"id": "integer"}),
			test: models.Schema{},
			// Also a false green before: a JSON mock scored 1.0 against a test
			// that returned no body at all.
			wantScore: 0, wantClass: "missed",
		},
		{
			name:      "object roots still score as they always did",
			mock:      objectSchema(map[string]string{"a": "string", "b": "string"}),
			test:      objectSchema(map[string]string{"a": "string"}),
			wantScore: 0.5, wantClass: "failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, pass, err := Match(docWith(opWithResponse(tt.mock)), docWith(opWithResponse(tt.test)),
				"test-set-0", "mock-set-0", zap.NewNop(), models.IdentifyMode)
			if err != nil {
				t.Fatalf("Match failed: %v", err)
			}
			// Check finiteness first and separately, so the failure names the
			// real problem instead of looking like a changed expectation.
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("score is not a finite number: %v (NaN and +/-Inf lose every comparison, so this mock can never be recorded as matched)", got)
			}
			if got != tt.wantScore {
				t.Errorf("score = %v, want %v", got, tt.wantScore)
			}
			if gotClass := classify(got, pass); gotClass != tt.wantClass {
				t.Errorf("consumer would report %q, want %q (score %v, pass %v)", gotClass, tt.wantClass, got, pass)
			}
		})
	}
}

// TestMatch_MissingStatusIsAFiniteSentinel pins the other arithmetic hazard in
// the same expression: the "test never returned this status" sentinel was -1,
// and dividing it by the mock's property count turned it into -Inf, or into NaN
// when the mock had no properties for the sentinel to be divided by.
func TestMatch_MissingStatusIsAFiniteSentinel(t *testing.T) {
	for _, tt := range []struct {
		name string
		mock models.Schema
	}{
		{name: "object root", mock: objectSchema(map[string]string{"id": "integer"})},
		{name: "array root", mock: arraySchema(scalarSchema("integer"))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testOp := opWithResponse(objectSchema(map[string]string{"id": "integer"}))
			testOp.Responses = map[string]models.ResponseItem{
				"404": testOp.Responses["200"],
			}

			got, pass, err := Match(docWith(opWithResponse(tt.mock)), docWith(testOp),
				"test-set-0", "mock-set-0", zap.NewNop(), models.IdentifyMode)
			if err != nil {
				t.Fatalf("Match failed: %v", err)
			}
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("score is not a finite number: %v", got)
			}
			if got != NOTCANDIDATE {
				t.Errorf("score = %v, want %v (NOTCANDIDATE)", got, NOTCANDIDATE)
			}
			if pass {
				t.Error("pass = true, want false: the test case never returned the mock's status code")
			}
		})
	}
}

// TestSchemaSimilarity_Polarity asserts the score means similarity, not
// difference. Individually plausible numbers survive an inverted polarity - the
// ordering does not, and neither does the perfect/disjoint pair. One branch of
// the old code assigned a variable literally named differencesCount into the
// slot the ratio was computed from, so this is not a hypothetical mix-up.
func TestSchemaSimilarity_Polarity(t *testing.T) {
	mock := objectSchema(map[string]string{"a": "string", "b": "string", "c": "string", "d": "string"})

	perfect := schemaSimilarity(mock, objectSchema(map[string]string{"a": "string", "b": "string", "c": "string", "d": "string"}))
	partial := schemaSimilarity(mock, objectSchema(map[string]string{"a": "string", "b": "string"}))
	disjoint := schemaSimilarity(mock, objectSchema(map[string]string{"z": "string"}))
	shapeMismatch := schemaSimilarity(mock, arraySchema(scalarSchema("string")))

	for name, score := range map[string]float64{
		"perfect": perfect, "partial": partial, "disjoint": disjoint, "shapeMismatch": shapeMismatch,
	} {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			t.Fatalf("%s score is not a finite number: %v", name, score)
		}
	}

	if perfect != 1.0 {
		t.Errorf("a test covering the mock completely scored %v, want 1", perfect)
	}
	if disjoint != 0.0 {
		t.Errorf("a test sharing no field with the mock scored %v, want 0", disjoint)
	}
	if shapeMismatch != 0.0 {
		t.Errorf("a test of a different root shape scored %v, want 0", shapeMismatch)
	}
	if !(perfect > partial && partial > disjoint) {
		t.Errorf("score is not ordered by similarity: perfect=%v partial=%v disjoint=%v", perfect, partial, disjoint)
	}
}

package consumer

import (
	"math"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// arrayRootDoc builds the contract `keploy contract generate` emits for an
// endpoint whose response body is a JSON array - the shape #4447 made
// reachable.
func arrayRootDoc(title string, itemProps map[string]string) *models.OpenAPI {
	props := make(map[string]map[string]interface{}, len(itemProps))
	for name, typ := range itemProps {
		props[name] = map[string]interface{}{"type": typ}
	}
	op := &models.Operation{
		RequestBody: &models.RequestBody{
			Content: map[string]models.MediaType{
				"application/json": {Schema: models.Schema{Type: "object"}},
			},
		},
		Responses: map[string]models.ResponseItem{
			"200": {Content: map[string]models.MediaType{
				"application/json": {Schema: models.Schema{
					Type:  "array",
					Items: &models.Schema{Type: "object", Properties: props},
				}},
			}},
		},
	}
	return &models.OpenAPI{
		OpenAPI: "3.0.0",
		Info:    models.Info{Title: title, Version: "1.0.0"},
		Paths:   map[string]models.PathItem{"/users": {Get: op}},
	}
}

// TestScoresForMocks_ArrayRootMockIsRecorded is the end-to-end assertion for the
// NaN score, at the layer that actually swallowed it.
//
// scoresForMocks seeds every mock at 0.0 and only overwrites that when
// `pass && candidateScore > current`. A NaN loses that comparison, so an
// array-root mock kept its seeded 0.0 and ValidateMockAgainstTests printed it
// as MISSED - with no test set and no test name attached, because the branch
// that records them never ran. A unit test on the score alone cannot catch a
// regression here; only going through this gate can.
func TestScoresForMocks_ArrayRootMockIsRecorded(t *testing.T) {
	s := &consumer{logger: zap.NewNop(), config: &config.Config{}}

	mock := arrayRootDoc("mock-users", map[string]string{"id": "integer", "name": "string"})
	matching := arrayRootDoc("test-users", map[string]string{"id": "integer", "name": "string"})

	mockSet := map[string]models.SchemaInfo{}
	s.scoresForMocks([]*models.OpenAPI{mock}, mockSet,
		map[string]map[string]*models.OpenAPI{"test-set-0": {"test-users": matching}}, "mock-set-0")

	got, ok := mockSet["mock-users"]
	if !ok {
		t.Fatal("mock is missing from the score set entirely")
	}
	if math.IsNaN(got.Score) || math.IsInf(got.Score, 0) {
		t.Fatalf("recorded score is not a finite number: %v", got.Score)
	}
	if got.Score != 1.0 {
		t.Errorf("score = %v, want 1 (an array-root mock matching an identical test must be recorded as passed)", got.Score)
	}
	if got.Name != "test-users" {
		t.Errorf("matched test name = %q, want %q: the gate never recorded which test matched", got.Name, "test-users")
	}
	if got.TestSetID != "test-set-0" {
		t.Errorf("matched test set = %q, want %q", got.TestSetID, "test-set-0")
	}
}

// TestScoresForMocks_BestCandidateWins asserts the score still discriminates
// between candidates once array roots are scorable. Returning a flat 1.0 for
// "the root schema has no properties" would fix the NaN while making every
// array-root mock tie with every array-root test, leaving the winner to Go's
// map iteration order.
func TestScoresForMocks_BestCandidateWins(t *testing.T) {
	s := &consumer{logger: zap.NewNop(), config: &config.Config{}}

	mock := arrayRootDoc("mock-users", map[string]string{"id": "integer", "name": "string"})
	best := arrayRootDoc("test-full", map[string]string{"id": "integer", "name": "string"})
	partial := arrayRootDoc("test-partial", map[string]string{"id": "integer"})

	for _, order := range []map[string]*models.OpenAPI{
		{"test-full": best, "test-partial": partial},
		{"test-partial": partial, "test-full": best},
	} {
		mockSet := map[string]models.SchemaInfo{}
		s.scoresForMocks([]*models.OpenAPI{mock}, mockSet,
			map[string]map[string]*models.OpenAPI{"test-set-0": order}, "mock-set-0")

		got := mockSet["mock-users"]
		if got.Score != 1.0 || got.Name != "test-full" {
			t.Errorf("best candidate = %q at score %v, want \"test-full\" at 1", got.Name, got.Score)
		}
	}
}

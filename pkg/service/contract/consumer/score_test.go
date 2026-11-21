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

// objectRootDoc builds the same contract for an endpoint whose response body is
// a JSON object, on the given path. The request body is the empty object both
// sides carry for an endpoint that takes no body: it is identical, so the
// matcher gets past the request comparison and scores the response, which is
// where the contract violation lives.
func objectRootDoc(title, path string, props map[string]string) *models.OpenAPI {
	responseProps := make(map[string]map[string]interface{}, len(props))
	for name, typ := range props {
		responseProps[name] = map[string]interface{}{"type": typ}
	}
	op := &models.Operation{
		RequestBody: &models.RequestBody{
			Content: map[string]models.MediaType{
				"application/json": {Schema: models.Schema{Type: "object"}},
			},
		},
		Responses: map[string]models.ResponseItem{
			"200": {Content: map[string]models.MediaType{
				"application/json": {Schema: models.Schema{Type: "object", Properties: responseProps}},
			}},
		},
	}
	return &models.OpenAPI{
		OpenAPI: "3.0.0",
		Info:    models.Info{Title: title, Version: "1.0.0"},
		Paths:   map[string]models.PathItem{path: {Get: op}},
	}
}

// TestScoresForMocks_ZeroOverlapIsRecordedAsCandidate covers the score the
// 0.0 seed swallowed.
//
// schemaSimilarity returns the fraction of the mock's response fields that the
// test provides, so a test that reaches the same endpoint with the same request
// but shares no response field with the mock scores exactly 0. The seed was
// also 0.0 and the gate is `pass && candidateScore > current`, so 0 > 0 was
// never recorded: the mock kept an empty test set and an empty test name, and
// ValidateMockAgainstTests printed it as MISSED - "no ideal test case found" -
// which is the opposite of what a disjoint response means. The candidate has to
// be recorded with its test, so the failure can be explained.
func TestScoresForMocks_ZeroOverlapIsRecordedAsCandidate(t *testing.T) {
	s := &consumer{logger: zap.NewNop(), config: &config.Config{}}

	mock := objectRootDoc("mock-users", "/users", map[string]string{"id": "integer"})
	test := objectRootDoc("test-users", "/users", map[string]string{"name": "string"})

	mockSet := map[string]models.SchemaInfo{}
	s.scoresForMocks([]*models.OpenAPI{mock}, mockSet,
		map[string]map[string]*models.OpenAPI{"test-set-0": {"test-users": test}}, "mock-set-0")

	got, ok := mockSet["mock-users"]
	if !ok {
		t.Fatal("mock is missing from the score set entirely")
	}
	if got.Score != 0.0 {
		t.Errorf("score = %v, want 0 (the mock's only response field is absent from the test)", got.Score)
	}
	if got.Name != "test-users" || got.TestSetID != "test-set-0" {
		t.Errorf("candidate not recorded: name = %q, test set = %q; want %q in %q",
			got.Name, got.TestSetID, "test-users", "test-set-0")
	}
}

// TestScoresForMocks_TypeMismatchIsRecordedAsCandidate is the same contract
// violation in its most common form - the field is there, its type is not - and
// it must reach the score set too. Both sides have "id", so a field-presence
// check would call this a match; schemaSimilarity compares the type and scores
// it 0.
func TestScoresForMocks_TypeMismatchIsRecordedAsCandidate(t *testing.T) {
	s := &consumer{logger: zap.NewNop(), config: &config.Config{}}

	mock := objectRootDoc("mock-users", "/users", map[string]string{"id": "integer"})
	test := objectRootDoc("test-users", "/users", map[string]string{"id": "string"})

	mockSet := map[string]models.SchemaInfo{}
	s.scoresForMocks([]*models.OpenAPI{mock}, mockSet,
		map[string]map[string]*models.OpenAPI{"test-set-0": {"test-users": test}}, "mock-set-0")

	got := mockSet["mock-users"]
	if got.Score != 0.0 {
		t.Errorf("score = %v, want 0 (id is an integer in the mock and a string in the test)", got.Score)
	}
	if got.Name != "test-users" || got.TestSetID != "test-set-0" {
		t.Errorf("candidate not recorded: name = %q, test set = %q; want %q in %q",
			got.Name, got.TestSetID, "test-users", "test-set-0")
	}
}

// TestValidateMockAgainstTests_ZeroOverlapFailsRatherThanMisses is the report
// the user actually reads. A recorded test that contradicts the mock is a
// failed contract check, counted under Failed and listed by name; only a mock no
// test reached at all is missed.
func TestValidateMockAgainstTests_ZeroOverlapFailsRatherThanMisses(t *testing.T) {
	s := &consumer{logger: zap.NewNop(), config: &config.Config{}}
	s.config.Contract.Mappings.Self = "my-service"

	mock := objectRootDoc("mock-users", "/users", map[string]string{"id": "integer"})
	test := objectRootDoc("test-users", "/users", map[string]string{"id": "string"})
	tests := map[string]map[string]*models.OpenAPI{"test-set-0": {"test-users": test}}

	scores, err := s.getMockScores(tests, []models.MockMapping{
		{Service: "provider-svc", TestSetID: "mock-set-0", Mocks: []*models.OpenAPI{mock}},
	})
	if err != nil {
		t.Fatalf("getMockScores returned an error: %v", err)
	}

	summary, err := s.ValidateMockAgainstTests(scores, tests)
	if err != nil {
		t.Fatalf("ValidateMockAgainstTests returned an error: %v", err)
	}
	if len(summary.ServicesSummary) != 1 {
		t.Fatalf("services summarized = %d, want 1", len(summary.ServicesSummary))
	}

	got := summary.ServicesSummary[0]
	if got.FailedCount != 1 || got.MissedCount != 0 {
		t.Errorf("counts: failed = %d, missed = %d; want failed = 1, missed = 0", got.FailedCount, got.MissedCount)
	}
	if status := got.TestSets["mock-set-0"]; len(status.Failed) != 1 || len(status.Missed) != 0 {
		t.Errorf("status = %+v; want the mock listed as failed and not as missed", status)
	}
}

// TestValidateMockAgainstTests_ResponseShapeMismatchFails pins the case #4466
// left as MISSED: an object response on both sides of the same path where the
// shapes disagree. The test reached the mock's endpoint, so the mock is not
// "uncovered" - the contract check failed, and the failure is the type diff this
// branch renders.
func TestValidateMockAgainstTests_ResponseShapeMismatchFails(t *testing.T) {
	s := &consumer{logger: zap.NewNop(), config: &config.Config{}}
	s.config.Contract.Mappings.Self = "my-service"

	mock := objectRootDoc("mock-users", "/users", map[string]string{"id": "integer"})
	test := arrayRootDoc("test-users", map[string]string{"id": "integer"})
	tests := map[string]map[string]*models.OpenAPI{"test-set-0": {"test-users": test}}

	scores, err := s.getMockScores(tests, []models.MockMapping{
		{Service: "provider-svc", TestSetID: "mock-set-0", Mocks: []*models.OpenAPI{mock}},
	})
	if err != nil {
		t.Fatalf("getMockScores returned an error: %v", err)
	}

	summary, err := s.ValidateMockAgainstTests(scores, tests)
	if err != nil {
		t.Fatalf("ValidateMockAgainstTests returned an error: %v", err)
	}

	got := summary.ServicesSummary[0]
	if got.FailedCount != 1 || got.MissedCount != 0 {
		t.Errorf("counts: failed = %d, missed = %d; want failed = 1, missed = 0", got.FailedCount, got.MissedCount)
	}
}

// TestValidateMockAgainstTests_NoCandidateIsMissed keeps the other half honest:
// a mock whose endpoint no recorded test reached is still missed, so recording
// a zero-score candidate does not turn every unmatched mock into a failure.
func TestValidateMockAgainstTests_NoCandidateIsMissed(t *testing.T) {
	s := &consumer{logger: zap.NewNop(), config: &config.Config{}}

	mock := objectRootDoc("mock-orders", "/orders", map[string]string{"id": "integer"})
	test := objectRootDoc("test-users", "/users", map[string]string{"id": "integer"})
	tests := map[string]map[string]*models.OpenAPI{"test-set-0": {"test-users": test}}

	scores, err := s.getMockScores(tests, []models.MockMapping{
		{Service: "provider-svc", TestSetID: "mock-set-0", Mocks: []*models.OpenAPI{mock}},
	})
	if err != nil {
		t.Fatalf("getMockScores returned an error: %v", err)
	}

	summary, err := s.ValidateMockAgainstTests(scores, tests)
	if err != nil {
		t.Fatalf("ValidateMockAgainstTests returned an error: %v", err)
	}

	got := summary.ServicesSummary[0]
	if got.MissedCount != 1 || got.FailedCount != 0 {
		t.Errorf("counts: missed = %d, failed = %d; want missed = 1, failed = 0", got.MissedCount, got.FailedCount)
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

package grpc

import (
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

func TestMatch_JSONComparison(t *testing.T) {
	t.Setenv(provenswitch.Env, "0") // this tests the paths with the proven switch off
	logger := zaptest.NewLogger(t)

	tests := []struct {
		name           string
		expectedData   string
		actualData     string
		noiseConfig    map[string]map[string][]string
		ignoreOrdering bool
		expectedMatch  bool
		description    string
	}{
		{
			name:           "Identical JSON responses",
			expectedData:   `{"name": "test", "value": 123}`,
			actualData:     `{"name": "test", "value": 123}`,
			noiseConfig:    map[string]map[string][]string{},
			ignoreOrdering: false,
			expectedMatch:  true,
			description:    "Should match when JSON data is identical",
		},
		{
			name:           "Different JSON responses",
			expectedData:   `{"name": "test", "value": 123}`,
			actualData:     `{"name": "test", "value": 456}`,
			noiseConfig:    map[string]map[string][]string{},
			ignoreOrdering: false,
			expectedMatch:  false,
			description:    "Should not match when JSON data differs",
		},
		{
			name:           "JSON with noise ignored",
			expectedData:   `{"name": "test", "value": 123, "timestamp": "2023-01-01"}`,
			actualData:     `{"name": "test", "value": 123, "timestamp": "2023-01-02"}`,
			noiseConfig:    map[string]map[string][]string{"body": {"timestamp": {".*"}}},
			ignoreOrdering: false,
			expectedMatch:  true,
			description:    "Should match when noisy fields are ignored",
		},
		{
			name:           "Non-JSON protoscope data",
			expectedData:   `1: {2: 3 4: "test"}`,
			actualData:     `1: {2: 3 4: "test"}`,
			noiseConfig:    map[string]map[string][]string{},
			ignoreOrdering: false,
			expectedMatch:  true,
			description:    "Should match non-JSON data using canonicalization",
		},
		{
			name:           "Mixed JSON and non-JSON",
			expectedData:   `{"name": "test"}`,
			actualData:     `1: {2: 3}`,
			noiseConfig:    map[string]map[string][]string{},
			ignoreOrdering: false,
			expectedMatch:  false,
			description:    "Should handle mixed JSON and non-JSON gracefully",
		},
		{
			name: "Protoscope field reorder with different indentation",
			expectedData: `9: {  3: {    1: {      2: {2: 0.0}   # 0x0i64
  1: {"candidateCnt"}}
  1: {      2: {3: {"OVS"}}
  1: {"type"}}}}`,
			actualData: `9: {  3: {    1: {      2: {3: {"OVS"}}
  1: {"type"}}
  1: {      2: {2: 0.0}   # 0x0i64
  1: {"candidateCnt"}}}}`,
			noiseConfig:    map[string]map[string][]string{},
			ignoreOrdering: false,
			expectedMatch:  true,
			description:    "Should match protoscope text where field order (and thus indentation) differs but content is identical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test case
			tc := &models.TestCase{
				Name: "test-case",
				GrpcResp: models.GrpcResp{
					Body: models.GrpcLengthPrefixedMessage{
						DecodedData:     tt.expectedData,
						CompressionFlag: 0,
						MessageLength:   uint32(len(tt.expectedData)),
					},
					Headers: models.GrpcHeaders{
						PseudoHeaders:   map[string]string{":status": "200"},
						OrdinaryHeaders: map[string]string{},
					},
					Trailers: models.GrpcHeaders{
						PseudoHeaders:   map[string]string{},
						OrdinaryHeaders: map[string]string{},
					},
				},
			}

			// Create actual response
			actualResp := &models.GrpcResp{
				Body: models.GrpcLengthPrefixedMessage{
					DecodedData:     tt.actualData,
					CompressionFlag: 0,
					MessageLength:   uint32(len(tt.actualData)),
				},
				Headers: models.GrpcHeaders{
					PseudoHeaders:   map[string]string{":status": "200"},
					OrdinaryHeaders: map[string]string{},
				},
				Trailers: models.GrpcHeaders{
					PseudoHeaders:   map[string]string{},
					OrdinaryHeaders: map[string]string{},
				},
			}

			// Run the match
			matched, result := Match(tc, actualResp, tt.noiseConfig, tt.ignoreOrdering, logger, true)

			// Check the result
			if matched != tt.expectedMatch {
				t.Errorf("Test %q failed: expected match=%v, got match=%v", tt.name, tt.expectedMatch, matched)
				t.Errorf("Description: %s", tt.description)
			}

			// Ensure result is not nil
			if result == nil {
				t.Errorf("Test %q failed: result should not be nil", tt.name)
			}

			// Check that body result has the correct data
			if len(result.BodyResult) == 0 {
				t.Errorf("Test %q failed: expected body result to be present", tt.name)
			} else {
				// Find the decoded data result
				var decodedDataResult *models.BodyResult
				for i := range result.BodyResult {
					if result.BodyResult[i].Type == models.GrpcData {
						decodedDataResult = &result.BodyResult[i]
						break
					}
				}

				if decodedDataResult == nil {
					t.Errorf("Test %q failed: expected GrpcData body result to be present", tt.name)
				} else if decodedDataResult.Normal != tt.expectedMatch {
					t.Errorf("Test %q failed: expected body result normal=%v, got normal=%v", tt.name, tt.expectedMatch, decodedDataResult.Normal)
				}
			}
		})
	}
}

func TestMatch_IgnoreOrdering(t *testing.T) {
	t.Setenv(provenswitch.Env, "0") // this tests the paths with the proven switch off
	logger := zaptest.NewLogger(t)

	// Test JSON array ordering differences
	tc := &models.TestCase{
		Name: "array-ordering-test",
		GrpcResp: models.GrpcResp{
			Body: models.GrpcLengthPrefixedMessage{
				DecodedData:     `{"items": [{"id": 1}, {"id": 2}]}`,
				CompressionFlag: 0,
				MessageLength:   30,
			},
			Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":status": "200"},
				OrdinaryHeaders: map[string]string{},
			},
			Trailers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{},
				OrdinaryHeaders: map[string]string{},
			},
		},
	}

	actualResp := &models.GrpcResp{
		Body: models.GrpcLengthPrefixedMessage{
			DecodedData:     `{"items": [{"id": 2}, {"id": 1}]}`,
			CompressionFlag: 0,
			MessageLength:   30,
		},
		Headers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{":status": "200"},
			OrdinaryHeaders: map[string]string{},
		},
		Trailers: models.GrpcHeaders{
			PseudoHeaders:   map[string]string{},
			OrdinaryHeaders: map[string]string{},
		},
	}

	noiseConfig := map[string]map[string][]string{}

	// Test with ignoreOrdering = false (should not match)
	matchedStrict, _ := Match(tc, actualResp, noiseConfig, false, logger, true)
	if matchedStrict {
		t.Error("Expected arrays with different ordering to not match when ignoreOrdering=false")
	}

	// Test with ignoreOrdering = true (should match)
	matchedIgnoreOrder, _ := Match(tc, actualResp, noiseConfig, true, logger, true)
	if !matchedIgnoreOrder {
		t.Error("Expected arrays with different ordering to match when ignoreOrdering=true")
	}

	// The failure's grade reads the body with the ordering the comparison
	// applied: reordered with a field added to each element is only new
	// fields (LOW) under ignoreOrdering, and an array that no longer holds
	// what it held, element for element, without it (MEDIUM).
	added := *actualResp
	added.Body.DecodedData = `{"items": [{"id": 2, "x": 1}, {"id": 1, "x": 1}]}`
	for _, c := range []struct {
		ignoreOrdering bool
		want           models.RiskLevel
	}{{true, models.Low}, {false, models.Medium}} {
		matched, result := Match(tc, &added, noiseConfig, c.ignoreOrdering, logger, false)
		if matched || result == nil || result.FailureInfo.Assessment == nil {
			t.Fatalf("ignoreOrdering=%v: an added field must fail with a body assessment: %v %+v", c.ignoreOrdering, matched, result)
		}
		if got := result.FailureInfo.Assessment.Risk; got != c.want {
			t.Errorf("ignoreOrdering=%v: graded %s, want %s (%q)", c.ignoreOrdering, got, c.want, result.FailureInfo.Assessment.Reasons)
		}
	}
}

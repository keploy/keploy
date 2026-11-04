package grpc

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// TestMatch_Assertions drives Match (not AssertionMatch directly) so it proves
// the wiring — the actual defect in #4609 was that Match never consulted
// tc.Assertions at all, so a response that matched the recording passed even
// when the assertions did not.
func TestMatch_Assertions(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// newResp builds a gRPC response whose body and grpc-status trailer are
	// identical on both sides, so the ordinary comparison always passes and any
	// failure must come from the assertions.
	newResp := func(body, grpcStatus string) *models.GrpcResp {
		return &models.GrpcResp{
			Body: models.GrpcLengthPrefixedMessage{
				DecodedData:   body,
				MessageLength: uint32(len(body)),
			},
			Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":status": "200"},
				OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
			},
			Trailers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{},
				OrdinaryHeaders: map[string]string{"grpc-status": grpcStatus},
			},
		}
	}

	tests := []struct {
		name          string
		body          string
		actualBody    string // defaults to body when empty
		grpcStatus    string
		assertions    map[models.AssertionType]interface{}
		expectedMatch bool
		description   string
	}{
		{
			name:       "issue repro: status_code and json_contains both fail",
			body:       `{"name": "test", "value": 123}`,
			grpcStatus: "0",
			assertions: map[models.AssertionType]interface{}{
				models.StatusCode:   500,
				models.JsonContains: map[string]interface{}{"missing_field": "x"},
			},
			expectedMatch: false,
			description:   "response matches the recording but the assertions do not; must fail",
		},
		{
			name:       "status_code matches grpc-status trailer",
			body:       `{"ok": true}`,
			grpcStatus: "5",
			assertions: map[models.AssertionType]interface{}{
				models.StatusCode: 5,
			},
			expectedMatch: true,
			description:   "status_code targets the grpc-status trailer, not the :status pseudo-header",
		},
		{
			name:       "status_code mismatch fails",
			body:       `{"ok": true}`,
			grpcStatus: "0",
			assertions: map[models.AssertionType]interface{}{
				models.StatusCode: 5,
			},
			expectedMatch: false,
			description:   "grpc-status 0 does not satisfy status_code: 5",
		},
		{
			name:       "status_code_in matches",
			body:       `{}`,
			grpcStatus: "7",
			assertions: map[models.AssertionType]interface{}{
				models.StatusCodeIn: []interface{}{"3", "7", "13"},
			},
			expectedMatch: true,
			description:   "grpc-status 7 is in the allowed set",
		},
		{
			name:       "json_contains present passes",
			body:       `{"name": "test", "value": 123}`,
			grpcStatus: "0",
			assertions: map[models.AssertionType]interface{}{
				models.JsonContains: map[string]interface{}{"name": "test"},
			},
			expectedMatch: true,
			description:   "the decoded body contains the asserted field",
		},
		{
			name:       "json_equal mismatch fails",
			body:       `{"name": "test"}`,
			actualBody: `{"name": "other"}`,
			grpcStatus: "0",
			assertions: map[models.AssertionType]interface{}{
				// HTTP parity: json_equal compares the actual decoded body against
				// the RECORDED one and ignores this value, so any realistic value
				// works — the case fails because actual != recorded.
				models.JsonEqual: map[string]interface{}{"name": "other"},
			},
			expectedMatch: false,
			description:   "json_equal asserts the actual decoded body equals the recorded one (HTTP parity)",
		},
		{
			name:       "header_equal on a trailer passes",
			body:       `{}`,
			grpcStatus: "0",
			assertions: map[models.AssertionType]interface{}{
				models.HeaderEqual: map[string]interface{}{"grpc-status": "0"},
			},
			expectedMatch: true,
			description:   "header assertions look in both headers and trailers",
		},
		{
			name:       "noise-only assertions do not trigger assertion matching",
			body:       `{"name": "test"}`,
			grpcStatus: "0",
			assertions: map[models.AssertionType]interface{}{
				models.NoiseAssertion: map[string][]string{},
			},
			expectedMatch: true,
			description:   "a noise-only assertions map must not divert to AssertionMatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actualBody := tt.actualBody
			if actualBody == "" {
				actualBody = tt.body
			}
			tc := &models.TestCase{
				Name:       "grpc-assertion-test",
				GrpcResp:   *newResp(tt.body, tt.grpcStatus),
				Assertions: tt.assertions,
			}
			actualResp := newResp(actualBody, tt.grpcStatus)

			matched, result := Match(tc, actualResp, map[string]map[string][]string{}, false, logger, false)
			if matched != tt.expectedMatch {
				t.Errorf("expected match=%v, got %v (%s)", tt.expectedMatch, matched, tt.description)
			}
			if result == nil {
				t.Error("result should not be nil")
			}
		})
	}
}

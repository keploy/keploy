package replay

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// nonStreamAnswerHooks answers the named tests with the given streaming
// response and every other test as prHooks does.
type nonStreamAnswerHooks struct {
	prHooks
	answers map[string]func(tc *models.TestCase) *pkg.StreamingHTTPResponse
}

func (h nonStreamAnswerHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	if answer := h.answers[tc.Name]; answer != nil {
		return answer(tc), nil
	}
	return h.prHooks.SimulateRequest(ctx, tc, testSetID)
}

// A test case recorded as a stream whose answer is not one (stream mode none:
// a JSON error to a recorded NDJSON stream, say) is compared as a normal
// response is. It used to fail with "unsupported HTTP stream mode: none" and
// status 0, which hid what the app answered.
func TestRunTestSetComparesANonStreamAnswerToAStreamTestAsANormalResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answer   func(tc *models.TestCase) *pkg.StreamingHTTPResponse
		wantPass bool
		wantCode int
		wantBody string
	}{
		{
			name: "a JSON error is reported as the app answered it",
			answer: func(*models.TestCase) *pkg.StreamingHTTPResponse {
				header := map[string]string{"Content-Type": "application/json"}
				return &pkg.StreamingHTTPResponse{StatusCode: 404, StatusMessage: http.StatusText(404), Header: header,
					Reader:       io.NopCloser(strings.NewReader(`{"error":"no running recording"}`)),
					StreamConfig: pkg.DetectHTTPStreamConfig(nil, &http.Response{Header: http.Header{"Content-Type": {header["Content-Type"]}}})}
			},
			wantCode: 404,
			wantBody: `{"error":"no running recording"}`,
		},
		{
			name: "the recorded answer passes",
			answer: func(tc *models.TestCase) *pkg.StreamingHTTPResponse {
				return &pkg.StreamingHTTPResponse{StatusCode: tc.HTTPResp.StatusCode, StatusMessage: http.StatusText(tc.HTTPResp.StatusCode),
					Header: tc.HTTPResp.Header, Reader: io.NopCloser(strings.NewReader(tc.HTTPResp.Body)),
					StreamConfig: pkg.HTTPStreamConfig{Mode: pkg.HTTPStreamModeNone}}
			},
			wantPass: true,
			wantCode: 200,
			wantBody: `{"ok":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.streaming("test-2")
			h.replayer.hookImpl = nonStreamAnswerHooks{answers: map[string]func(*models.TestCase) *pkg.StreamingHTTPResponse{"test-2": tc.answer}}
			_ = h.run(t)

			var got *models.TestResult
			for i := range h.report.results {
				if h.report.results[i].TestCaseID == "test-2" {
					got = &h.report.results[i]
				}
			}
			if got == nil {
				t.Fatalf("no result for the streaming test; results: %+v", h.report.results)
			}
			if got.Res.StatusCode != tc.wantCode || got.Res.Body != tc.wantBody {
				t.Errorf("result carries %d %q; want the app's answer %d %q", got.Res.StatusCode, got.Res.Body, tc.wantCode, tc.wantBody)
			}
			if pass := got.Status == models.TestStatusPassed; pass != tc.wantPass {
				t.Errorf("status %s; want passed=%v", got.Status, tc.wantPass)
			}
		})
	}
}

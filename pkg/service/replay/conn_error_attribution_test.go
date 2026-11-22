package replay

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// failedWith builds the result of tc failing with err, as RunTestSet does.
func failedWith(tc *models.TestCase, err error) *models.TestResult {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	return r.CreateFailedTestResult(tc, "test-set-0", time.Now(), err)
}

// A test that failed because the app could not be connected to is labelled
// APP_CONNECTION_ERROR, so its synthetic status_code=0 does not masquerade as a
// STATUS_CODE_CHANGED content regression. The label comes from the error, by
// pkg.IsAppConnectionError, the rule the reset re-send and the unreachable-port
// check read the same error by: the docker-proxy drop at a port no app listens
// behind is labelled in both shapes net/http reports it in, where the old match
// on the error's text labelled only one. pkg's
// TestAFailedTestRequestIsClassifiedByTheErrorNotItsText provokes each real
// error.
func TestCreateFailedTestResultLabelsAnAppConnectionErrorByTheError(t *testing.T) {
	httpTC := &models.TestCase{Name: "post-echo-1", Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Method: "POST", URL: "http://localhost:8097/echo"}}
	grpcTC := &models.TestCase{Name: "grpc-echo-1", Kind: models.GRPC_EXPORT}
	atUnreachablePort := func(cause error) error {
		return &pkg.UnreachableAppPortError{Port: 8097, Reason: "the app listens on port 8097 only on 127.0.0.1 inside the container",
			Err: &url.Error{Op: "Post", URL: "http://localhost:8097/echo", Err: cause}}
	}

	for _, tt := range []struct {
		name string
		tc   *models.TestCase
		err  error
		want bool
	}{
		{"a drop net/http reports as io.EOF", httpTC, atUnreachablePort(io.EOF), true},
		{"the same drop reported as errServerClosedIdle", httpTC, atUnreachablePort(errors.New("http: server closed idle connection")), true},
		{"a gRPC call that lost its connection", grpcTC,
			fmt.Errorf("failed to create stream: %w", status.Error(codes.Unavailable, `connection error: desc = "error reading server preface: EOF"`)), true},
		// No-answer failures stay out of APP_CONNECTION_ERROR: the prune gate
		// reads them through pkg.IsAppNoAnswer, and the report's category (which
		// k8s-proxy triages on) must not change. A stop's cancel is not the app
		// being unreachable.
		{"no answer in time", httpTC, prClientTimeoutErr(t), false},
		{"cancelled", httpTC, prCanceledErr(t), false},
		{"an internal error", httpTC, errors.New("invalid response type for HTTP test case"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := failedWith(tt.tc, tt.err)
			if tt.want {
				assert.Contains(t, res.FailureInfo.Category, models.AppConnectionError, "a test that failed with %v", tt.err)
			} else {
				assert.NotContains(t, res.FailureInfo.Category, models.AppConnectionError, "a test that failed with %v", tt.err)
			}
			if tt.tc.Kind == models.HTTP {
				assert.Equal(t, 0, res.Res.StatusCode)
				assert.Equal(t, tt.err.Error(), res.Res.Body, "the error's text stands in for the response")
			}
		})
	}
}

// A gRPC test with no answer is reported as an answer with no status: under
// the proven switch, a recording that has its grpc-status differs from the
// stand-in's none (-1, a StatusCodeChanged), as an HTTP test with no answer
// reports status 0. With the switch off the stand-in's absent status is read
// as 0, as it was: the same as the recording's OK.
func TestCreateFailedTestResultReportsAGRPCTestWithNoAnswerAsNoStatus(t *testing.T) {
	tc := &models.TestCase{Name: "grpc-echo-1", Kind: models.GRPC_EXPORT}
	tc.GrpcResp.Trailers = models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}}
	noAnswer := errors.New("the app sent no answer")

	t.Setenv(provenswitch.Env, "")
	off := failedWith(tc, noAnswer)
	assert.Equal(t, models.IntResult{Normal: true, Expected: 0, Actual: 0}, off.Result.StatusCode, "switch off: unchanged")
	assert.NotContains(t, off.FailureInfo.Category, models.StatusCodeChanged, "switch off: unchanged")

	t.Setenv(provenswitch.Env, "1")
	on := failedWith(tc, noAnswer)
	assert.Equal(t, models.IntResult{Normal: false, Expected: 0, Actual: -1}, on.Result.StatusCode, "KEPLOY_PROVEN=1: no answer is no status")
	assert.Contains(t, on.FailureInfo.Category, models.StatusCodeChanged, "KEPLOY_PROVEN=1")
	assert.Empty(t, on.GrpcRes.Trailers.OrdinaryHeaders, "KEPLOY_PROVEN=1: the stand-in carries no status")
}

func TestAppendCategoryUnique(t *testing.T) {
	cats := []models.FailureCategory{models.StatusCodeChanged}
	cats = appendCategoryUnique(cats, models.AppConnectionError)
	cats = appendCategoryUnique(cats, models.AppConnectionError) // dup must be a no-op
	if len(cats) != 2 {
		t.Fatalf("expected 2 unique categories, got %d: %v", len(cats), cats)
	}
	found := false
	for _, c := range cats {
		if c == models.AppConnectionError {
			found = true
		}
	}
	if !found {
		t.Fatal("AppConnectionError category not appended")
	}
}

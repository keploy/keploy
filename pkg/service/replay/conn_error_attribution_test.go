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

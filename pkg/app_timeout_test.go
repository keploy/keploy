package pkg

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAppTimeoutClassificationOnlyFiresForConfiguredAPITimeout verifies that
// IsAppTimeout and isClientAPITimeout classify ONLY keploy's configured
// test.apiTimeout as APP_TIMEOUT, and NEVER misclassify DNS timeouts, TCP dial
// ETIMEDOUT, unbounded mode, parent context deadlines, or gRPC's default
// fallback timeout when apiTimeout == 0.
func TestAppTimeoutClassificationOnlyFiresForConfiguredAPITimeout(t *testing.T) {
	dnsTimeout := &net.DNSError{Err: "i/o timeout", Name: "app.internal", IsTimeout: true}
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}
	dialDeadline := &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	grpcDeadline := status.Error(codes.DeadlineExceeded, "context deadline exceeded")
	realAppTimeout := &AppTimeoutError{TestCase: "tc-1", Duration: 5 * time.Second, Err: context.DeadlineExceeded}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"DNS resolver timeout", dnsTimeout, false},
		{"TCP dial ETIMEDOUT", dialTimeout, false},
		{"TCP dial deadline exceeded", dialDeadline, false},
		{"raw context.DeadlineExceeded without AppTimeoutError", context.DeadlineExceeded, false},
		{"raw gRPC DeadlineExceeded (e.g. defaultGrpcCallTimeout)", grpcDeadline, false},
		{"ErrAnswerStalled", ErrAnswerStalled, false},
		{"configured AppTimeoutError", realAppTimeout, true},
	} {
		t.Run("IsAppTimeout/"+tc.name, func(t *testing.T) {
			if got := IsAppTimeout(tc.err); got != tc.want {
				t.Errorf("IsAppTimeout(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	parentExpired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	for _, tc := range []struct {
		name      string
		parentCtx context.Context
		err       error
		timeout   time.Duration
		want      bool
	}{
		{"real client apiTimeout", context.Background(), context.DeadlineExceeded, 5 * time.Second, true},
		{"DNS timeout with apiTimeout set", context.Background(), dnsTimeout, 5 * time.Second, false},
		{"TCP dial ETIMEDOUT with apiTimeout set", context.Background(), dialTimeout, 5 * time.Second, false},
		{"TCP dial deadline with apiTimeout set", context.Background(), dialDeadline, 5 * time.Second, false},
		{"unbounded mode (timeout=0)", context.Background(), context.DeadlineExceeded, 0, false},
		{"parent context deadline exceeded", parentExpired, context.DeadlineExceeded, 5 * time.Second, false},
		{"ErrAnswerStalled", context.Background(), ErrAnswerStalled, 5 * time.Second, false},
	} {
		t.Run("isClientAPITimeout/"+tc.name, func(t *testing.T) {
			if got := isClientAPITimeout(tc.parentCtx, tc.err, tc.timeout); got != tc.want {
				t.Errorf("isClientAPITimeout(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestSimulateHTTP_RealAPITimeoutWrapsOnceWithoutDoubleErrorLog drives a real
// slow HTTP server against SimulateHTTP with APITimeout=1s and verifies it
// wraps the error in AppTimeoutError without logging a duplicate ERROR inside
// SimulateHTTP.
func TestSimulateHTTP_RealAPITimeoutWrapsOnceWithoutDoubleErrorLog(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	core, logs := observer.New(zapcore.ErrorLevel)
	tc := &models.TestCase{
		Name: "tc-slow",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    srv.URL + "/slow",
			Header: map[string]string{},
		},
	}
	_, err := SimulateHTTP(context.Background(), tc, "test-set-0", zap.New(core), SimulationConfig{
		APITimeout:    1,
		TargetFromURL: true,
	})
	if !IsAppTimeout(err) {
		t.Fatalf("SimulateHTTP err = %v (%T), want AppTimeoutError", err, err)
	}
	var te *AppTimeoutError
	if !errors.As(err, &te) || te.Duration != time.Second || te.TestCase != "tc-slow" {
		t.Fatalf("AppTimeoutError = %+v, want TestCase=tc-slow Duration=1s", te)
	}
	if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
		t.Errorf("SimulateHTTP must not emit its own ERROR log on apiTimeout (caller logs once), got %d: %v", len(errs), errs)
	}
}

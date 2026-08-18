package pkg

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// startResetOnAccept accepts one connection and immediately RSTs it (linger 0),
// which is what a docker-proxy / not-yet-ready app does to a replay request.
// net/http surfaces it as ECONNRESET or a bare EOF depending on timing — both
// are the recoverable class.
func startResetOnAccept(t *testing.T) (baseURL string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // close() now sends RST, not FIN
			}
			_ = c.Close()
		}
	}()
	return "http://" + ln.Addr().String(), func() { _ = ln.Close(); <-done }
}

// TestSimulateHTTP_TransportResetIsNotLoggedAsError drives the REAL send path,
// not the logging helper, because the call site is the thing that regressed.
//
// A transport-level reset is recoverable: the replay orchestration re-sends it
// once it can prove the failed attempt consumed no mock, and it usually recovers
// on the first re-send. Reporting it at ERROR is not cosmetic — keploy's own e2e
// scripts fail a run on any ERROR line in the replay log (see
// .github/workflows/test_workflow_scripts/golang/http_pokeapi/golang-linux.sh
// and its siblings), so one recovered transient turned a suite whose every test
// passed into a red build.
//
// The definitive ERROR is still emitted by the caller once the re-send is
// refused or exhausted ("failed to simulate request" in RunTestSet), so nothing
// is lost here.
func TestSimulateHTTP_TransportResetIsNotLoggedAsError(t *testing.T) {
	baseURL, stop := startResetOnAccept(t)
	defer stop()

	for _, tc := range []struct {
		name string
		call func(*zap.Logger) error
	}{
		{"SimulateHTTP", func(l *zap.Logger) error {
			_, err := SimulateHTTP(context.Background(), pingTestCase(baseURL), "test-set", l, SimulationConfig{APITimeout: 5})
			return err
		}},
		{"SimulateHTTPStreaming", func(l *zap.Logger) error {
			_, err := SimulateHTTPStreaming(context.Background(), pingTestCase(baseURL), "test-set", l, SimulationConfig{APITimeout: 5})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)

			err := tc.call(zap.New(core))
			if err == nil {
				t.Fatal("the request must still fail — this change is about the log level, not the outcome")
			}
			if !IsTransportConnReset(err) {
				t.Fatalf("test setup broken: expected a transport reset, got %v", err)
			}
			if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
				t.Errorf("%s logged %d ERROR line(s) for a re-sendable transport reset; keploy's e2e "+
					"scripts treat any ERROR in the replay log as a failed run, so a transient the "+
					"caller recovers from must not emit one: %v", tc.name, n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
			}
			if logs.FilterLevelExact(zapcore.DebugLevel).Len() == 0 {
				t.Errorf("%s should still record the reset at Debug for diagnosis", tc.name)
			}
		})
	}
}

// TestSimulateHTTP_UnreachableAppStillLogsError keeps the downgrade narrow.
// Only the class the caller can re-send is quietened; an app that is simply not
// there is terminal on this path and must still be reported at ERROR.
func TestSimulateHTTP_UnreachableAppStillLogsError(t *testing.T) {
	// Reserve a port and release it, so connects are refused for good.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	core, logs := observer.New(zapcore.DebugLevel)
	tc := &models.TestCase{
		Name: "ping", Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Method: "GET", URL: "http://" + addr + "/ping", Header: map[string]string{}},
	}
	if _, err := SimulateHTTP(context.Background(), tc, "test-set", zap.New(core), SimulationConfig{APITimeout: 5}); err == nil {
		t.Fatal("a refused port must fail")
	}
	if logs.FilterLevelExact(zapcore.ErrorLevel).Len() == 0 {
		t.Errorf("a genuinely unreachable app is not re-sendable here and must still be an ERROR, got %v", logs.All())
	}
}

// TestLogSimulateSendFailure_ClassifiesByRecoverability covers the policy itself
// for error shapes that are awkward to produce over a real socket.
func TestLogSimulateSendFailure_ClassifiesByRecoverability(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantError bool
	}{
		{"EPIPE is recoverable", syscall.EPIPE, false},
		{"connection refused is terminal here", syscall.ECONNREFUSED, true},
		{"host unreachable is terminal", syscall.EHOSTUNREACH, true},
		{"opaque failure is terminal", errors.New("no such host"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			logSimulateSendFailure(zap.New(core), tc.err)
			gotError := logs.FilterLevelExact(zapcore.ErrorLevel).Len() > 0
			if gotError != tc.wantError {
				t.Errorf("ERROR logged = %v, want %v (logs: %v)", gotError, tc.wantError, logs.All())
			}
		})
	}
}

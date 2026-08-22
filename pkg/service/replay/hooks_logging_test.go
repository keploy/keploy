package replay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// failingHookInstrumentation fails every agent hook and satisfies the rest of
// Instrumentation with no-ops. Only the hook methods matter here.
type failingHookInstrumentation struct {
	Instrumentation // embedded: any method this test does not exercise panics loudly if reached
	err             error
}

func (f *failingHookInstrumentation) BeforeSimulate(context.Context, *time.Time, string, string) error {
	return f.err
}
func (f *failingHookInstrumentation) AfterSimulate(context.Context, string, string) error {
	return f.err
}
func (f *failingHookInstrumentation) BeforeTestRun(context.Context, string) error { return f.err }
func (f *failingHookInstrumentation) BeforeTestSetCompose(context.Context, string, string, bool) error {
	return f.err
}
func (f *failingHookInstrumentation) AfterTestRun(context.Context, string, []string, models.TestCoverage) error {
	return f.err
}
func (f *failingHookInstrumentation) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, f.err
}

// TestAgentHookFailuresAreNotErrors pins the contract that makes "an ERROR line
// means the run is broken" hold on the hook path.
//
// Before/AfterSimulate and the test-run hooks are best-effort by construction:
// the default OSS implementation is a no-op extension point, the agent client
// swallows a transport failure calling them, and every call site here ignores
// the returned error and carries on. A failure therefore does not invalidate the
// run — but reported at Error it did fail the run, because keploy's e2e scripts
// grade a run by grepping its log for "ERROR". One hook failure used to emit
// three Error lines (agent handler, agent client, and here).
//
// The failure must still be visible, so Warn is required, not silence.
func TestAgentHookFailuresAreNotErrors(t *testing.T) {
	hookErr := errors.New("agent hook failed (status 500, body: \"boom\")")

	// A listener that RSTs, so SimulateRequest's own send fails in the
	// recoverable class and cannot contribute an ERROR of its own.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			_ = c.Close()
		}
	}()

	tc := &models.TestCase{
		Name: "get-health", Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    "http://" + ln.Addr().String() + "/health",
			Header: map[string]string{},
		},
		HTTPResp: models.HTTPResp{StatusCode: http.StatusOK},
	}

	cases := []struct {
		name string
		run  func(h *Hooks) error
	}{
		{"SimulateRequest (Before+AfterSimulate)", func(h *Hooks) error {
			_, err := h.SimulateRequest(context.Background(), tc, "test-set-0")
			return err
		}},
		{"BeforeTestRun", func(h *Hooks) error {
			return h.BeforeTestRun(context.Background(), "test-run-0")
		}},
		{"BeforeTestSetCompose", func(h *Hooks) error {
			return h.BeforeTestSetCompose(context.Background(), "test-run-0", "test-set-0", true)
		}},
		{"AfterTestRun", func(h *Hooks) error {
			return h.AfterTestRun(context.Background(), "test-run-0", []string{"test-set-0"}, models.TestCoverage{})
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := &config.Config{}
			cfg.Test.APITimeout = 5
			h := &Hooks{
				cfg:             cfg,
				logger:          zap.New(core),
				instrumentation: &failingHookInstrumentation{err: hookErr},
			}

			_ = c.run(h)

			if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
				t.Errorf("a failed best-effort agent hook produced %d ERROR line(s); the run carries on, "+
					"and keploy's e2e scripts fail a run on any ERROR line: %v",
					n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
			}
			if logs.FilterLevelExact(zapcore.WarnLevel).Len() == 0 {
				t.Errorf("the hook failure must still be visible at Warn, got %v", logs.All())
			}
		})
	}
}

// TestGetConsumedMocksDoesNotDoubleReport pins that the pass-through no longer
// logs an error it also returns: callers already report it (some at Error, some
// deliberately at Debug), so logging here produced two entries for one failure.
func TestGetConsumedMocksDoesNotDoubleReport(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	h := &Hooks{
		cfg:             &config.Config{},
		logger:          zap.New(core),
		instrumentation: &failingHookInstrumentation{err: errors.New("agent unreachable")},
	}

	if _, err := h.GetConsumedMocks(context.Background()); err == nil {
		t.Fatal("the error must still be returned so the caller can decide")
	}
	if n := logs.Len(); n != 0 {
		t.Errorf("GetConsumedMocks must not log an error it returns (the caller does), got %v", logs.All())
	}
}

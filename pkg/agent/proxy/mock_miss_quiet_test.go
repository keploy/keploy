package proxy

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// mockModeFor sets the agent-wide `keploy mock` switch for one test and puts
// it back afterwards. It is one switch for the process, so these tests do not
// run in parallel.
func mockModeFor(t *testing.T, on bool) {
	t.Helper()
	was := mismatch.MockMode()
	mismatch.SetMockMode(on)
	t.Cleanup(func() { mismatch.SetMockMode(was) })
}

func missErr() error {
	return models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{
		Protocol: "HTTP", Destination: "127.0.0.1:9000", ActualSummary: "GET /price",
	})
}

// Outside `keploy mock` (keploy test, sandbox and k8s replays) a miss keeps
// the error log and the Warn mismatch line those modes always showed, and the
// connection handler gets the error itself.
func TestMockFailedKeepsTheLogsOutsideAMockRun(t *testing.T) {
	mockModeFor(t, false)
	core, logs := observer.New(zap.WarnLevel)
	p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 4)}

	err := p.mockFailed(p.logger, missErr())

	if errors.As(err, new(reportedMiss)) {
		t.Fatal("a miss outside a mock run must not be marked as reported by the CLI")
	}
	if n := logs.FilterMessage("mock mismatch: no matching mock for outgoing call").FilterLevelExact(zap.WarnLevel).Len(); n != 1 {
		t.Fatalf("want the Warn mismatch line once, got %d", n)
	}
	if n := logs.FilterMessage("failed to mock the outgoing message").Len(); n != 1 {
		t.Fatalf("want the error log once, got %d", n)
	}
}

// In a `keploy mock` run the CLI names each miss once at the end, so the
// agent logs the miss at Debug and marks it reported; a failure that is not a
// miss (a parser or I/O error) keeps its error log there too.
func TestMockFailedIsQuietOnlyForAMissInAMockRun(t *testing.T) {
	mockModeFor(t, true)
	core, logs := observer.New(zap.WarnLevel)
	p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 4)}

	if err := p.mockFailed(p.logger, missErr()); !errors.As(err, new(reportedMiss)) {
		t.Fatalf("a miss in a mock run is reported by the CLI: got %T", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a miss in a mock run logs nothing at Warn or above, got %d entries: %v", logs.Len(), logs.All())
	}

	parse := errors.New("malformed frame")
	if err := p.mockFailed(p.logger, parse); errors.As(err, new(reportedMiss)) {
		t.Fatal("a parser failure is not a miss and must not be hidden")
	}
	if n := logs.FilterMessage("failed to mock the outgoing message").Len(); n != 1 {
		t.Fatalf("a parser failure keeps its error log in a mock run, got %d", n)
	}
}

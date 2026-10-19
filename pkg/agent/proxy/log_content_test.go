package proxy

import (
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is a value of the application's, a literal in the statement it sent.
const canary = "kp-canary-proxy-miss-6c0d"

// mysqlMiss is a miss of the statement MySQL's replayer reports: its command
// and its text, literals included, as the report keeps it.
func mysqlMiss() *models.MockMismatchReport {
	return &models.MockMismatchReport{Protocol: "MySQL", Destination: "db:3306", ClosestMock: "mock-0",
		ActualSummary: "COM_QUERY SELECT body FROM notes WHERE tag = '" + canary + "'", MatchPhase: models.MatchPhaseExhausted}
}

// The one line every parser's miss is logged by names the call by its shape,
// not the values it sent, unless KEPLOY_LOG_CONTENT is set.
func TestSendMockNotFoundError_TheMissLineCarriesNoValueTheCallSent(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
		p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, mysqlMiss()))
		logtest.Check(t, logs, canary, on, "mock mismatch: no matching mock for outgoing call")
		e := logs.FilterMessage("mock mismatch: no matching mock for outgoing call").All()
		if got, want := e[0].ContextMap()["actual_shape"], "COM_QUERY SELECT body FROM notes WHERE tag = ?"; got != want {
			t.Fatalf("the line names the call %v, want its shape %q", got, want)
		}
	})
}

// The misses a set's first test discards are named by their shapes, unless
// KEPLOY_LOG_CONTENT is set.
func TestCaptureWindow_TheDiscardedMissesAreNamedWithoutTheValuesTheySent(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		p := &Proxy{logger: logger, errChannel: make(chan error, 16)}
		p.pendingMockErrors.addBounded(models.ParserError{ParserErrorType: models.ErrMockNotFound, MismatchReport: mysqlMiss()}, maxPendingMockErrors)
		p.BeginTestErrorCapture()
		const msg = "mock mismatches made while no test's capture window was open are not attributed to a test"
		logtest.Check(t, logs, canary, on, msg)
		shapes, _ := logs.FilterMessage(msg).All()[0].ContextMap()["call_shapes"].([]interface{})
		if len(shapes) != 1 || shapes[0] != "COM_QUERY SELECT body FROM notes WHERE tag = ?" {
			t.Fatalf("the line names the calls %v, want their shapes", shapes)
		}
	})
}

// A miss with no report has no call to shape: the line names it by its kind,
// not by its parser's text, which can quote the call (net/http quotes a
// malformed request line whole), unless KEPLOY_LOG_CONTENT is set.
func TestCaptureWindow_AMissWithoutAReportIsNamedByItsKind(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		p := &Proxy{logger: logger, errChannel: make(chan error, 16)}
		miss := models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: fmt.Errorf("malformed HTTP request %q", "GET /reset?token="+canary)}
		p.pendingMockErrors.addBounded(miss, maxPendingMockErrors)
		p.BeginTestErrorCapture()
		const msg = "mock mismatches made while no test's capture window was open are not attributed to a test"
		logtest.Check(t, logs, canary, on, msg)
		shapes, _ := logs.FilterMessage(msg).All()[0].ContextMap()["call_shapes"].([]interface{})
		if len(shapes) != 1 || shapes[0] != string(models.ErrMockNotFound) {
			t.Fatalf("the line names the miss %v, want its kind %q", shapes, models.ErrMockNotFound)
		}
	})
}

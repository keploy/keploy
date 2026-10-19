package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
	"go.uber.org/zap"
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

// malformedHTTP sends a request whose header block net/http cannot read
// through the HTTP integration in test mode, as the proxy hands it a
// connection, and returns what the integration returned: net/http's error,
// which quotes the line it could not read, the canary in it.
func malformedHTTP(t *testing.T) error {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	app, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = app.Close() })
	returned := make(chan error, 1)
	go func() {
		returned <- httpint.New(zap.NewNop()).MockOutgoing(context.Background(), agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	if _, err := app.Write([]byte("GET /reset HTTP/1.1\r\nHost: deps.example\r\nX-Reset-Token " + canary + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	err := <-returned
	if err == nil || !strings.Contains(err.Error(), canary) {
		t.Fatalf("precondition: the integration returned %v, want net/http's error quoting the header line", err)
	}
	return err
}

// A parser's error is whatever the parser said, which can quote the call it
// could not read (net/http quotes a malformed header line whole). Every line
// the proxy logs it on, as it fails the connection, queues the miss and the
// connection's handler returns it, and the line that names the miss a test's
// window never took, names it by its kind, and its text only as logcontent
// allows.
func TestMockFailed_AParsersErrorIsLoggedWithoutTheCallItQuotes(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		p := &Proxy{logger: logger, errChannel: make(chan error, 16)}
		err := malformedHTTP(t)
		// As handleConnection hands it on, and the accept loop logs what the
		// handler returned.
		p.logHandlerError(p.mockFailed(logger, err))
		// The miss, made with no test's window open, is named as the next
		// window opens.
		p.pendingMockErrors.addBounded(<-p.errChannel, maxPendingMockErrors)
		p.BeginTestErrorCapture()
		const discarded = "mock mismatches made while no test's capture window was open are not attributed to a test"
		logtest.Check(t, logs, canary, on,
			"failed to mock the outgoing message",
			"mock mismatch: no matching mock for outgoing call (no structured report)",
			"failed to handle the client connection",
			discarded)
		for _, msg := range []string{"failed to mock the outgoing message", "mock mismatch: no matching mock for outgoing call (no structured report)", "failed to handle the client connection"} {
			if got, want := logs.FilterMessage(msg).All()[0].ContextMap()["error_kind"], fmt.Sprintf("%T", err); got != want {
				t.Errorf("%q names the error %v, want its kind %q", msg, got, want)
			}
		}
		shapes, _ := logs.FilterMessage(discarded).All()[0].ContextMap()["call_shapes"].([]interface{})
		if len(shapes) != 1 || shapes[0] != string(models.ErrMockNotFound) {
			t.Fatalf("the line names the miss %v, want its kind %q", shapes, models.ErrMockNotFound)
		}
	})
}

// Under the proven switch a miss is logged at Debug and handed back as a
// reportedMiss, which the accept loop logs at Debug too: by its kind, and its
// text only as logcontent allows.
func TestMockFailed_AQuietMissIsLoggedWithoutTheCallItQuotes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		p := &Proxy{logger: logger, errChannel: make(chan error, 16)}
		miss := fmt.Errorf("no recorded answer for %q: %w", "GET /reset?token="+canary, models.ErrNoMockMatched)
		got := p.mockFailed(logger, miss)
		if !errors.As(got, new(reportedMiss)) {
			t.Fatalf("precondition: mockFailed returned %T, want a reportedMiss", got)
		}
		p.logHandlerError(got)
		logtest.Check(t, logs, canary, on,
			"failed to mock the outgoing message",
			"mock mismatch: no matching mock for outgoing call (no structured report)",
			"failed to handle the client connection: no mock matched its call")
	})
}

// erringParser is a parser whose MockOutgoing fails with err.
type erringParser struct{ err error }

func (erringParser) MatchType(context.Context, []byte) bool { return true }
func (erringParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (e erringParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return e.err
}

// The MySQL replay path logs its parser's error, queues the miss and hands
// the error back to the accept loop as the other parsers' path does: by its
// kind, and its text, which can quote the packet it could not read, only as
// logcontent allows.
func TestMockMySQLOutgoing_AParsersErrorIsLoggedWithoutThePacketItQuotes(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		parserErr := fmt.Errorf("failed to decode the MySQL packet %q", "SELECT body FROM notes WHERE tag = '"+canary+"'")
		p := &Proxy{logger: logger, errChannel: make(chan error, 16),
			Integrations: map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: erringParser{err: parserErr}}}
		app, src := net.Pipe()
		t.Cleanup(func() { _ = app.Close(); _ = src.Close() })
		p.logHandlerError(p.mockMySQLOutgoing(context.Background(), src, "db:3306", nil, models.OutgoingOptions{}))
		logtest.Check(t, logs, canary, on,
			"mysql mock outgoing finished with error",
			"mock mismatch: no matching mock for outgoing call (no structured report)",
			"failed to handle the client connection")
	})
}

// A miss that cannot be queued for its test is logged by its kind, and its
// text only as logcontent allows: when the error drain has stopped, when the
// channel is full with no drain, and when the drain discards it with no
// test's window open.
func TestSendError_AMissThatCannotBeQueuedIsLoggedWithoutTheCallItQuotes(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	miss := models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: fmt.Errorf("malformed HTTP request %q", "GET /reset?token="+canary)}
	logtest.Each(t, func(t *testing.T, on bool) {
		t.Run("the drain stopped", func(t *testing.T) {
			logger, logs := logtest.Observe()
			stopped := make(chan struct{})
			close(stopped)
			p := &Proxy{logger: logger, errChannel: make(chan error), errDrainDone: stopped}
			p.errDrainActive.Store(true)
			p.SendError(miss)
			logtest.Check(t, logs, canary, on, "keploy's error drain stopped; a call no mock answered could not be reported to its test")
		})
		t.Run("the channel is full", func(t *testing.T) {
			logger, logs := logtest.Observe()
			p := &Proxy{logger: logger, errChannel: make(chan error)}
			p.SendError(miss)
			logtest.Check(t, logs, canary, on, "Error channel is full, dropping error")
		})
		t.Run("the drain discards it", func(t *testing.T) {
			logger, logs := logtest.Observe()
			p := &Proxy{logger: logger, errChannel: make(chan error, 1)}
			for range maxPendingMockErrors {
				p.pendingMockErrors.addBounded(miss, maxPendingMockErrors)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			p.StartErrorDrain(ctx)
			p.SendError(miss)
			const msg = "discarding mock error outside active test"
			untilTrue(t, func() bool { return logs.FilterMessage(msg).Len() > 0 })
			logtest.Check(t, logs, canary, on, msg)
		})
	})
}

package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
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

// An HTTP miss's next step can name the call's path whole: the HTTP parser
// writes the session call to declare, pathPrefix included, into it when the
// recording states its mocks' lifetimes. The one miss line logs that next
// step without it, unless KEPLOY_LOG_CONTENT is set.
func TestSendMockNotFoundError_TheMissLinesNextStepCarriesNoPath(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
		miss := &models.MockMismatchReport{Protocol: "HTTP", Destination: "api.test:443", ClosestMock: "mock-0",
			ActualSummary: "GET /reset/{}", MatchPhase: models.MatchPhaseExhausted,
			NextSteps: `Request structure changed since recording. Each test is served the HTTP calls it recorded. ` +
				`(sessionCalls: [{host: "api.test", method: "GET", pathPrefix: "/reset/` + canary + `"}])`}
		p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, miss))
		logtest.Check(t, logs, canary, on, "mock mismatch: no matching mock for outgoing call")
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
// through the HTTP integration in test mode, logging to logger, as the proxy
// hands it a connection, and returns what the integration returned: net/http's
// error, which quotes the line it could not read, the canary in it.
func malformedHTTP(t *testing.T, logger *zap.Logger) error {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	app, agentEnd := net.Pipe()
	t.Cleanup(func() { _ = app.Close() })
	returned := make(chan error, 1)
	go func() {
		returned <- httpint.New(logger).MockOutgoing(context.Background(), agentEnd, &models.ConditionalDstCfg{}, mm, models.OutgoingOptions{})
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
// could not read (net/http quotes a malformed header line whole). The
// parser's own lines for it, every line the proxy logs it on, as it fails the
// connection, queues the miss and the connection's handler returns it, and
// the line that names the miss a test's window never took, name it by its
// kind, and its text only as logcontent allows.
func TestMockFailed_AParsersErrorIsLoggedWithoutTheCallItQuotes(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		p := &Proxy{logger: logger, errChannel: make(chan error, 16)}
		err := malformedHTTP(t, logger)
		// As handleConnection hands it on, and the accept loop logs what the
		// handler returned.
		p.logHandlerError(p.mockFailed(logger, err))
		// The miss, made with no test's window open, is named as the next
		// window opens.
		p.pendingMockErrors.addBounded(<-p.errChannel, maxPendingMockErrors)
		p.BeginTestErrorCapture()
		const discarded = "mock mismatches made while no test's capture window was open are not attributed to a test"
		logtest.Check(t, logs, canary, on,
			"failed to parse the http request message",
			"failed to decode the http message from the yaml",
			"failed to mock the outgoing message",
			"mock mismatch: no matching mock for outgoing call (no structured report)",
			"failed to handle the client connection",
			discarded)
		// Nor by its text's fingerprint, which whoever hashes the texts
		// net/http makes around a header line finds a short value from.
		logtest.CheckNoFingerprint(t, logs, err.Error())
		for _, msg := range []string{"failed to mock the outgoing message", "mock mismatch: no matching mock for outgoing call (no structured report)", "failed to handle the client connection"} {
			if got, want := logs.FilterMessage(msg).All()[0].ContextMap()["error_kind"], "textproto.ProtocolError"; got != want {
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
		logtest.CheckNoFingerprint(t, logs, parserErr.Error())
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

// recordThroughTheHTTPParser records one exchange through recordViaSupervisor
// and the HTTP parser, as a proxied connection is recorded: the app sends
// request, and the upstream answers response once it has read the request's
// header block, which net/http need not be able to read. The relay forwards
// the exchange whatever the parser makes of it.
func recordThroughTheHTTPParser(t *testing.T, logger *zap.Logger, request, response string) {
	t.Helper()
	supervisor.ResetLeftOutWarningsForTest()
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(nil)))
	defer cancel()
	app, rawSrc, cleanupSrc := tcpConnPair(t)
	defer cleanupSrc()
	dstConn, upstream, cleanupDst := tcpConnPair(t)
	defer cleanupDst()
	srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}
	p := &Proxy{logger: logger, recordBufferCap: 1 << 20, recordBufferQueueSize: 64,
		recordBufferStallGrace: 2 * time.Second, recordBufferHalfCloseGrace: 200 * time.Millisecond}
	go func() {
		head := bufio.NewReader(upstream)
		for {
			line, err := head.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = upstream.Write([]byte(response))
		_ = upstream.Close()
	}()
	done := make(chan error, 1)
	go func() {
		done <- p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), httpint.New(logger), integrations.HTTP,
			make(chan *models.Mock, 8), &errgroup.Group{}, logger, 1, 2, models.OutgoingOptions{})
	}()
	if _, err := app.Write([]byte(request)); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	if got, err := io.ReadAll(app); err != nil || !strings.HasPrefix(string(got), "HTTP/1.1 200") {
		t.Fatalf("the app read %q, %v; want the upstream's answer, relayed, and its close", got, err)
	}
	_ = app.Close()
	if err := awaitEvent(t, done, "recordViaSupervisor returning after the connection ended"); err != nil {
		t.Fatalf("recordViaSupervisor returned %v", err)
	}
}

// On the record path a parser's error can quote the call it could not read,
// as on replay: net/http quotes a header line it cannot read whole, and
// strconv a Content-Length that is not a number. The HTTP parser's line for
// it, the WARN that says the exchange was not recorded, and the dispatcher's
// line as it falls through to passthrough name it by its kind, and its text
// only as logcontent allows. The WARN still says what the parser stopped on.
func TestRecordViaSupervisor_AParsersErrorIsLoggedWithoutTheCallItQuotes(t *testing.T) {
	const fallthroughMsg = "parser supervisor triggered passthrough fallback; relay continues raw forwarding until peer close"
	for name, c := range map[string]struct {
		request, response, parserLine, reason string
	}{
		"a header line net/http cannot read": {
			"GET /reset HTTP/1.1\r\nHost: upstream\r\nX-Reset-Token " + canary + "\r\n\r\n",
			"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
			"V2 HTTP record: failed to build mock", "http decode error: parse request"},
		"a request's length that is not a number": {
			"POST /reset HTTP/1.1\r\nHost: upstream\r\nContent-Length: " + canary + "\r\n\r\n",
			"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok",
			"V2 HTTP record: failed to read full request", "http decode error: request read failed"},
		"a response's length that is not a number": {
			"GET /reset HTTP/1.1\r\nHost: upstream\r\n\r\n",
			"HTTP/1.1 200 OK\r\nContent-Length: " + canary + "\r\n\r\nok",
			"V2 HTTP record: failed to read full response", "http decode error: response read failed"},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				recordThroughTheHTTPParser(t, logger, c.request, c.response)
				logtest.Check(t, logs, canary, on, c.parserLine, supervisor.LeftOutWarnMsg, fallthroughMsg)
				if got := logs.FilterMessage(supervisor.LeftOutWarnMsg).All()[0].ContextMap()["reason"]; got != c.reason {
					t.Fatalf("the WARN says the exchange was left out for %q, want %q", got, c.reason)
				}
			})
		})
	}
}

// closedUnderCancelParser is a V2 parser that, once the recording stops,
// returns a read error that quotes what it was reading and reads as a closed
// connection, as a parser that wraps its read's error does.
type closedUnderCancelParser struct{ stubParser }

func (closedUnderCancelParser) RecordOutgoing(ctx context.Context, _ *integrations.RecordSession) error {
	<-ctx.Done()
	return fmt.Errorf("read the request after %q: use of closed network connection: %w", "X-Reset-Token "+canary, ctx.Err())
}

// A parser that returns as the recording stops, with an error that reads as
// a closed connection, is logged by its error's kind, and its text only as
// logcontent allows.
func TestRecordViaSupervisor_AClosedConnectionsErrorIsLoggedWithoutWhatItQuotes(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(nil)))
		app, rawSrc, cleanupSrc := tcpConnPair(t)
		defer cleanupSrc()
		dstConn, _, cleanupDst := tcpConnPair(t)
		defer cleanupDst()
		p := &Proxy{logger: logger, recordBufferCap: 1 << 20, recordBufferQueueSize: 64,
			recordBufferStallGrace: 2 * time.Second, recordBufferHalfCloseGrace: 200 * time.Millisecond}
		done := make(chan error, 1)
		go func() {
			done <- p.recordViaSupervisor(ctx, rawSrc, connseq.NewUpstream(dstConn), closedUnderCancelParser{}, integrations.HTTP,
				make(chan *models.Mock, 8), &errgroup.Group{}, logger, 1, 2, models.OutgoingOptions{})
		}()
		cancel()
		_ = app.Close()
		_ = awaitEvent(t, done, "recordViaSupervisor returning after the recording stopped")
		logtest.Check(t, logs, canary, on, "V2 parser exited with network-closed error")
	})
}

// parserFailed only says how the accept loop logs a parser's error: what the
// handler's line is, and whether there is one, is what it was for the error
// itself. A parser that returns a bare io.EOF (a client that connects and
// closes without sending, as a health probe or a pre-dialled pool does) gets
// no line, wrapped or not; one that returns a refused local dial, a closed
// connection or a cancel gets the Debug line it gets unwrapped.
func TestLogHandlerError_AParsersErrorIsClassifiedAsItWasBeforeItWasWrapped(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5432}, Err: syscall.ECONNREFUSED}
	for name, err := range map[string]error{
		"a clean EOF":               io.EOF,
		"a closed connection":       net.ErrClosed,
		"a cancel":                  context.Canceled,
		"a refused local dial":      refused,
		"a reset":                   errors.New("read tcp: connection reset by peer"),
		"a parser's own error":      errors.New("malformed frame"),
		"a wrapped EOF":             fmt.Errorf("read the header: %w", io.EOF),
		"a parser's unexpected EOF": io.ErrUnexpectedEOF,
	} {
		t.Run(name, func(t *testing.T) {
			lines := func(err error) []string {
				logger, logs := logtest.Observe()
				(&Proxy{logger: logger}).logHandlerError(err)
				var out []string
				for _, e := range logs.AllUntimed() {
					out = append(out, e.Level.String()+" "+e.Message)
				}
				return out
			}
			bare, wrapped := lines(err), lines(parserFailed{err})
			if strings.Join(bare, "\n") != strings.Join(wrapped, "\n") {
				t.Fatalf("the handler's lines for parserFailed{%v} are %q, want those for the error itself, %q", err, wrapped, bare)
			}
		})
	}
}

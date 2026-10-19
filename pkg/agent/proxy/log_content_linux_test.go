//go:build linux

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
	"golang.org/x/sync/errgroup"
)

// legacyFailingParser records on the legacy path (it is no V2 parser) and
// fails with err.
type legacyFailingParser struct{ err error }

func (legacyFailingParser) MatchType(context.Context, []byte) bool { return true }
func (p legacyFailingParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return p.err
}
func (legacyFailingParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return nil
}

// malformedHeaderError is net/http's error for a header line it cannot read,
// which quotes the line whole, the canary in it.
func malformedHeaderError(t *testing.T) error {
	t.Helper()
	_, err := http.ReadRequest(bufio.NewReader(strings.NewReader("GET /reset HTTP/1.1\r\nHost: deps.example\r\nX-Reset-Token " + canary + "\r\n\r\n")))
	if err == nil || !strings.Contains(err.Error(), canary) {
		t.Fatalf("precondition: net/http returned %v, want its error quoting the header line", err)
	}
	return err
}

// A parser's error on the legacy record path, as the handler logs it and
// returns it, and the accept loop logs what the handler returned: by its
// kind, and its text only as logcontent allows. That holds for one that reads
// as a closed connection too, which both log at Debug.
func TestRecordOutgoing_ALegacyParsersErrorIsLoggedWithoutTheCallItQuotes(t *testing.T) {
	for name, c := range map[string]struct {
		err          func(*testing.T) error
		record, conn string
	}{
		"net/http's": {malformedHeaderError,
			"failed to record the outgoing message", "failed to handle the client connection"},
		"one that reads as a closed connection": {func(*testing.T) error {
			return fmt.Errorf("read the request after %q: use of closed network connection", "X-Reset-Token "+canary)
		}, "failed to record the outgoing message (connection closed)", "failed to handle the client connection (connection closed)"},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				p, _, err := serveThroughHandler(t, models.MODE_RECORD, legacyFailingParser{err: c.err(t)}, logger)
				p.logHandlerError(err)
				logtest.Check(t, logs, canary, on, c.record, c.conn)
			})
		})
	}
}

// The MySQL probe branch's and the generic parser's legacy record paths log a
// parser's error, and hand it to the accept loop, as the matched parser's
// path does: by its kind, its cause however the parser wrapped it.
func TestRecordOutgoing_TheProbedAndGenericLegacyPathsLogAParsersErrorByItsKind(t *testing.T) {
	for _, typ := range []integrations.IntegrationType{integrations.MYSQL, integrations.GENERIC} {
		t.Run(string(typ), func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				parserErr := fmt.Errorf("decode the upstream's reply: %w", malformedHeaderError(t))
				p := &Proxy{logger: logger, Integrations: map[integrations.IntegrationType]integrations.Integrations{typ: legacyFailingParser{err: parserErr}}}
				srcRaw, destRaw, cleanup := tcpConnPair(t)
				defer cleanup()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
				ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
				errGrp, gctx := errgroup.WithContext(ctx)
				gctx = context.WithValue(gctx, models.ErrGroupKey, errGrp)
				var err error
				if typ == integrations.MYSQL {
					err = p.recordMySQLOutgoing(gctx, srcRaw, destRaw, make(chan *models.Mock, 8), errGrp, logger, 1, 2, models.OutgoingOptions{}, nil)
				} else {
					err = p.recordGenericOutgoing(gctx, srcRaw, destRaw, make(chan *models.Mock, 8), errGrp, logger, 1, 2, models.OutgoingOptions{})
				}
				p.logHandlerError(err)
				logtest.Check(t, logs, canary, on, "failed to record the outgoing message", "failed to handle the client connection")
				for _, msg := range []string{"failed to record the outgoing message", "failed to handle the client connection"} {
					if got := logs.FilterMessage(msg).All()[0].ContextMap()["error_kind"]; got != "textproto.ProtocolError" {
						t.Errorf("%q names the error %v, want its cause's kind, textproto.ProtocolError", msg, got)
					}
				}
			})
		})
	}
}

// A parser's panic on the legacy record path is named by what it is, keploy's
// failure, on the handler's line and the accept loop's: util.ErrParserPanicked
// is text alone, which logcontent.ErrorKind would name only by its Go type.
func TestRecordOutgoing_AParsersPanicIsNamedByItsKind(t *testing.T) {
	logger, logs := logtest.Observe()
	p, _, err := serveThroughHandler(t, models.MODE_RECORD, matchedPanicParser{}, logger)
	p.logHandlerError(err)
	for _, msg := range []string{"failed to record the outgoing message", "failed to handle the client connection"} {
		lines := logs.FilterMessage(msg).All()
		if len(lines) != 1 {
			t.Fatalf("%q logged %d time(s), want once", msg, len(lines))
		}
		if got := lines[0].ContextMap()["error_kind"]; got != "parser panicked" {
			t.Errorf("%q names the error %v, want \"parser panicked\"", msg, got)
		}
	}
}

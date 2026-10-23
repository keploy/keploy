//go:build linux

package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// matchedPanicParser is a parser (not a V2 one, so it records on the legacy
// path) that matches every connection and whose goroutine panicked: it
// returns util.ErrParserPanicked, as util.RecoverInto makes it, or, with
// inHandler, panics in the handler's own goroutine.
type matchedPanicParser struct{ inHandler bool }

func (matchedPanicParser) MatchType(context.Context, []byte) bool { return true }
func (matchedPanicParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return fmt.Errorf("parser: %w", util.ErrParserPanicked)
}
func (m matchedPanicParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	if m.inHandler {
		panic("a parser bug")
	}
	return fmt.Errorf("parser: %w", util.ErrParserPanicked)
}

// panicLoss is the one capture loss p counted, failing the test unless it is
// a parser panic whose What starts with what.
func panicLoss(t *testing.T, p *Proxy, what string) {
	t.Helper()
	got := p.CaptureLosses()
	if len(got) != 1 || got[0].Kind != models.CaptureLossParserPanic || got[0].Count != 1 || !strings.HasPrefix(got[0].What, what) {
		t.Fatalf("capture losses %+v, want one %s starting %q", got, models.CaptureLossParserPanic, what)
	}
}

// throughHandler serves one connection through handleRecovered, as the accept
// loop does, with parser as the only one, in mode: in record mode to an
// upstream that accepts it.
func throughHandler(t *testing.T, mode models.Mode, parser integrations.Integrations, logger *zap.Logger) (*Proxy, string) {
	t.Helper()
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(containerAt{addr: "none"})
	t.Cleanup(func() { containerkey.Default = saved })
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	upstream := ln.Addr().String()
	cfg := config.New()
	cfg.Agent.Mode = mode
	p := New(logger, fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: parser}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
	if mode == models.MODE_RECORD {
		if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	} else if err := p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true}); err != nil {
		t.Fatal(err)
	}
	app, src := acceptedConn(t)
	t.Cleanup(func() { _ = app.Close() })
	done := make(chan error, 1)
	go func() { done <- p.handleRecovered(context.Background(), src) }()
	if _, err := io.WriteString(app, "GET / HTTP/1.1\r\nHost: up\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, done, "the handler returning")
	return p, upstream
}

// Every place the proxy hands a connection to a parser, to mock it or to
// record it, counts the parser's panic as a capture loss of that parser and
// destination, and so does the handler around them for a panic in its own
// goroutine. Before, a panicked parser's goroutine read as a clean end, and a
// panic in the handler was recovered and nothing counted it.
func TestEveryParserDispatchCountsAParserPanic(t *testing.T) {
	t.Run("mock, a matched parser", func(t *testing.T) {
		p, upstream := throughHandler(t, models.MODE_TEST, matchedPanicParser{}, zap.NewNop())
		panicLoss(t, p, "the http parser, on a connection to "+upstream)
	})
	t.Run("record, a matched parser", func(t *testing.T) {
		p, upstream := throughHandler(t, models.MODE_RECORD, matchedPanicParser{}, zap.NewNop())
		panicLoss(t, p, "the http parser, on a connection to "+upstream)
	})
	t.Run("a panic in the handler's goroutine", func(t *testing.T) {
		p, _ := throughHandler(t, models.MODE_TEST, matchedPanicParser{inHandler: true}, zap.NewNop())
		panicLoss(t, p, "the proxy's connection handler, on a connection from 127.0.0.1:")
	})
	t.Run("mock, MySQL", func(t *testing.T) {
		p := &Proxy{logger: zap.NewNop(), Integrations: map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: matchedPanicParser{}}}
		srcRaw, _, cleanup := tcpConnPair(t)
		defer cleanup()
		_ = p.mockMySQLOutgoing(context.Background(), srcRaw, "10.0.0.5:3306", nil, models.OutgoingOptions{})
		panicLoss(t, p, "the mysql parser, on a connection to 10.0.0.5:3306")
	})
	for _, typ := range []integrations.IntegrationType{integrations.MYSQL, integrations.GENERIC} {
		t.Run("record, "+string(typ), func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), Integrations: map[integrations.IntegrationType]integrations.Integrations{typ: matchedPanicParser{}}}
			srcRaw, destRaw, cleanup := tcpConnPair(t)
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
			ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")
			errGrp, gctx := errgroup.WithContext(ctx)
			gctx = context.WithValue(gctx, models.ErrGroupKey, errGrp)
			if typ == integrations.MYSQL {
				_ = p.recordMySQLOutgoing(gctx, srcRaw, destRaw, make(chan *models.Mock, 8), errGrp, zap.NewNop(), 1, 2, models.OutgoingOptions{}, nil)
			} else {
				_ = p.recordGenericOutgoing(gctx, srcRaw, destRaw, make(chan *models.Mock, 8), errGrp, zap.NewNop(), 1, 2, models.OutgoingOptions{})
			}
			panicLoss(t, p, "the "+string(typ)+" parser, on a connection to "+destRaw.RemoteAddr().String())
		})
	}
}

// A parser's panic is keploy's failure, not a miss: with the switch off it is
// not logged as a mock mismatch, and it takes no place among the misses kept
// for a test's read.
func TestAParserPanicIsNoMockMismatch(t *testing.T) {
	for _, site := range []string{"a matched parser", "MySQL"} {
		t.Run(site, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			if site == "MySQL" {
				p := &Proxy{logger: zap.New(core), Integrations: map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: matchedPanicParser{}}}
				srcRaw, _, cleanup := tcpConnPair(t)
				defer cleanup()
				_ = p.mockMySQLOutgoing(context.Background(), srcRaw, "10.0.0.5:3306", nil, models.OutgoingOptions{})
			} else {
				throughHandler(t, models.MODE_TEST, matchedPanicParser{}, zap.New(core))
			}
			if n := logs.FilterMessageSnippet("mock mismatch").Len(); n != 0 {
				t.Fatalf("a parser's panic was logged as a mock mismatch %d time(s)", n)
			}
			if n := logs.FilterMessage("failed to mock the outgoing message").Len(); n != 1 {
				t.Fatalf("the parser's panic was logged %d time(s) as its failure, want once", n)
			}
		})
	}
}

package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// An HTTP call with no Host header (an HTTP/1.0 client, an instance-metadata
// probe) that no mock answers is reported with the address it was dialled at.
// The HTTP parser names a call's upstream by its Host, then its URL's
// authority; with neither, the miss's log line and the miss kept for its test
// said destination "", nothing of where the call went.
func TestAMissWithNoHostNamesTheAddressItWasDialledAt(t *testing.T) {
	const dst = "10.96.0.9:80"
	app, src := acceptedConn(t)
	p := replayProxyTo(t, dst, models.OutgoingOptions{})
	core, logs := observer.New(zap.DebugLevel)
	p.logger = zap.New(core)
	p.errChannel = make(chan error, 4)
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: httpint.New(zap.NewNop())}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	if _, err := app.Write([]byte("GET /latest/meta-data/ HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// keploy answers the call it has no mock for, or closes: either way the
	// miss was reported by then.
	if resp, err := http.ReadResponse(bufio.NewReader(app), nil); err == nil {
		_ = resp.Body.Close()
	}
	_ = app.Close()
	_ = awaitEvent(t, done, "handleConnection returning after the client left")

	entries := logs.FilterMessageSnippet("mock mismatch: no matching mock for outgoing call").All()
	if len(entries) != 1 {
		t.Fatalf("mismatch log lines = %d, want 1: %v", len(entries), logs.All())
	}
	if got := entries[0].ContextMap()["destination"]; got != dst {
		t.Errorf("the mismatch line names destination %q, want the dialled %q", got, dst)
	}
	var queued []string
	for len(p.errChannel) > 0 {
		var pe models.ParserError
		if err := <-p.errChannel; errors.As(err, &pe) && pe.MismatchReport != nil {
			queued = append(queued, pe.MismatchReport.Destination)
		}
	}
	if len(queued) != 1 || queued[0] != dst {
		t.Fatalf("the misses kept for the test name destinations %q, want one naming %q", queued, dst)
	}
}

// A parser's own destination is kept, and a fabricated address
// (models.ConditionalDstCfg.AddrFabricated), which names no server, is not
// named for a report without one.
func TestAMissKeepsItsParsersDestinationAndNamesNoFabricatedAddress(t *testing.T) {
	for _, tc := range []struct {
		name, parser string
		dst          *models.ConditionalDstCfg
		want         string
	}{
		{"the parser's destination", "api.example.com", &models.ConditionalDstCfg{Addr: "10.0.0.5:80"}, "api.example.com"},
		{"a fabricated address", "", &models.ConditionalDstCfg{Addr: "127.0.0.1:3306", AddrFabricated: true}, ""},
		{"no address known", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 1)}
			report := &models.MockMismatchReport{Protocol: "HTTP", Destination: tc.parser, ActualSummary: "GET /"}
			p.sendMockNotFoundErrorFrom(models.NewMockMismatchError(models.ErrNoMockMatched, report), 0, tc.dst)
			var pe models.ParserError
			if err := <-p.errChannel; !errors.As(err, &pe) || pe.MismatchReport == nil {
				t.Fatalf("no miss kept: %v", err)
			}
			if got := pe.MismatchReport.Destination; got != tc.want {
				t.Fatalf("destination %q, want %q", got, tc.want)
			}
			if report.Destination != tc.parser {
				t.Fatalf("the parser's report was changed: %q", report.Destination)
			}
		})
	}
}

// returnedMiss is a parser that takes every connection, reads its call and
// returns a miss whose report names no destination: the miss is returned to
// the proxy, not handed to the test's reporter as the HTTP parser does.
type returnedMiss struct{ integrations.Integrations }

func (returnedMiss) MatchType(context.Context, []byte) bool { return true }

func (returnedMiss) MockOutgoing(_ context.Context, src net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	_, _ = src.Read(make([]byte, 64))
	report := &models.MockMismatchReport{Protocol: "fake", ActualSummary: "PING"}
	return models.NewMockMismatchError(fmt.Errorf("fake: %w", models.ErrNoMockMatched), report)
}

// A miss its parser returns, and that names no destination, is reported by
// the proxy (mockFailedFrom) with the address the connection was dialled at:
// the generic parser, which takes every protocol keploy does not parse,
// builds its reports with no destination, and so may any parser that matched.
func TestAReturnedMissWithNoDestinationNamesTheAddressItWasDialledAt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parsers  map[integrations.IntegrationType]integrations.Integrations
		priority []ParserPriority
	}{
		{"the generic parser", map[integrations.IntegrationType]integrations.Integrations{integrations.GENERIC: generic.New(zap.NewNop())}, nil},
		{"a parser that matched the call", map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: returnedMiss{}}, []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const dst = "10.96.0.7:7000"
			app, src := acceptedConn(t)
			defer func() { _ = app.Close() }()
			p := replayProxyTo(t, dst, models.OutgoingOptions{})
			p.errChannel = make(chan error, 4)
			p.Integrations = tc.parsers
			p.integrationsPriority = tc.priority
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()
			if _, err := app.Write([]byte("PING\r\n")); err != nil {
				t.Fatal(err)
			}
			if err := awaitEvent(t, done, "handleConnection returning the miss"); !isMockMiss(err) {
				t.Fatalf("handleConnection = %v, want the parser's miss", err)
			}
			var queued []string
			for len(p.errChannel) > 0 {
				var pe models.ParserError
				if err := <-p.errChannel; errors.As(err, &pe) && pe.MismatchReport != nil {
					queued = append(queued, pe.MismatchReport.Destination)
				}
			}
			if len(queued) != 1 || queued[0] != dst {
				t.Fatalf("the misses kept for the test name destinations %q, want one naming %q", queued, dst)
			}
		})
	}
}

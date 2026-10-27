package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// panickedParser is a parser whose goroutine panicked while it served the
// connection: it returns util.ErrParserPanicked, as util.RecoverInto makes
// it.
type panickedParser struct{ integrations.Integrations }

func (panickedParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return fmt.Errorf("generic: %w", util.ErrParserPanicked)
}

// A replay parser that panicked served nothing: the proxy counts it as a
// capture loss of the connection's destination, which under the proven switch
// fails the test (CAPTURE_INCOMPLETE), and returns the error. It is no miss:
// the application's call may well have a recording.
func TestAParserThatPanickedIsACaptureLoss(t *testing.T) {
	p := &Proxy{
		logger:       zap.NewNop(),
		Integrations: map[integrations.IntegrationType]integrations.Integrations{integrations.GENERIC: panickedParser{}},
	}
	srcRaw, _, cleanup := tcpConnPair(t)
	defer cleanup()
	err := p.mockGenericOutgoing(context.Background(), srcRaw, &models.ConditionalDstCfg{Addr: "10.0.0.5:6379"}, nil, models.OutgoingOptions{}, zap.NewNop())
	if !errors.Is(err, util.ErrParserPanicked) {
		t.Fatalf("mockGenericOutgoing = %v, want the parser's panic", err)
	}
	want := models.CaptureLosses{{Kind: models.CaptureLossParserPanic, Count: 1, What: "the generic parser, on a connection to 10.0.0.5:6379"}}
	if got := p.CaptureLosses(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("capture losses %+v, want %+v", got, want)
	}
}

// Only a panic is: a parser's other ends, a miss among them, are not.
func TestOnlyAPanicIsAParserPanicLoss(t *testing.T) {
	p := &Proxy{}
	for _, err := range []error{nil, io.EOF, context.Canceled, models.ErrNoMockMatched, errors.New("decode failed")} {
		p.countParserPanic(err, integrations.HTTP, "10.0.0.5:80")
	}
	if got := p.CaptureLosses(); len(got) != 0 {
		t.Fatalf("capture losses %+v, want none", got)
	}
	p.countParserPanic(util.ErrParserPanicked, integrations.HTTP, "10.0.0.5:80")
	if got := p.CaptureLosses(); len(got) != 1 || got[0].Kind != models.CaptureLossParserPanic || got[0].Count != 1 {
		t.Fatalf("capture losses %+v, want one parser panic", got)
	}
}

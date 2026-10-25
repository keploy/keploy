package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/generic"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// TestRecordViaSupervisor_GenericPassesAResetOn drives the live record path
// of a connection no parser matched: generic, which is V2, through
// recordViaSupervisor and its relay, over real sockets, with the client side
// wrapped and both sides closed the way handleConnection does it. When one
// peer resets, the other must read a reset, as it would with no proxy in
// between, and not the FIN that calls a cut stream complete. The exchange
// before the reset is still recorded.
func TestRecordViaSupervisor_GenericPassesAResetOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// resetter is the peer that resets; the other one must read it.
		resetter, reader func(*genericV2Run) *net.TCPConn
	}{
		{"the destination resets", func(r *genericV2Run) *net.TCPConn { return r.server },
			func(r *genericV2Run) *net.TCPConn { return r.client }},
		{"the client resets", func(r *genericV2Run) *net.TCPConn { return r.client },
			func(r *genericV2Run) *net.TCPConn { return r.server }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := startGenericV2(t)
			request, reply := []byte("PING\r\n"), []byte("PONG\r\n")
			if _, err := run.client.Write(request); err != nil {
				t.Fatalf("write the request: %v", err)
			}
			readExactly(t, run.server, request, "the destination")
			if _, err := run.server.Write(reply); err != nil {
				t.Fatalf("write the reply: %v", err)
			}
			readExactly(t, run.client, reply, "the app")

			resetAndClose(t, tc.resetter(run))
			awaitEvent(t, run.returned, "the record path's return after the reset")
			if err := readEnd(tc.reader(run)); !neterr.IsConnReset(err) {
				t.Fatalf("the other peer's read ended with %v, want a reset. The record relay "+
					"ended on the reset but did not pass it on, so the caller's close sent a FIN", err)
			}
			m := awaitEvent(t, run.mocks, "the exchange's mock")
			if len(m.Spec.GenericRequests) != 1 || len(m.Spec.GenericResponses) != 1 ||
				m.Spec.GenericRequests[0].Message[0].Data != string(request) ||
				m.Spec.GenericResponses[0].Message[0].Data != string(reply) {
				t.Fatalf("the mock is %+v, want the one exchange %q -> %q", m.Spec, request, reply)
			}
		})
	}
}

// genericV2Run is one connection recorded by generic through
// recordViaSupervisor: the app's end, the destination service's end, the
// mocks it recorded, and a channel closed once the record path has returned
// and both of the proxy's connections are closed.
type genericV2Run struct {
	client, server *net.TCPConn
	mocks          chan *models.Mock
	returned       chan struct{}
}

func startGenericV2(t *testing.T) *genericV2Run {
	t.Helper()
	clientApp, rawSrc := tcpPair(t)
	dstConn, destSvc := tcpPair(t)
	// What handleConnection hands the dispatcher.
	srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}
	upstream := connseq.NewUpstream(dstConn)

	p := &Proxy{
		recordBufferCap:        64 * 1024,
		recordBufferQueueSize:  64,
		recordBufferStallGrace: 2 * time.Second,
	}
	run := &genericV2Run{client: clientApp, server: destSvc,
		mocks: make(chan *models.Mock, 8), returned: make(chan struct{})}
	// The V2 path emits through the recording's mock manager, not the
	// channel it is given.
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(run.mocks)
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	t.Cleanup(cancel)
	go func() {
		defer close(run.returned)
		_ = p.recordViaSupervisor(ctx, srcConn, upstream, generic.New(zap.NewNop()), integrations.GENERIC,
			run.mocks, &errgroup.Group{}, zap.NewNop(), 1, 2, models.OutgoingOptions{})
		// handleConnection's deferred close, once the parser has returned.
		_ = srcConn.Close()
		_ = upstream.Close()
	}()
	return run
}

func readExactly(t *testing.T, c net.Conn, want []byte, who string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != string(want) {
		t.Fatalf("%s read %q, %v; want %q", who, got, err, want)
	}
}

// resetAndClose closes c with SO_LINGER 0, so its peer gets a reset.
func resetAndClose(t *testing.T, c *net.TCPConn) {
	t.Helper()
	if err := c.SetLinger(0); err != nil {
		t.Fatalf("set linger: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// readEnd reads c until it ends and returns how: io.EOF for a FIN, the
// error otherwise.
func readEnd(c net.Conn) error {
	buf := make([]byte, 64)
	for {
		if _, err := c.Read(buf); err != nil {
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return err
		}
	}
}

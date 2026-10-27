package integrations

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// keptConn is a connection's request aloft as the proxy's kept connection
// holds it (FlightCarrier, trackedConn): a write lands it and leaves none
// aloft.
type keptConn struct {
	mu sync.Mutex
	f  *learn.Flight
}

func (*keptConn) Hold() func() { return func() {} }
func (k *keptConn) InFlight() *learn.Flight {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.f
}
func (k *keptConn) land() {
	k.mu.Lock()
	f := k.f
	k.f = nil
	k.mu.Unlock()
	f.Land()
}

// landsOnClose is the client connection as the TLS layer the proxy serves a
// kept connection over hands it to a parser: its close writes close_notify,
// which lands the request aloft on k as an answer's first bytes would
// (trackedConn.Write). closed: it was closed.
type landsOnClose struct {
	net.Conn
	k      *keptConn
	closed *atomic.Bool
}

func (c landsOnClose) Close() error {
	c.k.land()
	c.closed.Store(true)
	return c.Conn.Close()
}

// A parser that panics with a request aloft on its connection writes no more
// of an answer to it. RecoverCtx and RecoverIntoCtx end that request untold
// (learn.Flight.EndUntold) before they close the connection the parser was
// handed, whose close over TLS writes close_notify and would land it: a serve
// told in it is in an order keploy cannot tell (0,0), never one the
// application had its answer to (1,1). A ctx with no request aloft is
// pUtil.Recover's and pUtil.RecoverInto's: the panic is recovered and the
// connection closed, and the close lands it.
func TestTheCtxRecoversEndTheRequestAloftUntoldBeforeTheyClose(t *testing.T) {
	for _, into := range []bool{false, true} {
		for _, aloft := range []bool{true, false} {
			tr := learn.NewTracker()
			tr.Begin()
			k := &keptConn{f: tr.Lift()}
			told := &models.Mock{Name: "told-before-the-panic"}
			tr.ServedMock(told)
			ctx := learn.WithTracker(context.Background(), tr)
			if aloft {
				ctx = WithRequestHolder(ctx, k)
			}
			app, proxy := net.Pipe()
			client := landsOnClose{Conn: proxy, k: k, closed: &atomic.Bool{}}
			result := make(chan error, 1)
			// Each form is deferred itself: recover stops a panic only in
			// the function the panicking goroutine defers.
			func() {
				if into {
					defer RecoverIntoCtx(ctx, zap.NewNop(), client, nil, result)
				} else {
					defer RecoverCtx(ctx, zap.NewNop(), client, nil)
				}
				panic("a parser bug mid-request")
			}()
			_ = app.Close()
			if !client.closed.Load() {
				t.Fatalf("into %v, aloft %v: the client connection is not closed", into, aloft)
			}
			if into {
				if err := <-result; !errors.Is(err, pUtil.ErrParserPanicked) {
					t.Fatalf("aloft %v: the goroutine's result is %v, want ErrParserPanicked", aloft, err)
				}
			}
			first, last := tr.Placed().Order.Of(told)
			if aloft && (first != 0 || last != 0) {
				t.Fatalf("into %v: the serve told in the request aloft when the parser panicked is at %d,%d; want 0,0: the application was given no answer", into, first, last)
			}
			if !aloft && (first != 1 || last != 1) {
				t.Fatalf("into %v, no request aloft on ctx: the serve is at %d,%d; want 1,1, as pUtil.Recover leaves it (the close landed it)", into, first, last)
			}
		}
	}
}

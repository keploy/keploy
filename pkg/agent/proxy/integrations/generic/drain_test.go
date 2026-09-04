package generic

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// testDrainIdle keeps these quick; the drain's logic does not depend on
// the size of the window, only on whether the survivor moved bytes during
// one. The production value is pinned separately, in the util package.
const testDrainIdle = 400 * time.Millisecond

// splitReadConn fails on Read while Write keeps working — what Go's
// tls.Conn does, since it keeps c.in.err and c.out.err separate. Recording
// reaches encodeGeneric with SafeConns over *tls.Conn on every
// TLS-intercepted path, so one direction failing while the other stays
// healthy is the real case, not a contrivance.
type splitReadConn struct {
	net.Conn
	readErr error
}

func (c *splitReadConn) Read(p []byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	return c.Conn.Read(p)
}

// Delegated because embedding net.Conn as an INTERFACE promotes only
// net.Conn's method set — a fake that silently swallows CloseWrite
// certifies nothing about a path whose whole subject is the FIN.
func (c *splitReadConn) CloseWrite() error { return proxyutil.CloseWriteIfPossible(c.Conn) }

// TestEncodeGeneric_DrainsTheSurvivorInBothDirections is the generic-path
// twin of the relay's drain test. What it loses when it goes wrong is
// worse than the relay's: the user's bytes AND the mocks the tee captured
// from them.
//
// Scope it honestly, though. encodeGeneric is reached only via
// Generic.recordLegacy, which needs session.V2 == nil, and
// shouldRecordViaSupervisor routes to the supervisor unless
// newRelayDisabled(). Every in-tree parser reports IsV2() == true, so this
// path — like http's and mysql's siblings — runs only under
// KEPLOY_NEW_RELAY=off. It is the rollback, not the default.
//
// Both orientations are driven on purpose. The survivor is whichever
// direction did NOT report, so the drain has to poll that direction's byte
// counter; code that assumes the first result came from the first copy
// goroutine watches the counter of the direction that just died, which
// never advances again — the first tick reads "stalled" and the drain
// collapses into the fixed wall clock it replaced. That is invisible in
// one orientation and total data loss in the other.
func TestEncodeGeneric_DrainsTheSurvivorInBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name      string
		breakDest bool
	}{
		{"client read half fails, response survives", false},
		{"upstream read half fails, request survives", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientApp, srcProxy := prodPair(t, true)
			destSvc, dstProxy := prodPair(t, false)

			clientConn, destConn := srcProxy, dstProxy
			var sender, receiver net.Conn
			if tc.breakDest {
				destConn = &splitReadConn{Conn: dstProxy, readErr: io.ErrUnexpectedEOF}
				sender, receiver = clientApp, destSvc
			} else {
				clientConn = &splitReadConn{Conn: srcProxy, readErr: io.ErrUnexpectedEOF}
				sender, receiver = destSvc, clientApp
			}

			// Paced so the transfer is still ARRIVING when the other
			// direction breaks, and so it outlasts a whole idle window —
			// a wall clock truncates this, an idle timer does not.
			const chunks, chunkSize = 12, 16 * 1024
			gap := testDrainIdle / 3
			payload := bytes.Repeat([]byte("G"), chunks*chunkSize)
			go func() {
				for i := 0; i < chunks; i++ {
					if _, err := sender.Write(payload[i*chunkSize : (i+1)*chunkSize]); err != nil {
						return
					}
					time.Sleep(gap)
				}
			}()

			got := make(chan int, 1)
			go func() {
				buf := make([]byte, len(payload))
				n, _ := io.ReadFull(receiver, buf)
				got <- n
			}()

			mocks := make(chan *models.Mock, 64)
			returned := make(chan struct{})
			go func() {
				_ = encodeGenericWithDrain(context.Background(), zap.NewNop(), nil,
					clientConn, destConn, mocks, models.OutgoingOptions{}, testDrainIdle)
				close(returned)
			}()

			select {
			case <-returned:
			case <-time.After(30 * time.Second):
				t.Fatal("encodeGeneric never returned")
			}
			// Model the caller: handleConnection closes the real sockets the
			// moment this returns. Without that close the test proves nothing,
			// because the copy goroutines would keep delivering on their own.
			//
			// It must be the REAL socket. SafeConn.Close is a no-op — that is
			// the whole reason keploy cannot wake a stranded io.Copy — so
			// closing the wrapper here would model nothing and quietly turn
			// this into a test that passes against a truncating build.
			closeUnderlying(t, srcProxy)
			closeUnderlying(t, dstProxy)

			select {
			case n := <-got:
				if n < len(payload) {
					t.Fatalf("the surviving direction delivered %d of %d bytes over %v. On this "+
						"path an early return or a fixed grace costs the user's bytes AND the "+
						"mocks the tee captured from them.",
						n, len(payload), time.Duration(chunks)*gap)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("reader never completed")
			}
		})
	}
}

// TestCaptureTeeWriterCountsWhatItDelivered is the positive control for
// the counter the drain polls. A tee that always reported zero would make
// every survivor look stalled from the first tick, silently restoring the
// wall clock — and no forwarding test would notice, because forwarding
// still works.
func TestCaptureTeeWriterCountsWhatItDelivered(t *testing.T) {
	var sink bytes.Buffer
	ch := make(chan []byte, 8)
	tee := &captureTeeWriter{dest: &sink, ch: ch, closeOnce: &sync.Once{}}

	if got := tee.delivered(); got != 0 {
		t.Fatalf("a fresh tee reports %d bytes delivered, want 0", got)
	}
	if _, err := tee.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := tee.Write([]byte(" world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := tee.delivered(), int64(len("hello world")); got != want {
		t.Fatalf("tee reports %d bytes delivered, want %d. The drain treats an unmoving "+
			"count as a stalled survivor, so a counter that does not advance abandons a "+
			"direction that is still delivering.", got, want)
	}
	if sink.String() != "hello world" {
		t.Fatalf("destination got %q", sink.String())
	}
}

// closeUnderlying closes the real socket inside a SafeConn, which is what
// handleConnection's deferred close reaches. SafeConn's own Close is a
// no-op, so calling it here would assert nothing.
func closeUnderlying(t *testing.T, c net.Conn) {
	t.Helper()
	sc, ok := c.(*proxyutil.SafeConn)
	if !ok {
		t.Fatalf("expected a *proxyutil.SafeConn, got %T — production wraps both conns, and a "+
			"raw conn here would make this test pass for the wrong reason", c)
	}
	_ = sc.Unwrap().Close()
}

// TestEncodeGeneric_ProductionEntryPointDrainsTheSurvivor drives
// encodeGeneric itself, on the production window, through the survivor
// path.
//
// The tests above call encodeGenericWithDrain so they can run at 400ms,
// which means the one line that wires proxyutil.RelayDrainIdle into the
// drain is executed but never checked. Passing a window too small to cover
// a real transfer abandons the survivor immediately — the original bug —
// and nothing else here would notice.
func TestEncodeGeneric_ProductionEntryPointDrainsTheSurvivor(t *testing.T) {
	clientApp, srcProxy := prodPair(t, true)
	destSvc, dstProxy := prodPair(t, false)

	broken := &splitReadConn{Conn: srcProxy, readErr: io.ErrUnexpectedEOF}

	const chunks, chunkSize = 10, 16 * 1024
	payload := bytes.Repeat([]byte("E"), chunks*chunkSize)
	go func() {
		for i := 0; i < chunks; i++ {
			if _, err := destSvc.Write(payload[i*chunkSize : (i+1)*chunkSize]); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Ends the survivor's copy so this returns without sitting out the
		// production idle window.
		if tc, ok := destSvc.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	got := make(chan int, 1)
	go func() {
		buf := make([]byte, len(payload))
		n, _ := io.ReadFull(clientApp, buf)
		got <- n
	}()

	mocks := make(chan *models.Mock, 64)
	returned := make(chan struct{})
	go func() {
		_ = encodeGeneric(context.Background(), zap.NewNop(), nil, broken, dstProxy,
			mocks, models.OutgoingOptions{})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(60 * time.Second):
		t.Fatal("encodeGeneric never returned")
	}
	closeUnderlying(t, srcProxy)
	closeUnderlying(t, dstProxy)

	select {
	case n := <-got:
		if n < len(payload) {
			t.Fatalf("the surviving direction delivered %d of %d bytes through encodeGeneric "+
				"on the production window. The drain is only as good as the window wired in "+
				"at that call site.", n, len(payload))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reader never completed")
	}
}

// TestEncodeGeneric_CleanThenResetReturnsPromptly guards generic's copy of
// the early return: when both copy results are already in hand there is
// nothing to drain, and waiting anyway costs a full idle window — 30s in
// production — on the case the code itself calls "very common".
func TestEncodeGeneric_CleanThenResetReturnsPromptly(t *testing.T) {
	clientApp, srcProxy := prodPair(t, true)
	destSvc, dstProxy := prodPair(t, false)

	go func() {
		_, _ = clientApp.Write([]byte("hello"))
		if tc, ok := clientApp.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		time.Sleep(20 * time.Millisecond)
		if tc, ok := destSvc.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = destSvc.Close()
	}()

	mocks := make(chan *models.Mock, 64)
	start := time.Now()
	returned := make(chan struct{})
	go func() {
		_ = encodeGenericWithDrain(context.Background(), zap.NewNop(), nil, srcProxy, dstProxy,
			mocks, models.OutgoingOptions{}, testDrainIdle)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("encodeGenericWithDrain never returned")
	}
	if elapsed := time.Since(start); elapsed > testDrainIdle*3/4 {
		t.Fatalf("returned after %v, most of one %v window, with both copy results already "+
			"available. In production that dead time is %v per clean-then-reset connection.",
			elapsed, testDrainIdle, proxyutil.RelayDrainIdle)
	}
}

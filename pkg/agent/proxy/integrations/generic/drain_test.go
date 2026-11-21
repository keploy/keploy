package generic

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/memconn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// memProdPair returns an in-memory connected pair where the proxy side is
// wrapped in SafeConn just as proxy.buildRecordSession wraps real sockets.
func memProdPair(withReader bool) (peer *memconn.Conn, proxySide net.Conn, rawProxy *memconn.Conn) {
	peer, rawProxy = memconn.Pair()
	if withReader {
		return peer, proxyutil.NewSafeConnWithReader(rawProxy, rawProxy, zap.NewNop()), rawProxy
	}
	return peer, proxyutil.NewSafeConn(rawProxy, zap.NewNop()), rawProxy
}

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
// shouldRecordViaSupervisor routes every parser implementing
// IntegrationsV2 to the supervisor, and every in-tree parser reports
// IsV2() == true. With the KEPLOY_NEW_RELAY rollback knob removed there is
// no way to reach this legacy path from the dispatcher at all; it is kept
// covered because the code is still reachable by direct construction.
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
			synctest.Test(t, func(t *testing.T) {
				clientApp, srcProxy, srcRaw := memProdPair(true)
				destSvc, dstProxy, dstRaw := memProdPair(false)
				defer func() {
					_ = clientApp.Close()
					_ = destSvc.Close()
					_ = srcRaw.Close()
					_ = dstRaw.Close()
				}()

				clientConn, destConn := srcProxy, dstProxy
				var sender, receiver *memconn.Conn
				if tc.breakDest {
					destConn = &splitReadConn{Conn: dstProxy, readErr: io.ErrUnexpectedEOF}
					sender, receiver = clientApp, destSvc
				} else {
					clientConn = &splitReadConn{Conn: srcProxy, readErr: io.ErrUnexpectedEOF}
					sender, receiver = destSvc, clientApp
				}

				const windows, chunksPerWindow, chunkSize = 6, 2, 16 * 1024
				payload := bytes.Repeat([]byte("G"), windows*chunksPerWindow*chunkSize)
				got := make([]byte, len(payload))

				ctx := syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop()))
				mocks := make(chan *models.Mock, 64)
				returned := make(chan struct{})
				go func() {
					_ = encodeGeneric(ctx, zap.NewNop(), nil, clientConn, destConn, mocks, models.OutgoingOptions{})
					close(returned)
				}()

				// Wait until the broken direction has failed and WaitForSurvivor
				// has armed its real proxyutil.RelayDrainIdle timer before the
				// multi-window transfer begins.
				synctest.Wait()
				select {
				case <-returned:
					closeUnderlying(t, srcProxy)
					closeUnderlying(t, dstProxy)
					t.Fatal("encodeGeneric returned immediately on the first copy error without draining the survivor")
				default:
				}

				step := proxyutil.RelayDrainIdle * 2 / 3
				rem := proxyutil.RelayDrainIdle - step
				for w := 0; w < windows; w++ {
					// Stay silent for 2/3 of the production idle window before
					// sending: this catches both a shortened initial window at
					// encodeGeneric and a shortened Reset duration (e.g. idle/2).
					time.Sleep(step)
					synctest.Wait()
					select {
					case <-returned:
						closeUnderlying(t, srcProxy)
						closeUnderlying(t, dstProxy)
						t.Fatalf("window %d of %d: the surviving direction was abandoned after %d of %d bytes "+
							"before its %v idle window expired",
							w+1, windows, w*chunksPerWindow*chunkSize, len(payload), proxyutil.RelayDrainIdle)
					default:
					}

					for c := 0; c < chunksPerWindow; c++ {
						off := (w*chunksPerWindow + c) * chunkSize
						if _, err := sender.Write(payload[off : off+chunkSize]); err != nil {
							t.Fatalf("window %d chunk %d write: %v", w+1, c+1, err)
						}
						if _, err := io.ReadFull(receiver, got[off:off+chunkSize]); err != nil {
							t.Fatalf("window %d chunk %d read: %v", w+1, c+1, err)
						}
					}
					synctest.Wait()

					// Advance across the idle timer tick; WaitForSurvivor must
					// observe the delivered bytes and reset for another window.
					time.Sleep(rem)
					synctest.Wait()
					select {
					case <-returned:
						closeUnderlying(t, srcProxy)
						closeUnderlying(t, dstProxy)
						t.Fatalf("window %d of %d: the surviving direction was abandoned after %d of %d bytes. "+
							"On this path an early return, a fixed grace, or watching the dead direction's "+
							"counter costs the user's bytes AND the mocks the tee captured from them.",
							w+1, windows, (w+1)*chunksPerWindow*chunkSize, len(payload))
					default:
					}
				}

				if !bytes.Equal(got, payload) {
					t.Fatal("surviving direction delivered corrupted bytes")
				}

				// Go silent without closing: the drain must stall after one
				// full RelayDrainIdle window with no new bytes.
				silenceStart := time.Now()
				select {
				case <-returned:
				case <-time.After(2 * proxyutil.RelayDrainIdle):
					t.Fatal("drain kept resetting after the surviving direction went silent")
				}
				if wait := time.Since(silenceStart); wait != proxyutil.RelayDrainIdle {
					t.Fatalf("drain stalled after %v of silence, want %v", wait, proxyutil.RelayDrainIdle)
				}

				// Model the caller: handleConnection closes the real sockets the
				// moment this returns.
				closeUnderlying(t, srcProxy)
				closeUnderlying(t, dstProxy)
				synctest.Wait()
			})
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
	tee := &captureTeeWriter{dest: &sink, stream: newCaptureStream(8, 1), fromClient: true}

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

// TestEncodeGeneric_ProductionEntryPointDrainsTheSurvivor drives encodeGeneric
// over multi-second chunk gaps followed by a clean survivor FIN.
//
// Each gap is proxyutil.RelayDrainIdle/2 (15s), which catches an encodeGeneric
// call site that wires a sub-second window instead of RelayDrainIdle, and the
// ending CloseWrite verifies that a survivor finishing cleanly returns
// immediately without sitting out a trailing idle window.
func TestEncodeGeneric_ProductionEntryPointDrainsTheSurvivor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientApp, srcProxy, srcRaw := memProdPair(true)
		destSvc, dstProxy, dstRaw := memProdPair(false)
		defer func() {
			_ = clientApp.Close()
			_ = destSvc.Close()
			_ = srcRaw.Close()
			_ = dstRaw.Close()
		}()

		broken := &splitReadConn{Conn: srcProxy, readErr: io.ErrUnexpectedEOF}

		ctx := syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop()))
		mocks := make(chan *models.Mock, 64)
		returned := make(chan struct{})
		start := time.Now()
		go func() {
			_ = encodeGeneric(ctx, zap.NewNop(), nil, broken, dstProxy, mocks, models.OutgoingOptions{})
			close(returned)
		}()

		synctest.Wait()
		select {
		case <-returned:
			closeUnderlying(t, srcProxy)
			closeUnderlying(t, dstProxy)
			t.Fatal("encodeGeneric returned immediately without draining the survivor")
		default:
		}

		const chunks, chunkSize = 10, 16 * 1024
		gap := proxyutil.RelayDrainIdle / 2
		payload := bytes.Repeat([]byte("E"), chunks*chunkSize)
		got := make([]byte, len(payload))
		for i := 0; i < chunks; i++ {
			time.Sleep(gap)
			synctest.Wait()
			select {
			case <-returned:
				closeUnderlying(t, srcProxy)
				closeUnderlying(t, dstProxy)
				t.Fatalf("the surviving direction delivered %d of %d bytes through encodeGeneric "+
					"after %v. The drain is only as good as the window wired in at that call site (%v).",
					i*chunkSize, len(payload), time.Since(start), proxyutil.RelayDrainIdle)
			default:
			}
			off := i * chunkSize
			if _, err := destSvc.Write(payload[off : off+chunkSize]); err != nil {
				t.Fatalf("chunk %d write: %v", i+1, err)
			}
			if _, err := io.ReadFull(clientApp, got[off:off+chunkSize]); err != nil {
				t.Fatalf("chunk %d read: %v", i+1, err)
			}
			synctest.Wait()
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("surviving direction delivered corrupted bytes")
		}

		// Ends the survivor's copy so encodeGeneric must return immediately
		// without sitting out the production idle window.
		finAt := time.Now()
		if err := destSvc.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		select {
		case <-returned:
		case <-time.After(2 * proxyutil.RelayDrainIdle):
			t.Fatal("encodeGeneric never returned after survivor half-closed")
		}
		if waitAfterFIN := time.Since(finAt); waitAfterFIN != 0 {
			t.Fatalf("encodeGeneric waited %v after survivor FIN, want 0", waitAfterFIN)
		}

		closeUnderlying(t, srcProxy)
		closeUnderlying(t, dstProxy)
		synctest.Wait()
	})
}

// TestEncodeGeneric_CleanThenResetReturnsPromptly guards generic's copy of
// the early return: when both copy results are already in hand there is
// nothing to drain, and waiting anyway costs a full idle window — 30s in
// production — on the case the code itself calls "very common".
//
// Running inside testing/synctest guarantees the first clean outcome is
// consumed before the peer resets and asserts exact zero fake elapsed time on
// return, catching both a missing early return and any fixed sleep on that
// branch without wall-clock flakiness.
func TestEncodeGeneric_CleanThenResetReturnsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientApp, srcProxy, srcRaw := memProdPair(true)
		destSvc, dstProxy, dstRaw := memProdPair(false)
		defer func() {
			_ = clientApp.Close()
			_ = destSvc.Close()
			_ = srcRaw.Close()
			_ = dstRaw.Close()
		}()

		ctx := syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop()))
		mocks := make(chan *models.Mock, 64)
		returned := make(chan struct{})
		go func() {
			_ = encodeGeneric(ctx, zap.NewNop(), nil, srcProxy, dstProxy, mocks, models.OutgoingOptions{})
			close(returned)
		}()

		if _, err := clientApp.Write([]byte("hello")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := clientApp.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		got, err := io.ReadAll(destSvc)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(got) != "hello" {
			t.Fatalf("dest got %q, want %q", got, "hello")
		}

		// Ensure the clean client->dest outcome has already been consumed by
		// encodeGeneric (i == 0) before the destination resets, so the reset
		// is guaranteed to land on i == 1.
		synctest.Wait()

		start := time.Now()
		destSvc.Reset()

		select {
		case <-returned:
		case <-time.After(2 * proxyutil.RelayDrainIdle):
			t.Fatal("encodeGeneric did not return after clean EOF followed by reset")
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("encodeGeneric waited %v after both copy results were in hand, want 0", elapsed)
		}

		closeUnderlying(t, srcProxy)
		closeUnderlying(t, dstProxy)
		synctest.Wait()
	})
}

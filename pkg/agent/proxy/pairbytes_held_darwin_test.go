//go:build darwin

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent/procsock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// On macOS 27 a process keploy's CLI starts without root is listed only its
// own sockets in macOS's TCP table, so the application's end of every
// connection the proxy holds is missing from it, and every wait and close was
// unknown. The application's socket is now read from the descriptor table of
// the process holding it (procsock), which a process of the same user may
// read: what its send buffer holds bounds what it had written, and an empty
// one proves everything it wrote was received.

// heldPair is darwinPair with the application's end hidden from the table
// (hideFromTable) and nothing proven listed: what an agent on macOS 27 reads.
func heldPair(t *testing.T) (p *Proxy, app, proxySide *net.TCPConn, reader *trackedConn) {
	t.Helper()
	p, app, proxySide, reader = darwinPair(t)
	hideFromTable(t, app.LocalAddr())
	p.paired.visible.Store(false)
	return p, app, proxySide, reader
}

// fillSendBuffer writes n bytes to app from a goroutine, more than the
// proxy's receive buffer takes while nothing reads it, and returns once the
// application's send buffer holds some: bytes still on their way.
func fillSendBuffer(t *testing.T, p *Proxy, app *net.TCPConn, n int) {
	t.Helper()
	go func() { _, _ = app.Write(make([]byte, n)) }()
	pcs, err := p.paired.upTo(1)
	if err != nil || len(pcs) != 1 {
		t.Fatalf("paired connections: %v, %v", pcs, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, found, err := pcs[0].appSocket(&heldSockets{})
		if err != nil || !found {
			t.Fatalf("the application's socket read from this process: found %v, %v", found, err)
		}
		if s.SndCC > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the application's send buffer never held a byte")
		}
		time.Sleep(time.Millisecond)
	}
}

// wantOneCloseUnknownLine checks that logs hold one line saying a close cannot
// tell what the application had written (closeUnknown), its error saying has.
func wantOneCloseUnknownLine(t *testing.T, logs *observer.ObservedLogs, has string) {
	t.Helper()
	lines := logs.FilterMessageSnippet("keploy cannot tell what the application had written").All()
	if len(lines) != 1 {
		t.Fatalf("%d lines say the close cannot tell what was written, want one: %+v", len(lines), logs.All())
	}
	if e, _ := lines[0].ContextMap()["error"].(string); !strings.Contains(e, has) {
		t.Errorf("the line's error is %q, want it to say %q", e, has)
	}
}

// failHeldProxySide makes every read of the proxy's side a held socket's wait
// or close makes (readHeldProxySide) fail, as one of a descriptor closed under
// it would.
func failHeldProxySide(t *testing.T) {
	t.Helper()
	was := readHeldProxySide
	readHeldProxySide = func(*net.TCPConn) proxySide { return proxySide{err: errors.New("bad file descriptor")} }
	t.Cleanup(func() { readHeldProxySide = was })
}

func waitHeld(p *Proxy, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return p.WaitPairedBytes(ctx, 1)
}

func TestAWaitReadsAnApplicationSocketTheTableHidesFromItsHolderOnDarwin(t *testing.T) {
	t.Run("everything received", func(t *testing.T) {
		p, app, proxySide, _ := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, 10*time.Second); err != nil {
			t.Fatalf("the wait ended with %v, want nil: the application's socket, read from this process, holds nothing on its way", err)
		}
		if !p.paired.held.Load() {
			t.Fatal("held is false once a wait read the application's socket from its holder")
		}
	})
	t.Run("the proxy's side cannot be read", func(t *testing.T) {
		p, app, proxySide, _ := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		failHeldProxySide(t)
		if err := waitHeld(p, 2*time.Second); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("the wait ended with %v, want unknown: what the proxy's side received cannot be read", err)
		}
	})
	t.Run("bytes in the application's send buffer", func(t *testing.T) {
		p, app, _, reader := heldPair(t)
		const n = 8 << 20
		fillSendBuffer(t, p, app, n)
		if err := waitHeld(p, time.Second); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the wait ended with %v, want it to wait for the bytes in the application's send buffer", err)
		}
		go func() { _, _ = io.ReadFull(reader, make([]byte, n)) }()
		if err := waitHeld(p, 20*time.Second); err != nil {
			t.Fatalf("the wait ended with %v once the proxy read them, want nil", err)
		}
	})
	t.Run("the application's FIN on its way", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		untilStateLeaves(t, proxySide, xnuTCPEstablished)
		if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		if err := waitHeld(p, 10*time.Second); err != nil {
			t.Fatalf("the wait ended with %v after the proxy's side took the application's FIN, want nil", err)
		}
	})
	t.Run("a socket no process holds, once one was read", func(t *testing.T) {
		p, app, proxySide, _ := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		pcs, _ := p.paired.upTo(1)
		pcs[0].holder.Store(nil)
		hideSockets(t, app.LocalAddr())
		if _, err := app.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := waitHeld(p, time.Second); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the wait ended with %v, want it to wait for the FIN of a socket every holder closed", err)
		}
	})
	t.Run("a bound past what was written", func(t *testing.T) {
		// The proxy's side received bytes the application had not yet
		// seen acknowledged as the wait read its send buffer: they are
		// counted twice, so only a later read of the buffer, empty, ends
		// the wait. Nothing happens on the connection to wake it, so the
		// wait's own looks are what read it again.
		p, app, proxySide, _ := heldPair(t)
		var reads atomic.Int32
		alterHeld(t, p, func(s *procsock.TCP) {
			if reads.Add(1) <= 3 {
				s.SndCC += 100
			}
		})
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, 5*time.Second); err != nil {
			t.Fatalf("the wait ended with %v after %d reads of the application's socket, want nil once a read of its own finds its send buffer empty", err, reads.Load())
		}
	})
	t.Run("the processes cannot be read, once one was read", func(t *testing.T) {
		p, app, proxySide, _ := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		pcs, _ := p.paired.upTo(1)
		pcs[0].holder.Store(nil)
		was := heldSocketsRead
		heldSocketsRead = func() (map[[2]netip.AddrPort]heldSocket, error) { return nil, errors.New("no processes") }
		t.Cleanup(func() { heldSocketsRead = was })
		if err := waitHeld(p, 5*time.Second); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("the wait ended with %v, want unknown: a socket whose holders cannot be read is not known gone", err)
		}
	})
	t.Run("a holder that closes its socket while the wait waits", func(t *testing.T) {
		// Its send buffer shown never empty, the bound read from it is
		// never reached. The holder closes it, and its FIN reaches the
		// proxy's side while the holder's read still shows it (a row's
		// seam); the next look, the wait's own with nothing else to wake
		// it, finds it gone, so its FIN, after everything it wrote, ends
		// the wait.
		p, app, proxySide, _ := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		pcs, _ := p.paired.upTo(1)
		shown, found, err := pcs[0].appSocket(&heldSockets{})
		if err != nil || !found {
			t.Fatalf("the application's socket read from this process: found %v, %v", found, err)
		}
		shown.SndCC += 100
		var keep atomic.Bool
		var reads atomic.Int32
		keep.Store(true)
		one := heldSocketRead
		heldSocketRead = func(pid int, fd int32) (procsock.TCP, bool) {
			reads.Add(1)
			if keep.Load() {
				return shown, true
			}
			return one(pid, fd)
		}
		t.Cleanup(func() { heldSocketRead = one })
		done := make(chan error, 1)
		go func() { done <- waitHeld(p, 5*time.Second) }()
		for deadline := time.Now().Add(5 * time.Second); reads.Load() < 3; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the wait read the application's socket %d times, want it to look again", reads.Load())
			}
		}
		_ = app.Close()
		untilStateLeaves(t, proxySide, xnuTCPEstablished)
		keep.Store(false)
		if err := <-done; err != nil {
			t.Fatalf("the wait ended with %v once the holder closed its socket, want nil: its FIN came after everything it wrote", err)
		}
	})
	t.Run("a reset socket", func(t *testing.T) {
		// A reset empties the send buffer unsent: it settles nothing.
		p, app, proxySide, _ := heldPair(t)
		var reads atomic.Int32
		alterHeld(t, p, func(s *procsock.TCP) {
			if reads.Add(1) == 1 {
				s.SndCC += 100
			} else {
				s.State = xnuTCPClosed
			}
		})
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, time.Second); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the wait ended with %v, want it to wait: a reset socket's empty send buffer says nothing of what was received", err)
		}
	})
	t.Run("the application's FIN not yet taken", func(t *testing.T) {
		// Its FIN sent and not acknowledged, after everything it wrote:
		// the wait wants it too, and ends once the proxy's side takes it.
		p, app, proxySide, _ := heldPair(t)
		alterHeld(t, p, func(s *procsock.TCP) { s.State = xnuTCPFinWait1 })
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, time.Second); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the wait ended with %v, want it to wait for the application's FIN", err)
		}
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		untilStateLeaves(t, proxySide, xnuTCPEstablished)
		if err := waitHeld(p, 5*time.Second); err != nil {
			t.Fatalf("the wait ended with %v once the proxy's side took the application's FIN, want nil", err)
		}
	})
	t.Run("a send buffer that never empties", func(t *testing.T) {
		// The agent could run for the whole budget (a clock the row
		// moves) and nothing moved: unknown, naming the connection.
		p, app, proxySide, _ := heldPair(t)
		p.paired.clk = &stall.ManualClock{Step: stall.OnTime}
		alterHeld(t, p, func(s *procsock.TCP) { s.SndCC += 100 })
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		err := waitHeld(p, 30*time.Second)
		if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), app.LocalAddr().String()+" to keploy's agent") {
			t.Fatalf("the wait ended with %v, want unknown naming %s", err, app.LocalAddr())
		}
	})
	t.Run("the processes cannot be read", func(t *testing.T) {
		p, app, proxySide, _ := heldPair(t)
		was := heldSocketsRead
		heldSocketsRead = func() (map[[2]netip.AddrPort]heldSocket, error) { return nil, errors.New("no processes") }
		t.Cleanup(func() { heldSocketsRead = was })
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, 10*time.Second); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("the wait ended with %v, want unknown", err)
		}
	})
	t.Run("the table lists neither end", func(t *testing.T) {
		p, app, proxySide, _ := heldPair(t)
		hideFromTable(t, proxySide.LocalAddr())
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, 10*time.Second); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("the wait ended with %v, want unknown: there is no proxy's side in the table to count from", err)
		}
	})
}

func TestACloseReadsAnApplicationSocketTheTableHidesFromItsHolderOnDarwin(t *testing.T) {
	t.Run("nothing on its way", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		pcs, _ := p.paired.upTo(1)
		untilSendBufferEmpty(t, pcs[0])
		_ = reader.Close()
		if err := p.PairedBytesErr(); err != nil {
			t.Fatalf("PairedBytesErr = %v after a close with nothing on its way, want nil", err)
		}
		w := pcs[0].wroteAtClose.Load()
		if w == nil || w.want != 5 || w.fin {
			t.Fatalf("the close's record = %+v, want 5 bytes written and no FIN", w)
		}
	})
	t.Run("bytes in the application's send buffer", func(t *testing.T) {
		// Unknown, and said in one line naming the connection, the bytes
		// and why they cannot be counted.
		p, app, proxySide, reader := heldPair(t)
		p.settleReads = true
		core, logs := observer.New(zapcore.WarnLevel)
		p.logger = zap.New(core)
		fillSendBuffer(t, p, app, 8<<20)
		_ = reader.Close()
		if err := p.PairedBytesErr(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("PairedBytesErr = %v after a close with bytes in the application's send buffer, want unknown", err)
		}
		// The close also charges the bytes it tosses unread (pairLost's
		// own warning): the line is the one that says why nothing counts.
		lines := logs.FilterMessageSnippet("keploy cannot tell what the application had written").All()
		if len(lines) != 1 {
			t.Fatalf("%d such warnings, want one: %+v", len(lines), logs.All())
		}
		f := lines[0].ContextMap()
		if want := app.LocalAddr().String() + " -> " + proxySide.LocalAddr().String(); f["connection"] != want {
			t.Errorf("the line names the connection %v, want %s", f["connection"], want)
		}
		if n, _ := f["unacknowledged_bytes"].(uint32); n == 0 {
			t.Errorf("the line counts %v bytes in the send buffer, want them", f["unacknowledged_bytes"])
		}
		if why, _ := f["why"].(string); !strings.Contains(why, "no sequence numbers") {
			t.Errorf("the line says why %q, want that the read of the socket has no sequence numbers", why)
		}
	})
	t.Run("the proxy's side cannot be read", func(t *testing.T) {
		p, app, proxySide, reader := heldPair(t)
		p.settleReads = true
		core, logs := observer.New(zapcore.WarnLevel)
		p.logger = zap.New(core)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		pcs, _ := p.paired.upTo(1)
		untilSendBufferEmpty(t, pcs[0])
		failHeldProxySide(t)
		_ = reader.Close()
		if err := p.PairedBytesErr(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("PairedBytesErr = %v, want unknown: what the proxy's side received cannot be read", err)
		}
		if w := pcs[0].wroteAtClose.Load(); w != nil {
			t.Fatalf("the close's record = %+v, want none from a side not read", w)
		}
		wantOneCloseUnknownLine(t, logs, "bad file descriptor")
	})
	t.Run("the processes cannot be read", func(t *testing.T) {
		p, _, _, reader := heldPair(t)
		p.settleReads = true
		core, logs := observer.New(zapcore.WarnLevel)
		p.logger = zap.New(core)
		was := heldSocketsRead
		heldSocketsRead = func() (map[[2]netip.AddrPort]heldSocket, error) { return nil, errors.New("no processes") }
		t.Cleanup(func() { heldSocketsRead = was })
		_ = reader.Close()
		if err := p.PairedBytesErr(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("PairedBytesErr = %v, want unknown", err)
		}
		wantOneCloseUnknownLine(t, logs, "no processes")
	})
	for _, fin := range []bool{false, true} {
		name := "a socket no process holds, once one was read, its FIN not taken"
		if fin {
			name = "a socket no process holds, once one was read, its FIN taken"
		}
		t.Run(name, func(t *testing.T) {
			// One no process the agent may read holds (another user's)
			// is gone only once the proxy's side took its FIN or reset:
			// until then bytes it wrote may still be on their way, and
			// the close is unknown, not silent.
			p, app, proxySide, reader := heldPair(t)
			p.settleReads = true
			core, logs := observer.New(zapcore.WarnLevel)
			p.logger = zap.New(core)
			if _, err := app.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			untilUnread(t, proxySide, 5)
			if err := waitHeld(p, 10*time.Second); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
				t.Fatal(err)
			}
			pcs, _ := p.paired.upTo(1)
			pcs[0].holder.Store(nil)
			hideSockets(t, app.LocalAddr())
			if fin {
				if err := app.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				untilStateLeaves(t, proxySide, xnuTCPEstablished)
			}
			_ = reader.Close()
			err := p.PairedBytesErr()
			if got := errors.Is(err, models.ErrProxyConnsUnsupported); got == fin {
				t.Fatalf("PairedBytesErr = %v, want unknown %v: a socket no process holds is gone only once the proxy's side took its FIN", err, !fin)
			}
			if w := pcs[0].wroteAtClose.Load(); w != nil {
				t.Fatalf("the close's record = %+v, want none for a socket no process holds", w)
			}
			if !fin {
				wantOneCloseUnknownLine(t, logs, "no process keploy's agent may read holds it")
			}
		})
	}
	t.Run("a socket its holder closed with bytes on their way", func(t *testing.T) {
		// Closed with bytes queued, it lives on in the kernel (FIN_WAIT_1)
		// until they and its FIN are acknowledged, and no process holds
		// it: the close is unknown, said in one line. Before, once one
		// socket had been read from its holder, the close made no record
		// and answered nothing while megabytes were still on their way.
		listed := readXNUTable
		p, app, proxySide, reader := heldPair(t)
		p.settleReads = true
		core, logs := observer.New(zapcore.WarnLevel)
		p.logger = zap.New(core)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if err := waitHeld(p, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		_ = proxySide.SetReadBuffer(8 << 10)
		if err := app.SetWriteBuffer(6 << 20); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := app.Write(make([]byte, 4<<20)); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the write did not fit the application's send buffer")
		}
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		a, _ := addrPortOf(app.LocalAddr())
		r, _ := addrPortOf(app.RemoteAddr())
		table, err := listed()
		if err != nil {
			t.Fatal(err)
		}
		if e, ok := findXNUPCB(table, a, r); !ok || !e.finPending() || e.sndCC == 0 {
			t.Fatalf("the application's socket, closed by its holder, is %+v (listed %v), want it with its FIN and bytes still unacknowledged", e, ok)
		}
		_ = reader.Close()
		if err := p.PairedBytesErr(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
			t.Fatalf("PairedBytesErr = %v after a close with bytes on their way from a socket its holder closed, want unknown", err)
		}
		wantOneCloseUnknownLine(t, logs, "may still be on their way")
	})
	t.Run("bytes in the send buffer once the proxy's side took the FIN", func(t *testing.T) {
		// Taken, not yet seen acknowledged by the holder: the FIN came
		// after every one of them, so the side's count holds them all,
		// and the close records it, not unknown.
		p, app, proxySide, reader := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		pcs, _ := p.paired.upTo(1)
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		untilStateLeaves(t, proxySide, xnuTCPEstablished)
		alterHeld(t, p, func(s *procsock.TCP) { s.SndCC = 5 })
		_ = reader.Close()
		if err := p.PairedBytesErr(); err != nil {
			t.Fatalf("PairedBytesErr = %v, want nil: the proxy's side took the FIN after every byte", err)
		}
		if w := pcs[0].wroteAtClose.Load(); w == nil || w.want != 6 {
			t.Fatalf("the close's record = %+v, want 5 bytes and the FIN received", w)
		}
	})
	t.Run("a reset socket", func(t *testing.T) {
		// Its send buffer emptied unsent: gone, and no record is made.
		p, app, proxySide, reader := heldPair(t)
		if _, err := app.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		untilUnread(t, proxySide, 5)
		if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
		pcs, _ := p.paired.upTo(1)
		untilSendBufferEmpty(t, pcs[0])
		alterHeld(t, p, func(s *procsock.TCP) { s.State = xnuTCPClosed })
		_ = reader.Close()
		if err := p.PairedBytesErr(); err != nil {
			t.Fatalf("PairedBytesErr = %v, want nil", err)
		}
		if w := pcs[0].wroteAtClose.Load(); w != nil {
			t.Fatalf("the close's record = %+v, want none for a reset socket", w)
		}
	})
	for name, state := range map[string]uint8{"FIN_WAIT_1": xnuTCPFinWait1, "CLOSING": xnuTCPClosing, "LAST_ACK": xnuTCPLastAck} {
		t.Run("its FIN not yet acknowledged, in "+name, func(t *testing.T) {
			// Sent after everything it wrote, not yet taken by the
			// proxy's side: on its way, and counted.
			p, app, proxySide, reader := heldPair(t)
			if _, err := app.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			untilUnread(t, proxySide, 5)
			if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
				t.Fatal(err)
			}
			pcs, _ := p.paired.upTo(1)
			untilSendBufferEmpty(t, pcs[0])
			alterHeld(t, p, func(s *procsock.TCP) { s.State = state })
			_ = reader.Close()
			if err := p.PairedBytesErr(); err != nil {
				t.Fatalf("PairedBytesErr = %v, want nil", err)
			}
			if w := pcs[0].wroteAtClose.Load(); w == nil || w.want != 6 || !w.fin {
				t.Fatalf("the close's record = %+v, want 5 bytes and the FIN written", w)
			}
		})
	}
}

// alterHeld changes, by f, the application socket of p's paired connection 1
// as every read of it from its holder shows it, the read of every process
// (heldSocketsRead) and of a holder found before (heldSocketRead), for the
// rest of the row.
func alterHeld(t *testing.T, p *Proxy, f func(*procsock.TCP)) {
	t.Helper()
	pcs, err := p.paired.upTo(1)
	if err != nil || len(pcs) != 1 {
		t.Fatalf("paired connections: %v, %v", pcs, err)
	}
	pc := pcs[0]
	mine := func(s procsock.TCP) bool { return sameEnd(s.Local, pc.app) && sameEnd(s.Remote, pc.proxy) }
	all, one := heldSocketsRead, heldSocketRead
	heldSocketsRead = func() (map[[2]netip.AddrPort]heldSocket, error) {
		by, err := all()
		for k, s := range by {
			if mine(s.sock) {
				f(&s.sock)
				by[k] = s
			}
		}
		return by, err
	}
	heldSocketRead = func(pid int, fd int32) (procsock.TCP, bool) {
		s, ok := one(pid, fd)
		if ok && mine(s) {
			f(&s)
		}
		return s, ok
	}
	t.Cleanup(func() { heldSocketsRead, heldSocketRead = all, one })
}

// A holder found before whose descriptor now holds another socket (closed
// and its number reused) is not read as the application's: every process is
// read again.
func TestAHoldersDescriptorHoldingAnotherSocketIsReadAgainOnDarwin(t *testing.T) {
	p, _, _, _ := heldPair(t)
	_, other, _, _ := darwinPair(t)
	pcs, _ := p.paired.upTo(1)
	pc := pcs[0]
	rc, err := other.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fd int32
	if err := rc.Control(func(f uintptr) { fd = int32(f) }); err != nil {
		t.Fatal(err)
	}
	pc.holder.Store(&sockOwner{pid: os.Getpid(), fd: fd})
	s, found, err := pc.appSocket(&heldSockets{})
	if err != nil || !found || !sameEnd(s.Local, pc.app) || !sameEnd(s.Remote, pc.proxy) {
		t.Fatalf("appSocket = %+v, %v, %v; want the application's socket, %s to %s", s, found, err, pc.app, pc.proxy)
	}
	if o := pc.holder.Load(); o == nil || o.fd == fd {
		t.Fatalf("the holder is %+v, want the application socket's own descriptor, not %d", o, fd)
	}
}

// A wait's budget spent with nothing moving names every connection it waited
// on: each orphan, and each application socket read from its holder.
func TestAWaitsBudgetNamesEachConnectionItWaitedOnOnDarwin(t *testing.T) {
	app1, app2 := netip.MustParseAddrPort("127.0.0.1:1001"), netip.MustParseAddrPort("127.0.0.1:1002")
	orphan := &pairTarget{pc: &pairedConn{app: app1}, orphan: xnuTCPFinWait2}
	held := &pairTarget{pc: &pairedConn{app: app2}, base: targetBase{held: true}}
	for _, c := range []struct {
		name      string
		pending   []*pairTarget
		has, hasn []string
	}{
		{"held only", []*pairTarget{held}, []string{app2.String() + " to keploy's agent"}, []string{"FIN short"}},
		{"orphan only", []*pairTarget{orphan}, []string{app1.String() + " (its orphan in"}, []string{"descriptor table"}},
		{"both", []*pairTarget{orphan, held}, []string{app1.String() + " (its orphan in", app2.String() + " to keploy's agent"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := orphansUnmoved(c.pending)
			if !errors.Is(err, models.ErrProxyConnsUnsupported) {
				t.Fatalf("orphansUnmoved = %v, want unknown", err)
			}
			for _, s := range c.has {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("orphansUnmoved = %q, want it to say %q", err, s)
				}
			}
			for _, s := range c.hasn {
				if strings.Contains(err.Error(), s) {
					t.Errorf("orphansUnmoved = %q, want it not to say %q", err, s)
				}
			}
		})
	}
}

func untilSendBufferEmpty(t *testing.T, pc *pairedConn) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, found, err := pc.appSocket(&heldSockets{})
		if err != nil || !found {
			t.Fatalf("the application's socket read from this process: found %v, %v", found, err)
		}
		if s.SndCC == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the application's send buffer still holds %d bytes", s.SndCC)
		}
		time.Sleep(time.Millisecond)
	}
}

// An application is another process: on macOS 27, under go test, the table
// lists this process only its own sockets, the shape keploy's agent reads; on
// macOS 26 it lists the child's. Either way the wait ends once the proxy has
// the bytes, not once the child closes its idle connection (an HTTP client's
// pool: 90 s for Go's), and not unknown.
func TestAChildProcesssWritesAreCountedOnDarwin(t *testing.T) {
	if addr := os.Getenv("KEPLOY_HELD_CHILD_ADDR"); addr != "" {
		// The child: it writes five bytes and keeps its connection open,
		// as an HTTP client's pool keeps an idle one.
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			os.Exit(3)
		}
		if _, err := c.Write([]byte("hello")); err != nil {
			os.Exit(4)
		}
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	p := &Proxy{logger: zap.NewNop()}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	child := exec.Command(os.Args[0], "-test.run", "^TestAChildProcesssWritesAreCountedOnDarwin$")
	child.Env = append(os.Environ(), "KEPLOY_HELD_CHILD_ADDR="+ln.Addr().String())
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	proxySide := c.(*net.TCPConn)
	t.Cleanup(func() { _ = proxySide.Close() })
	p.paired.add(proxySide, 1, 0)
	p.proveAppVisible(proxySide)
	reader := p.inflight.track(proxySide)
	t.Cleanup(func() { p.inflight.forget(reader) })
	if _, err := io.ReadFull(reader, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	t.Logf("macOS's TCP table lists the child's socket to this process: %v", p.paired.visible.Load())
	if err := waitHeld(p, 10*time.Second); err != nil {
		t.Fatalf("the wait on a child process's idle connection ended with %v, want nil", err)
	}
}

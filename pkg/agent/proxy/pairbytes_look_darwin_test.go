//go:build darwin

package proxy

import (
	"io"
	"net/netip"
	"slices"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// One look of the wait at a target (pairReached) reads what the proxy's
// side received (targetBase.received: on macOS, its next expected sequence
// number less the one after the application's first). The look notes that
// as the most seen and as progress for the watchdog, and the target is
// reached at exactly what was written: the count is not one past it.
// (TestOneLookOfTheWaitCountsWhatTheProxysSideReceived on Linux.)
func TestOneLookOfTheWaitCountsWhatTheProxysSideReceivedOnDarwin(t *testing.T) {
	p, app, _, reader := darwinPair(t)
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	targets, err := p.pairTargets(1)
	if err != nil || len(targets) != 1 || targets[0].want != 10 || targets[0].seen != 0 {
		t.Fatalf("pairTargets = %v, %v; want one of 10 bytes, none seen yet", targets, err)
	}
	target := targets[0]
	before := p.paired.progress.Load()
	reached, err := p.pairReached(target)
	if err != nil || !reached {
		t.Fatalf("pairReached = %v, %v with all 10 bytes received; want reached", reached, err)
	}
	if moved := p.paired.progress.Load() - before; target.seen != 10 || moved != 1 {
		t.Fatalf("the look saw %d bytes and moved the progress %d; want 10 and 1", target.seen, moved)
	}
}

// A loss a bytes wait finds once the proxy has closed its side short of what
// the application wrote (pairReached) is the bytes past what the side
// received, up to what the wait waited for, but for a FIN it counts. A test
// whose end was cut short owns the part written before its wait, and the test
// open now is charged for the part written after it. A cut end whose bytes
// all arrived owns none of it, and a FIN after a cut end's data is no byte of
// the open test's. The application's first sequence number is near the wrap,
// so every count crosses it. (TestALossAWaitFindsChargesEachTestWhoseBytesItHolds
// on Linux.)
func TestALossAWaitFindsChargesEachTestWhoseBytesItHoldsOnDarwin(t *testing.T) {
	const isn = uint32(0xfffffff8)
	for _, tc := range []struct {
		name       string
		cut        uint64 // the cut end's want
		received   uint32 // what the proxy's side had received at its close
		want       uint64 // what the wait waits for
		finCounted bool
		charged    uint64 // the open test's
		owners     []string
	}{
		{"the cut end's bytes and the open test's", 10, 4, 20, false, 1, []string{"test-1"}},
		{"the cut end's data, then the application's FIN", 10, 4, 11, true, 0, []string{"test-1"}},
		{"past a cut end whose bytes arrived", 3, 6, 20, false, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No orphan of the proxy's socket is listed: it is gone, and
			// tells nothing of the application's FIN.
			read := readXNUTable
			readXNUTable = func() ([]xnuPCB, error) { return nil, nil }
			t.Cleanup(func() { readXNUTable = read })
			p := &Proxy{logger: zap.NewNop()}
			pc := &pairedConn{app: netip.MustParseAddrPort("127.0.0.1:50912"), done: make(chan struct{})}
			close(pc.done)
			pc.atClose.Store(&proxySide{rcvNxt: isn + 1 + tc.received, state: xnuTCPEstablished})
			pc.cutShort("test-1", tc.cut)
			target := &pairTarget{pc: pc, base: targetBase{isn: isn}, want: tc.want, finCounted: tc.finCounted}
			reached, err := p.pairReached(target)
			if err != nil || !reached {
				t.Fatalf("pairReached = %v, %v; want settled as a loss", reached, err)
			}
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != tc.charged {
				t.Fatalf("app_bytes_lost charged to the open test = %d, want %d", l, tc.charged)
			}
			if got := ownersOf(p); !slices.Equal(got, tc.owners) {
				t.Fatalf("the loss is owned by %q, want %q", got, tc.owners)
			}
		})
	}
}

// The proxy's handler closes a connection with bytes unread after a test's
// end was cut short on it, and the application's FIN in. Nothing at a close
// on macOS knows the sequence number of the connection's first byte, so where
// the bytes tossed begin is not known (dataReceived): the test open now is
// charged, as it would be had it written them. A count that took the side's
// received bytes as none would hand the whole span to the cut end, and leave
// the open test uncharged for the bytes it wrote.
func TestBytesTossedAtACloseAfterACutEndChargeTheOpenTestOnDarwin(t *testing.T) {
	p, app, proxySide, reader := darwinPair(t)
	pc := pairedConnOf(t, p, 1)
	if _, err := app.Write([]byte("publish 1\n")); err != nil {
		t.Fatal(err)
	}
	pc.cutShort("test-1", 10)
	if _, err := app.Write([]byte("publish 2\n")); err != nil {
		t.Fatal(err)
	}
	if err := app.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	untilUnread(t, proxySide, 20)
	untilStateLeaves(t, proxySide, xnuTCPEstablished)
	_ = reader.Close()
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("%d app_bytes_lost charged to the open test at the close, want 1: it wrote 10 of the bytes tossed", l)
	}
}

//go:build linux

package proxy

import (
	"io"
	"net/netip"
	"slices"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// ownersOf takes p's owned losses and returns the test each names.
func ownersOf(p *Proxy) []string {
	var out []string
	for _, o := range p.TakeOwnedLosses() {
		out = append(out, o.Test)
	}
	return out
}

// A loss a bytes wait finds once the proxy has closed its side short of what
// the application wrote (pairReached) is the bytes past what the side
// received, up to what the wait waited for, but for a FIN it counts. A test
// whose end was cut short owns the part written before its wait, and the test
// open now is charged for the part written after it: bytes of both lost in one
// range charge both. A cut end whose bytes all arrived owns none of it, and a
// FIN after a cut end's data is no byte of the open test's.
func TestALossAWaitFindsChargesEachTestWhoseBytesItHolds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cut        uint64 // the cut end's want
		received   uint64 // what the proxy's side had received at its close
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
			was := lookupOrphan
			lookupOrphan = func(uint32, netip.AddrPort, netip.AddrPort) (diagTCP, error) { return diagTCP{}, errNoSocket }
			t.Cleanup(func() { lookupOrphan = was })
			p := &Proxy{logger: zap.NewNop()}
			pc := &pairedConn{app: netip.MustParseAddrPort("127.0.0.1:50912"), done: make(chan struct{})}
			close(pc.done)
			pc.atClose.Store(&proxySide{received: tc.received, state: tcpEstablished})
			pc.cutShort("test-1", tc.cut)
			reached, err := p.pairReached(&pairTarget{pc: pc, want: tc.want, finCounted: tc.finCounted})
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

// Bytes a cut end wrote that arrived after its wait looked, all still unread
// as the proxy closes the connection, are tossed by the close: they are that
// test's loss, and the test open now is charged only for bytes it wrote after
// them. The cut end's wait, which looked before the bytes arrived, marks the
// connection with what it waited for (pairedConn.cutShort).
func TestACutEndsBytesTossedUnreadAtTheCloseAreItsOwn(t *testing.T) {
	msg := []byte("Q\x00\x00\x00\x0eSELECT 1;\x00")
	for _, tc := range []struct {
		name      string
		alsoWrote bool // the open test writes on the connection too
		charged   uint64
	}{
		{"all of the cut end's bytes arrived", false, 0},
		{"the open test wrote after them", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
			pc := pairedConnOf(t, p, 1)
			if _, err := app.Write(msg); err != nil {
				t.Fatal(err)
			}
			pc.cutShort("test-1", uint64(len(msg)))
			n := len(msg)
			if tc.alsoWrote {
				if _, err := app.Write(msg); err != nil {
					t.Fatal(err)
				}
				n += len(msg)
			}
			unreadArrived(t, proxySide, n)
			_ = reader.Close()
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != tc.charged {
				t.Fatalf("app_bytes_lost charged to the open test = %d, want %d", l, tc.charged)
			}
			if got := ownersOf(p); !slices.Equal(got, []string{"test-1"}) {
				t.Fatalf("the loss is owned by %q, want test-1's", got)
			}
		})
	}
}

// A close that cannot read what the proxy's socket held unread cannot tell
// where the bytes lost begin: they may be any written up to what the
// application had written as the close began. A cut end owns its share, and
// the test open now, which wrote after it, is charged too.
func TestAnUnreadableCloseChargesTheOpenTestPastACutEnd(t *testing.T) {
	p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	pc := pairedConnOf(t, p, 1)
	if _, err := app.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	pc.cutShort("test-1", 1)
	if _, err := app.Write([]byte("publish 2\n")); err != nil {
		t.Fatal(err)
	}
	unreadArrived(t, proxySide, 10)
	// Its socket closed under the close: neither its side nor what it held
	// unread can be read.
	_ = proxySide.Close()
	_ = reader.Close()
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 1 {
		t.Fatalf("app_bytes_lost charged to the open test = %d, want 1", l)
	}
	if got := ownersOf(p); !slices.Equal(got, []string{"test-1"}) {
		t.Fatalf("the loss is owned by %q, want test-1's", got)
	}
}

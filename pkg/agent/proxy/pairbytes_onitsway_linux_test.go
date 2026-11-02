//go:build linux

package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/models"
)

// letThrough takes c's drop-all filter (dropAll) off its socket. A socket the
// proxy has closed is not c's any more: nothing is taken off it.
func letThrough(c *net.TCPConn) {
	if rc, err := c.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_DETACH_FILTER, 0) })
	}
}

// onItsWayEnds are how a plaintext connection's bytes on their way at the
// proxy's close end: data arrives (a loss: the proxy never read it), the
// application's FIN or its reset comes first (none), or keploy stops (none:
// nothing more of the run is judged).
var onItsWayEnds = []struct {
	name string
	want uint64
}{{"data", 1}, {"its FIN", 0}, {"its reset", 0}, {"keploy's stop", 0}}

// Plaintext the application wrote to a connection before the proxy began to
// close it, still on its way to the proxy's socket, was charged only by a
// wait that held the connection: one closed and dropped before a test's end
// took it was judged by none, a silent loss. The close now waits for those
// bytes by reading the socket: data that arrives is a loss whatever holds the
// connection; the application's reset with none before it is none, and so is
// keploy's own stop. Its FIN alone on its way is none, and not read at all.
// The proxy's socket drops every segment (dropAll) while the application
// writes and the close begins, so the bytes are on their way, and takes them
// again once the application has the close's FIN. On main the data case
// counts none.
func TestPlaintextOnItsWayAtAnUnheldCloseIsJudged(t *testing.T) {
	for _, tc := range onItsWayEnds {
		t.Run(tc.name, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			tracked := reader.(*trackedConn)
			tracked.beforeClose = func() { p.pairClosing(ctx, proxySide, tracked) }
			dropAll(t, proxySide)
			if tc.name == "its FIN" {
				// Only its FIN is on its way: the close's read of
				// its socket, made once the FIN is sent, surely
				// counts it, so the close reads nothing.
				if err := app.CloseWrite(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := app.Write([]byte("publish 2\n")); err != nil {
				t.Fatal(err)
			}
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				_ = reader.Close()
			}()
			if _, err := io.ReadAll(app); err != nil { // the close's FIN
				t.Fatal(err)
			}
			switch tc.name {
			case "its reset":
				if err := app.SetLinger(0); err != nil {
					t.Fatal(err)
				}
				_ = app.Close()
			case "keploy's stop":
				stop()
			}
			letThrough(proxySide)
			<-closed
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != tc.want {
				t.Fatalf("app_bytes_lost = %d once the proxy's close of a connection with bytes on their way ended, want %d", l, tc.want)
			}
		})
	}
}

// The same, unprivileged: a fixture on loopback cannot hold bytes on their way
// on demand without a socket filter, so the close's read of the application's
// socket is made to count 24 more than were written, and the application ends
// them once the close has sent its FIN. A read that fails (the socket closed
// under it) is a loss: whether the bytes were calls cannot be told. The loss
// is the bytes past what the proxy had received at the close, up to what the
// application had written by then: a test whose end was cut short owns it
// only when some of those were written before that end (pairedConn.lostBy),
// and the test open now is charged when some were written after it.
// Once the close is done, a bytes wait that holds the connection finds it
// reached. A connection whose close found nothing on its way is not read at
// all: its close does not wait. Nor is one whose close found only the
// application's FIN on its way, one byte short of a count that surely holds
// it (its socket read with the FIN sent): the bytes the application writes
// once it has the close's FIN are then refused, as after any close, and no
// loss. One byte short of a count that may not hold the FIN is read, and so is
// a count short by the FIN and a byte. Keploy's stop as the read returns, with
// data or with a failed read, charges nothing either.
func TestPlaintextOnItsWayAtTheProxysCloseIsReadThrough(t *testing.T) {
	cases := append([]struct {
		name string
		want uint64
	}{}, onItsWayEnds...)
	cases = append(cases, []struct {
		name string
		want uint64
	}{{"a failed read", 1}, {"data, after a cut end's bytes arrived", 1}, {"data, all written before a cut end", 0},
		{"data, written before a cut end and after it", 1},
		{"its FIN alone on its way", 0}, {"a byte on its way, its FIN not surely counted", 1}, {"a byte and its FIN on their way", 1},
		{"data, as keploy stops", 0}, {"a failed read, as keploy stops", 0}}...)
	// How many more than were written the close's read counts, and whether
	// its socket reads with the FIN sent: 24, open, unless named.
	type onItsWay struct {
		n       uint32
		finSent bool
	}
	counted := map[string]onItsWay{
		"its FIN alone on its way":                      {1, true},
		"a byte on its way, its FIN not surely counted": {1, false},
		"a byte and its FIN on their way":               {2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, app, proxySide, reader := heldPair(t, "tcp4", "127.0.0.1:0")
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			tracked := reader.(*trackedConn)
			tracked.beforeClose = func() { p.pairClosing(ctx, proxySide, tracked) }
			pc := pairedConnOf(t, p, 1)
			owner := ""
			if cut, ok := map[string]uint64{"data, after a cut end's bytes arrived": 1, "data, all written before a cut end": 25,
				"data, written before a cut end and after it": 13}[tc.name]; ok {
				// A byte written and read, then an end cut short that
				// waited for cut: 1, that byte; 25, that byte and the
				// 24 the close counts on their way; 13, that byte and
				// the first 12 of those, the rest written after the
				// cut end, in the test open now.
				if _, err := app.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
				pc.cutShort("test-1", cut)
				if cut > 1 {
					owner = "test-1"
				}
			}
			more, ok := counted[tc.name]
			if !ok {
				more = onItsWay{24, false}
			}
			was := lookupWritten
			lookupWritten = func(ds *diagSock, c *pairedConn, atClose bool) (diagTCP, error) {
				d, err := was(ds, c, atClose)
				if err == nil && d.info != nil {
					d.wqueue += more.n
					if more.finSent {
						info := *d.info
						info.State = tcpFinWait1
						d.state, d.info = tcpFinWait1, &info
					}
				}
				return d, err
			}
			t.Cleanup(func() { lookupWritten = was })
			if tc.name == "data, as keploy stops" || tc.name == "a failed read, as keploy stops" {
				// Keploy stops once the close's read has returned,
				// before the close judges what it read.
				read := plainOnItsWay
				plainOnItsWay = func(ctx context.Context, c *net.TCPConn) (bool, error) {
					data, err := read(ctx, c)
					stop()
					return data, err
				}
				t.Cleanup(func() { plainOnItsWay = read })
			}
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				_ = reader.Close()
			}()
			for pc.atClose.Load() == nil {
				runtime.Gosched()
			}
			lookupWritten = was
			if _, err := io.ReadAll(app); err != nil { // the close's FIN
				t.Fatal(err)
			}
			switch tc.name {
			case "data", "data, after a cut end's bytes arrived", "data, all written before a cut end", "data, written before a cut end and after it",
				"its FIN alone on its way", "a byte on its way, its FIN not surely counted", "a byte and its FIN on their way",
				"data, as keploy stops":
				if _, err := app.Write([]byte("publish 2\n")); err != nil {
					t.Fatal(err)
				}
			case "a failed read", "a failed read, as keploy stops":
				_ = proxySide.Close()
			case "its FIN":
				if err := app.CloseWrite(); err != nil {
					t.Fatal(err)
				}
			case "its reset":
				if err := app.SetLinger(0); err != nil {
					t.Fatal(err)
				}
				_ = app.Close()
			case "keploy's stop":
				stop()
			}
			<-closed
			if l := lossesOf(p, models.CaptureLossAppBytesLost); l != tc.want {
				t.Fatalf("app_bytes_lost = %d once the proxy's close of a connection with bytes on their way ended, want %d", l, tc.want)
			}
			var owners []string
			for _, o := range p.TakeOwnedLosses() {
				owners = append(owners, o.Test)
			}
			if fmt.Sprint(owners) != fmt.Sprint(map[bool][]string{true: {owner}}[owner != ""]) {
				t.Fatalf("the loss is owned by %v, want %q's alone (or none)", owners, owner)
			}
			if tc.name == "its FIN" || tc.name == "its reset" {
				if err := p.WaitPairedBytes(context.Background(), 1); err != nil {
					t.Fatalf("WaitPairedBytes: %v, want settled: the close read the connection to its end", err)
				}
			}
		})
	}
	t.Run("nothing on its way", func(t *testing.T) {
		p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
		// The application has its FIN and its socket stays open: a close
		// that read the socket would wait for good.
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(app); err != nil {
			t.Fatal(err)
		}
		if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
			t.Fatalf("app_bytes_lost = %d, want 0", l)
		}
	})
}

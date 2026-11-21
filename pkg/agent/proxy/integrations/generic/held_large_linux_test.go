//go:build linux

package generic

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// socketRcvBuf returns the socket's receive buffer size (SO_RCVBUF), or -1
// when conn is not a TCP socket or the query fails.
func socketRcvBuf(c net.Conn) int {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return -1
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return -1
	}
	n := -1
	_ = rc.Control(func(fd uintptr) { n, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF) })
	return n
}

// socketCanHold reports whether a loopback TCP socket whose receive buffer is
// sized to bufSize holds want bytes unread with its reader paused.
func socketCanHold(t *testing.T, bufSize, want int) (bool, int64, int) {
	t.Helper()
	app, proxySide := tcpPair(t)
	_ = proxySide.(*net.TCPConn).SetReadBuffer(bufSize)
	rb := socketRcvBuf(proxySide)
	if rb > 0 && rb < want {
		_ = app.Close()
		_ = proxySide.Close()
		return false, 0, rb
	}
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		_, _ = app.Write(make([]byte, want))
	}()
	var got, last int64
	stable := 0
	until(func() bool {
		got = pUtil.UnreadBytes(proxySide)
		if got >= int64(want) {
			return true
		}
		if got > 0 && got == last {
			stable++
			return stable > 100
		}
		stable, last = 0, got
		return false
	})
	_ = app.Close()
	_ = proxySide.Close()
	<-wrote
	return got >= int64(want), got, rb
}

// warmLoopbackPair streams 64 MiB fast over a default-sysctl loopback TCP pair
// so Linux TCP receive-buffer autotuning grows the buffer, then probes how
// many bytes the socket holds unread with its reader paused, and drains the
// probe before returning the clean pair.
func warmLoopbackPair(t *testing.T) (*net.TCPConn, net.Conn, int64) {
	t.Helper()
	app, proxySide := tcpPair(t)
	warm := make(chan error, 1)
	go func() {
		_, err := app.Write(make([]byte, 64<<20))
		warm <- err
	}()
	if _, err := io.CopyN(io.Discard, proxySide, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := <-warm; err != nil {
		t.Fatal(err)
	}
	probe := make(chan error, 1)
	go func() {
		_, err := app.Write(make([]byte, 16<<20))
		probe <- err
	}()
	var held, last int64
	stable := 0
	until(func() bool {
		held = pUtil.UnreadBytes(proxySide)
		if held > 0 && held == last {
			stable++
		} else {
			stable, last = 0, held
		}
		return stable > 50
	})
	if _, err := io.CopyN(io.Discard, proxySide, 16<<20); err != nil {
		t.Fatal(err)
	}
	if err := <-probe; err != nil {
		t.Fatal(err)
	}
	return app, proxySide, held
}

// readAnswerBeforeEnd reads want from app, failing at once if the parser ends
// first (done) or if want does not arrive within 10 s.
func readAnswerBeforeEnd(t *testing.T, app net.Conn, done <-chan error, logs *observer.ObservedLogs, proxySide net.Conn, want string) {
	t.Helper()
	type res struct {
		got []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
		got := make([]byte, len(want))
		_, err := io.ReadFull(app, got)
		ch <- res{got, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || string(r.got) != want {
			never := logs.FilterMessageSnippet("never paused").Len()
			t.Fatalf("answer %q, %v; want %q (never-paused warns: %d, socket unread: %d)", r.got, r.err, want, never, pUtil.UnreadBytes(proxySide))
		}
	case err := <-done:
		_ = app.Close()
		<-ch
		never := logs.FilterMessageSnippet("never paused").Len()
		t.Fatalf("the parser ended with %v before answering %q (never-paused warns: %d, socket unread: %d)", err, want, never, pUtil.UnreadBytes(proxySide))
	}
}

// assertHeldLargeRequestAnsweredWhole sends a request whose parser is held
// past its window after 1000 bytes with the whole rest already in proxySide,
// then sends a follow-up exchange ("AFTER" -> "AFTEROK"): if the read-on cap
// cut the request early, either the cut part misses or its unread tail misses
// on the next round, and a false "never paused" Warn is logged.
func assertHeldLargeRequestAnsweredWhole(t *testing.T, app, proxySide net.Conn, req []byte) {
	t.Helper()
	recorded := recordedAs("mock-big", req)
	recorded.TestModeInfo.IsFiltered = true
	recorded.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "BIGOK"}}}}
	store := eofStore{mocks: []*models.Mock{
		recordedExchange("mock-ping", "PING", "PONG"),
		recorded,
		recordedExchange("mock-after", "AFTER", "AFTEROK"),
	}}
	conn := &heldPast{Conn: proxySide, cut: 1000, rest: len(req) - 1000}
	core, logs := observer.New(zapcore.WarnLevel)
	done := make(chan error, 1)
	go func() {
		done <- decodeGeneric(context.Background(), zap.New(core), []byte("PING"), conn, nil, store, models.OutgoingOptions{})
	}()
	readAnswerBeforeEnd(t, app, done, logs, proxySide, "PONG")
	wrote := make(chan error, 1)
	go func() {
		_, err := app.Write(req)
		wrote <- err
	}()
	readAnswerBeforeEnd(t, app, done, logs, proxySide, "BIGOK")
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if _, err := app.Write([]byte("AFTER")); err != nil {
		t.Fatal(err)
	}
	readAnswerBeforeEnd(t, app, done, logs, proxySide, "AFTEROK")
	if never := logs.FilterMessageSnippet("never paused").Len(); never != 0 {
		t.Fatalf("a request whose application paused was warned as never pausing %d times", never)
	}
	_ = app.Close()
	awaitEnd(t, done, "a connection the application closed")
}

// A request whose parser is held past its window after 1000 bytes, with the
// whole rest already in the socket (the application paused and waits for its
// answer), is answered whole even when the unread rest exceeds readOnLimit
// (2 MiB), and no "never paused" Warn is logged: the read-on cap counts only
// bytes read beyond what the socket held at the parser's look. Exercised both
// with an explicit 4 MiB socket buffer (where rmem_max allows it) and on a
// warmed default-sysctl loopback socket whose buffer kernel autotuning grew.
func TestAHeldRequestOverTheReadOnLimitIsAnsweredWhole(t *testing.T) {
	t.Run("with a 4 MiB socket buffer", func(t *testing.T) {
		for _, size := range []int{1 << 20, 3 << 20} {
			t.Run(fmt.Sprintf("%d KiB", size>>10), func(t *testing.T) {
				if ok, got, rb := socketCanHold(t, 4<<20, size); !ok {
					t.Skipf("the socket held only %d of %d bytes (SO_RCVBUF %d)", got, size, rb)
				}
				req := varied(size, true)
				eachSwitch(t, func(t *testing.T) {
					app, proxySide := tcpPair(t)
					_ = proxySide.(*net.TCPConn).SetReadBuffer(4 << 20)
					assertHeldLargeRequestAnsweredWhole(t, app, proxySide, req)
				})
			})
		}
	})

	t.Run("on a warm autotuned socket", func(t *testing.T) {
		eachSwitch(t, func(t *testing.T) {
			var (
				app       *net.TCPConn
				proxySide net.Conn
				held      int64
				size      int
			)
			for i := range 2 {
				app, proxySide, held = warmLoopbackPair(t)
				size = min(int(held)-256<<10, 2<<20+256<<10)
				if size >= 2<<20+64<<10 {
					break
				}
				if i == 0 {
					_ = app.Close()
					_ = proxySide.Close()
				}
			}
			if size < 2<<20+64<<10 {
				t.Skipf("the kernel grew the loopback buffer to hold only %d bytes unread (SO_RCVBUF %d)", held, socketRcvBuf(proxySide))
			}
			req := varied(size, true)
			assertHeldLargeRequestAnsweredWhole(t, app, proxySide, req)
		})
	})
}

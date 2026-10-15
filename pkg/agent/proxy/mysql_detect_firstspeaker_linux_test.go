//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// These look at the socket itself (util.Waiting), which only Linux does.

// lateRead holds its connection's Read until told to go on, and tells when a
// read deadline is set on it.
type lateRead struct {
	net.Conn
	deadlineSet chan struct{}
	goOn        chan struct{}
}

func (c *lateRead) Read(b []byte) (int, error) {
	<-c.goOn
	return c.Conn.Read(b)
}

func (c *lateRead) SetReadDeadline(t time.Time) error {
	err := c.Conn.SetReadDeadline(t)
	if !t.IsZero() {
		select {
		case c.deadlineSet <- struct{}{}:
		default:
		}
	}
	return err
}

func (c *lateRead) NetConn() net.Conn { return c.Conn }

// A read a deadline stopped before it began reads nothing — Go fails it
// without looking at the socket — so the probe would take a client that had
// spoken for a silent one. stop looks at the socket itself.
func TestFirstReadStoppedBeforeItBeganStillSeesWhatArrived(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(app net.Conn)
		want func(b []byte, err error) bool
	}{
		{"bytes", func(app net.Conn) { _, _ = io.WriteString(app, "hello") },
			func(b []byte, err error) bool { return string(b) == "hello" }},
		{"the client's close", func(app net.Conn) { _ = app.Close() },
			func(b []byte, err error) bool { return len(b) == 0 && errors.Is(err, io.EOF) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, srcConn := clientPair(t)
			tc.send(app)
			arrived(t, srcConn)
			late := &lateRead{Conn: srcConn, deadlineSet: make(chan struct{}, 1), goOn: make(chan struct{})}
			r := startFirstRead(late, zap.NewNop())
			type result struct {
				b   []byte
				err error
			}
			res := make(chan result, 1)
			go func() {
				b, err := r.stop()
				res <- result{b, err}
			}()
			awaitEvent(t, late.deadlineSet, "stop setting its deadline")
			close(late.goOn) // the read begins only now, past its deadline
			got := awaitEvent(t, res, "stop returning")
			if !tc.want(got.b, got.err) {
				t.Fatalf("stop returned %q, %v", got.b, got.err)
			}
		})
	}
}

// A client that connected and left at once — a readiness check — to a
// destination that refuses is the client's doing, not a dial failure to
// report: the probe says client-read-error with no error, however the two
// ends' outcomes interleave.
func TestProbeAClientThatLeftIsNotADialFailure(t *testing.T) {
	addr := refusedAddr(t)
	p := testProxy(t)
	app, srcConn := clientPair(t)
	_ = app.Close()
	arrived(t, srcConn)

	probe, err := p.probeMysql(context.Background(), srcConn, addr, nil, uint32(mustPort(t, addr)),
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil || probe.Reason != "client-read-error" {
		t.Fatalf("reason %q, err %v; want client-read-error and no error", probe.Reason, err)
	}
}

// A destination that cannot be reached cannot serve the connection, whichever
// end spoke: the probe returns the dial's failure for the caller to report,
// rather than letting generic dispatch dial the same address again.
func TestProbeADialFailureEndsTheConnection(t *testing.T) {
	addr := refusedAddr(t)
	p := testProxy(t)
	app, srcConn := clientPair(t)
	if _, err := io.WriteString(app, "publish 1\n"); err != nil {
		t.Fatal(err)
	}

	probe, err := p.probeMysql(context.Background(), srcConn, addr, nil, uint32(mustPort(t, addr)),
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if !errors.Is(err, syscall.ECONNREFUSED) || probe.Reason != "upstream-dial-failed" {
		t.Fatalf("reason %q, err %v; want upstream-dial-failed with the refusal", probe.Reason, err)
	}
}

// refusedAddr is a loopback address nothing listens on: a socket bound to it
// and not listening keeps the port from anyone else and refuses connections.
func refusedAddr(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("127.0.0.1:%d", sa.(*syscall.SockaddrInet4).Port)
}

// arrived waits, with no clock, until what the peer sent, or its close, is
// queued on conn's socket.
func arrived(t *testing.T, conn net.Conn) {
	t.Helper()
	rc, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if err := rc.Read(func(fd uintptr) bool {
		_, _, err := unix.Recvfrom(int(fd), b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		return !errors.Is(err, unix.EAGAIN)
	}); err != nil {
		t.Fatal(err)
	}
}

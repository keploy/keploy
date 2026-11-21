//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// holdDial is the handshake hold's dial of a tree listener at 127.0.0.1:
// the connection the listener accepted (srv, the listener's end) and the
// hold's end as a pre-dial (pre). With once, the listener closes after that
// one accept, as a Gradle daemon's listener for its test worker does.
func holdDial(t *testing.T, once bool) (addr string, port uint32, pre *util.Predialed, srv net.Conn, l net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	addr = l.Addr().String()
	up, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	srv, err = l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if once {
		_ = l.Close()
	}
	return addr, uint32(l.Addr().(*net.TCPAddr).Port), util.NewPredialed(addr, connseq.NewUpstream(up)), srv, l
}

// treeCall runs serveTreeListener for a call from this process (the run's
// app) to addr in a goroutine, and gives the caller's end and its result.
func treeCall(t *testing.T, p *Proxy, addr string, port uint32, pre *util.Predialed, held treeListener) (net.Conn, chan error) {
	t.Helper()
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = cli.Close() })
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(os.Getpid()), AgentNsPid: uint32(os.Getpid())}
	done := make(chan error, 1)
	go func() {
		served, _, err := p.serveTreeListener(context.Background(), srv, dest, addr, "1", pre, held)
		if err == nil && !served {
			err = errors.New("not served: left to the mock path")
		}
		done <- err
	}()
	return cli, done
}

// exchange sends ping from the caller and pong from the listener's end of
// the call, each within a deadline, and says what went wrong, "" if nothing.
func exchange(cli, srv net.Conn) string {
	_ = cli.SetDeadline(time.Now().Add(10 * time.Second))
	_ = srv.SetDeadline(time.Now().Add(10 * time.Second))
	go func() { _, _ = cli.Write([]byte("ping")) }()
	got := make([]byte, 4)
	if _, err := io.ReadFull(srv, got); err != nil || string(got) != "ping" {
		return fmt.Sprintf("the listener's end read %q, %v; want the caller's ping", got, err)
	}
	go func() { _, _ = srv.Write([]byte("pong")) }()
	if _, err := io.ReadFull(cli, got); err != nil || string(got) != "pong" {
		return fmt.Sprintf("the caller read %q, %v; want the listener's pong", got, err)
	}
	return ""
}

// A call between the run's own processes whose handshake keploy held is
// served over the hold's dial, the connection the listener accepted for it,
// with the listener the hold found as it dialed. A listener that accepts one
// connection and closes (a Gradle daemon's for its test worker, which the
// daemon forks: the caller is not the listener's ancestor, so the call is
// relayed, not handed to the live handler) is gone once the call is served:
// asked then, keploy closed the call as one nothing accepted, and the
// listener's writes to the hold's dial failed ("Broken pipe").
func TestATreeCallIsServedOverTheHoldsDialToAListenerThatTookOnlyIt(t *testing.T) {
	recorded.forget()
	t.Cleanup(recorded.forget)
	p := selfCallProxy()
	addr, port, pre, srv, _ := holdDial(t, true)
	cli, done := treeCall(t, p, addr, port, pre, treeListener{owner: os.Getpid(), found: true})
	if msg := exchange(cli, srv); msg != "" {
		t.Fatal(msg)
	}
	_ = cli.Close()
	_ = srv.Close()
	if err := <-done; err != nil {
		t.Fatalf("the call was not served over the hold's dial: %v", err)
	}
}

// When the hold found no listener as it dialed (one came after its look)
// but its dial connected, the listener is asked again as the call is
// served: one still there is the call's, served over the hold's dial; one
// already gone leaves the call, open, to the mock path, which takes the
// hold's dial, never closed as one nothing accepted.
func TestATreeCallWhoseListenerCameAfterTheHoldsLookIsAskedAgain(t *testing.T) {
	recorded.forget()
	t.Cleanup(recorded.forget)
	t.Run("still listening", func(t *testing.T) {
		p := selfCallProxy()
		addr, port, pre, srv, _ := holdDial(t, false)
		cli, done := treeCall(t, p, addr, port, pre, treeListener{})
		if msg := exchange(cli, srv); msg != "" {
			t.Fatal(msg)
		}
		_ = cli.Close()
		_ = srv.Close()
		if err := <-done; err != nil {
			t.Fatalf("the call was not served: %v", err)
		}
	})
	t.Run("gone", func(t *testing.T) {
		p := selfCallProxy()
		addr, port, pre, _, _ := holdDial(t, true)
		cli, done := treeCall(t, p, addr, port, pre, treeListener{})
		err := <-done
		if err == nil || err.Error() != "not served: left to the mock path" {
			t.Fatalf("serveTreeListener = %v, want the call left to the mock path", err)
		}
		// Not closed by serveTreeListener: the caller's end still blocks.
		_ = cli.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, err := cli.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the caller's read ended with %v: the call was closed", err)
		}
	})
}

// A pre-dial of another address (one an earlier connection from the same
// reused port left) and the listener found with it are not this call's: the
// listener is asked for the call's own address, and a call nothing listens
// at is closed, as its connect would have been refused, with no dial.
func TestATreeCallIgnoresAPreDialOfAnotherAddress(t *testing.T) {
	recorded.forget()
	t.Cleanup(recorded.forget)
	p := selfCallProxy()
	_, _, pre, _, _ := holdDial(t, false)
	gone, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := gone.Addr().String()
	port := uint32(gone.Addr().(*net.TCPAddr).Port)
	_ = gone.Close()
	_, done := treeCall(t, p, addr, port, pre, treeListener{owner: os.Getpid(), found: true})
	if err := <-done; err != nil {
		t.Fatalf("serveTreeListener = %v, want the call closed as refused (served, no error, no dial)", err)
	}
}

// The hold looks for a mock run's local listener as it dials (dialHeld), and
// keeps what it found beside the connection: a listener that closes after
// accepting the hold's dial is the call's all the same.
func TestTheHandshakeHoldKeepsTheListenerItFoundAsItDialed(t *testing.T) {
	recorded.forget()
	t.Cleanup(recorded.forget)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { // takes one connection, then stops listening
		if c, err := l.Accept(); err == nil {
			t.Cleanup(func() { _ = c.Close() })
		}
		_ = l.Close()
	}()
	p := selfCallProxy()
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	decide := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, l.Addr().String())})
	d := decide(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Accept || d.Keep == nil {
		t.Fatalf("decision %+v, want accept keeping the connection", d)
	}
	d.Keep()
	pre, held := p.predials.takeWith(appEnd)
	t.Cleanup(pre.CloseIfUnused)
	if pre == nil || held != (treeListener{owner: os.Getpid(), found: true}) {
		t.Fatalf("kept %v with listener %+v, want the dial with this process's listener", pre, held)
	}
}

// Only a mock run's local destination is looked for (treeListenerAt): a
// recording that is not a mock run, or a destination that is not this
// machine's, costs no look at the listeners, even with one on its port.
func TestTheHandshakeHoldLooksOnlyAtAMockRunsLocalListeners(t *testing.T) {
	l, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	local := &agent.NetworkAddress{Version: 4, Port: port, IPv4Addr: 0x7f000001}
	if got := selfCallProxy().treeListenerAt(local); got != (treeListener{owner: os.Getpid(), found: true}) {
		t.Fatalf("a mock run's local listener = %+v, want this process's", got)
	}
	notMock := &Proxy{appPID: uint32(os.Getpid()), logger: zap.NewNop()}
	if got := notMock.treeListenerAt(local); got != (treeListener{}) {
		t.Fatalf("a recording that is not a mock run looked: %+v", got)
	}
	away := &agent.NetworkAddress{Version: 4, Port: port, IPv4Addr: 0xc0000201} // 192.0.2.1, TEST-NET-1
	if got := selfCallProxy().treeListenerAt(away); got != (treeListener{}) {
		t.Fatalf("a destination that is not this machine's was looked for: %+v", got)
	}
}

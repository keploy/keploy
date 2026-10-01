//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// unansweredAddr is an address whose SYNs are dropped: a listener whose
// accept queue is full, so a dial to it stays connecting.
func unansweredAddr(t *testing.T) string {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(sa.(*unix.SockaddrInet4).Port)).String()
	for i := 0; i < 4; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			break // the queue is full
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	return addr
}

type goneAfterFirstLookup struct {
	fakeHandshakeDest
	calls atomic.Int32
}

func (g *goneAfterFirstLookup) GetForHandshake(context.Context, netip.AddrPort, netip.AddrPort) (*agent.NetworkAddress, error) {
	if g.calls.Add(1) == 1 {
		return g.dest, nil
	}
	return nil, agent.ErrConnectingSocketGone
}

type flakyLookup struct {
	fakeHandshakeDest
	calls atomic.Int32
}

func (f *flakyLookup) GetForHandshake(context.Context, netip.AddrPort, netip.AddrPort) (*agent.NetworkAddress, error) {
	if f.calls.Add(1) == 1 {
		return f.dest, nil
	}
	return nil, errors.New("sock_diag: resource temporarily unavailable")
}

// TestDecideHandshakeKeepsDiallingThroughALookupFailure: a recheck that
// fails for any reason other than the socket being gone does not end a dial
// the application still waits for.
func TestDecideHandshakeKeepsDiallingThroughALookupFailure(t *testing.T) {
	prev := handshakeRecheck
	handshakeRecheck = 50 * time.Millisecond
	t.Cleanup(func() { handshakeRecheck = prev })

	p := &Proxy{logger: zap.NewNop()}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	lookup := &flakyLookup{fakeHandshakeDest: fakeHandshakeDest{dest: ipv4Dest(t, unansweredAddr(t))}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(400*time.Millisecond, cancel) // the proxy stopping
	d := p.decideHandshake(lookup)(ctx, appEnd, proxyEnd)
	if lookup.calls.Load() < 3 {
		t.Fatalf("rechecked %d times in 400ms; the test proves nothing", lookup.calls.Load())
	}
	// It ran until its context ended, not until the first failed recheck.
	if d.Outcome != synhold.Accept {
		t.Fatalf("decision %+v; a cancelled dial completes the handshake", d)
	}
}

// TestDecideHandshakeStopsWhenTheApplicationGivesUp: an application whose
// connect timeout is shorter than the destination's silence closes its
// socket; the dial for it — and the SYN held for it — must end then, not
// after the kernel's SYN retries run out.
func TestDecideHandshakeStopsWhenTheApplicationGivesUp(t *testing.T) {
	prev := handshakeRecheck
	handshakeRecheck = 50 * time.Millisecond
	t.Cleanup(func() { handshakeRecheck = prev })

	p := &Proxy{logger: zap.NewNop()}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	lookup := &goneAfterFirstLookup{fakeHandshakeDest: fakeHandshakeDest{dest: ipv4Dest(t, unansweredAddr(t))}}
	start := time.Now()
	d := p.decideHandshake(lookup)(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Drop || d.Keep != nil {
		t.Fatalf("decision %+v, want drop with nothing kept", d)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the abandoned dial ran %v", took)
	}
}

// TestDecideHandshakeClosesWhatTheApplicationGaveUpOn: an application whose
// connect timed out while the destination was answering leaves nothing open
// at the destination.
func TestDecideHandshakeClosesWhatTheApplicationGaveUpOn(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	closed := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = c.Read(make([]byte, 1)) // returns when the proxy closes it
		close(closed)
	}()
	p := &Proxy{logger: zap.NewNop()}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	lookup := &goneAfterFirstLookup{fakeHandshakeDest: fakeHandshakeDest{dest: ipv4Dest(t, l.Addr().String())}}
	d := p.decideHandshake(lookup)(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Drop || d.Keep != nil {
		t.Fatalf("decision %+v, want drop with nothing kept", d)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the destination still holds a connection the application gave up on")
	}
}

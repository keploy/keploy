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
	"go.keploy.io/server/v3/pkg/agent/containerkey"
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

// A mock record whose application gives up connecting to a dependency that
// never answers notes the dependency unreached, with no answer.
func TestAGivenUpConnectToADependencyOfAMockRecordIsNotedUnreached(t *testing.T) {
	prev := handshakeRecheck
	handshakeRecheck = 50 * time.Millisecond
	t.Cleanup(func() { handshakeRecheck = prev })
	t.Setenv("KEPLOY_PROVEN", "1")

	p := &Proxy{logger: zap.NewNop(), mockMode: true}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	s := &unreachedSink{}
	p.SetTopologySink(s)
	silent := unansweredAddr(t)
	dest := ipv4Dest(t, silent)
	dest.Instance = 5
	d := p.decideHandshake(&goneAfterFirstLookup{fakeHandshakeDest: fakeHandshakeDest{dest: dest}})(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Drop {
		t.Fatalf("decision %+v, want drop", d)
	}
	want := models.UnreachedRow{Addr: silent, Answer: models.UnreachedNoAnswer, Instance: 5}
	if got := s.got(); len(got) != 1 || got[0] != want {
		t.Fatalf("noted %+v; want [%+v]", got, want)
	}
}

// A mock record whose application gives up connecting to a container the
// index is still placing notes no unreached dependency: whether it is a
// mock.real destination is not known at the handshake, and once it is
// placed as one the failure is its test's REAL row's.
func TestAGivenUpConnectToAContainerStillBeingPlacedIsNoUnreachedDependency(t *testing.T) {
	prev := handshakeRecheck
	handshakeRecheck = 50 * time.Millisecond
	t.Cleanup(func() { handshakeRecheck = prev })
	liveInstances(t, 4)
	silent := unansweredAddr(t)
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })

	p := realProxy(t, []string{"container:redis"}, nil)
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	p.mockMode = true
	sink := &unreachedSink{}
	p.SetTopologySink(sink)
	f := placingContainer{addr: silent, c: containerkey.Container{ID: "r", Image: "redis:7", Port: 6379},
		placed: make(chan struct{}), awaiting: make(chan struct{}, 1)}
	k := containerkey.NewKeyer(f)
	session, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	k.Bind(session)
	k.Watch(session, zap.NewNop())
	containerkey.Default = k
	resolved := make(chan struct{}, 1)
	p.realDialResolved = func() { resolved <- struct{}{} }
	p.realTracker.BeginTestOf(4)
	dest := ipv4Dest(t, silent)
	dest.Instance = 4

	d := p.decideHandshake(&goneAfterFirstLookup{fakeHandshakeDest: fakeHandshakeDest{dest: dest}})(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Drop {
		t.Fatalf("decision %+v, want drop", d)
	}
	awaitEvent(t, f.awaiting, "the placing being waited for")
	close(f.placed)
	awaitEvent(t, resolved, "the placing ending")
	if got := sink.got(); len(got) != 0 {
		t.Fatalf("noted %+v unreached", got)
	}
	if got, want := p.realTracker.EndTestOf(4).Uses, (models.RealUse{Dest: "container:redis", Failed: 1}); len(got) != 1 || got[0] != want {
		t.Fatalf("the test's REAL row: %+v, want [%+v]", got, want)
	}
}

// An application that gives up connecting to a mock.real destination fails
// that connect as it would without keploy: with two test workers' tests open,
// the failure is in the REAL row of the worker whose instance the kernel
// stamped the connect with, and in no other.
func TestAGivenUpConnectToARealDestinationIsItsOwnWorkersFailure(t *testing.T) {
	prev := handshakeRecheck
	handshakeRecheck = 50 * time.Millisecond
	t.Cleanup(func() { handshakeRecheck = prev })

	silent := unansweredAddr(t)
	p := realProxy(t, []string{silent}, nil)
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	twoWorkers(t, p)
	dest := ipv4Dest(t, silent)
	dest.Instance, dest.KernelPid = workerFour.Instance, workerFour.Pid
	d := p.decideHandshake(&goneAfterFirstLookup{fakeHandshakeDest: fakeHandshakeDest{dest: dest}})(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Drop || d.Keep != nil {
		t.Fatalf("decision %+v, want drop with nothing kept", d)
	}
	if got, want := endWorkers(t, p).Uses, []models.RealUse{{Dest: silent, Failed: 1}}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("worker 4's REAL row: %+v, want %+v", got, want)
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

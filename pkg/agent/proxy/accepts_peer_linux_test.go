package proxy

import (
	"context"
	"net"
	"sync"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
)

// peerCounter is a fakeCounter that pairs by the accept-time peer address
// (agent.AcceptedPeerPairer), as the macOS hooks do.
type peerCounter struct {
	fakeCounter
	pmu   sync.Mutex
	peers []*net.TCPAddr
	plain int
}

func (f *peerCounter) ProxyConnPair(rc syscall.RawConn) (agent.ProxyConnPairing, error) {
	f.pmu.Lock()
	f.plain++
	f.pmu.Unlock()
	return f.fakeCounter.ProxyConnPair(rc)
}

func (f *peerCounter) ProxyConnPairPeer(rc syscall.RawConn, peer *net.TCPAddr) (agent.ProxyConnPairing, error) {
	f.pmu.Lock()
	f.peers = append(f.peers, peer)
	f.pmu.Unlock()
	return f.fakeCounter.ProxyConnPair(rc)
}

// Hooks that pair by the peer are handed the address accept() returned for
// every connection, and are never asked ProxyConnPair: that is what lets them
// pair a connection reset before the accept, whose socket no longer answers
// getpeername.
func TestHooksThatPairByThePeerGetTheAcceptedAddress(t *testing.T) {
	f := &peerCounter{}
	p, _ := startListening(t, f)
	c, err := net.Dial("tcp", p.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	want := c.LocalAddr().(*net.TCPAddr)
	// The accept loop counts the connection before it hands it on: wait
	// for the count to cover it (the canary's, then this one). No deadline
	// but the test binary's -timeout.
	if err := p.WaitAccepted(context.Background(), 2); err != nil {
		t.Fatalf("WaitAccepted(2): %v", err)
	}
	f.pmu.Lock()
	defer f.pmu.Unlock()
	if f.plain != 0 {
		t.Fatalf("hooks that pair by the peer were asked ProxyConnPair %d times", f.plain)
	}
	for _, got := range f.peers {
		if got != nil && got.Port == want.Port && got.IP.Equal(want.IP) {
			return
		}
	}
	t.Fatalf("the hooks were handed peers %v, none the client's %v", f.peers, want)
}

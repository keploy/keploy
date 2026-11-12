package supervisor

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// The first error the relay tells of the client's socket is kept; a nil one,
// and every later one, is not.
func TestTheClientsEndErrorIsTheFirstTold(t *testing.T) {
	t.Parallel()
	var none *Session
	none.MarkClientEndError(syscall.EPIPE)
	if err := none.ClientEndError(); err != nil {
		t.Fatalf("a nil session says %v", err)
	}
	s := &Session{}
	if err := s.ClientEndError(); err != nil {
		t.Fatalf("an unmarked session says %v, want nil", err)
	}
	s.MarkClientEndError(nil)
	if err := s.ClientEndError(); err != nil {
		t.Fatalf("after a nil mark: %v, want nil", err)
	}
	s.MarkClientEndError(errors.New("use of closed network connection"))
	s.MarkClientEndError(syscall.Errno(10054)) // WSAECONNRESET
	if err := s.ClientEndError(); err != nil {
		t.Fatalf("after an error with no errno and an errno past 255 (Windows' WSAECONNRESET): %v, want nil", err)
	}
	s.MarkPeerEnded(fakeconn.FromClient, false)
	s.MarkEndedWithConnection(fakeconn.FromDest)
	s.MarkClientEndError(&net.OpError{Op: "write", Err: os.NewSyscallError("write", syscall.EPIPE)})
	s.MarkClientEndError(syscall.ECONNRESET)
	if err := s.ClientEndError(); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("got %v, want the first, EPIPE, out of the write's error", err)
	}
	s.MarkClientEndError(syscall.Errno(255))
	if s.PeerEnded(fakeconn.FromClient) != PeerClosed || s.PeerEnded(fakeconn.FromDest) != PeerOpen || !s.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("the errno kept changed how the connection's sides ended")
	}
	f := &Session{}
	f.MarkClientEndError(syscall.Errno(255))
	f.MarkPeerEnded(fakeconn.FromDest, true)
	if err := f.ClientEndError(); !errors.Is(err, syscall.Errno(255)) || f.PeerEnded(fakeconn.FromDest) != PeerReset {
		t.Fatalf("got %v and the destination's end %v, want errno 255 and its reset", err, f.PeerEnded(fakeconn.FromDest))
	}
}

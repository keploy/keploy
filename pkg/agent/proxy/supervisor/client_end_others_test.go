//go:build !windows

package supervisor

import (
	"errors"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// Off Windows the session keeps the errno itself, up to 255, the byte's
// whole range, without changing how the connection's sides ended; a Windows
// socket error, past 255, is not kept.
func TestOffWindowsTheClientsEndErrnoIsKeptAsItIs(t *testing.T) {
	t.Parallel()
	s := &Session{}
	s.MarkClientEndError(winWSAEConnReset)
	if err := s.ClientEndError(); err != nil {
		t.Fatalf("after Windows' WSAECONNRESET, past 255: %v, want nil", err)
	}
	s.MarkPeerEnded(fakeconn.FromClient, false)
	s.MarkEndedWithConnection(fakeconn.FromDest)
	s.MarkClientEndError(syscall.Errno(255))
	if err := s.ClientEndError(); !errors.Is(err, syscall.Errno(255)) {
		t.Fatalf("got %v, want errno 255", err)
	}
	if s.PeerEnded(fakeconn.FromClient) != PeerClosed || s.PeerEnded(fakeconn.FromDest) != PeerOpen || !s.EndedWithConnection(fakeconn.FromDest) {
		t.Fatal("the errno kept changed how the connection's sides ended")
	}
	f := &Session{}
	f.MarkClientEndError(syscall.ETIMEDOUT)
	f.MarkPeerEnded(fakeconn.FromDest, true)
	if err := f.ClientEndError(); !errors.Is(err, syscall.ETIMEDOUT) || f.PeerEnded(fakeconn.FromDest) != PeerReset {
		t.Fatalf("got %v and the destination's end %v, want ETIMEDOUT, an errno no Windows code keeps, and its reset", err, f.PeerEnded(fakeconn.FromDest))
	}
}

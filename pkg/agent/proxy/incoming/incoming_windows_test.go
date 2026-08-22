//go:build windows

package proxy

import (
	"context"
	"errors"
	"net"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// stubHooks records whether the source-port lookup was consulted and answers
// with whatever the test set up.
type stubHooks struct {
	getCalled    bool
	deleteCalled bool
	addr         *agent.NetworkAddress
	err          error
}

func (s *stubHooks) Get(_ context.Context, _ uint16) (*agent.NetworkAddress, error) {
	s.getCalled = true
	return s.addr, s.err
}

func (s *stubHooks) Delete(_ context.Context, _ uint16) error {
	s.deleteCalled = true
	return nil
}

func (s *stubHooks) Load(_ context.Context, _ agent.HookCfg, _ config.Agent) error { return nil }

func (s *stubHooks) WatchBindEvents(_ context.Context) (<-chan models.IngressEvent, error) {
	return nil, nil
}

// fakeConn is a net.Conn that only has to report a remote address.
type fakeConn struct {
	net.Conn
	remote net.Addr
}

func (f *fakeConn) RemoteAddr() net.Addr { return f.remote }

func remoteFrom(t *testing.T, addr string) net.Conn {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}
	return &fakeConn{remote: a}
}

// The WinDivert backend leaves the application on its advertised port and has
// the kernel redirect inbound packets, so the real destination is only
// recoverable per connection. StartIngress signals that with a zero port, and
// the lookup must still happen — this is the pre-existing behaviour and it must
// not change.
func TestGetActualDestinationLooksUpWhenTargetUnknown(t *testing.T) {
	hooks := &stubHooks{addr: &agent.NetworkAddress{Version: 4, IPv4Addr: 0x0A000005, Port: 9090}}
	pm := &IngressProxyManager{logger: zap.NewNop(), hooks: hooks}

	got := pm.getActualDestination(context.Background(), remoteFrom(t, "127.0.0.1:51000"), "127.0.0.1:0", zap.NewNop())

	if !hooks.getCalled {
		t.Fatal("the source-port lookup was skipped for an unknown target; the WinDivert backend depends on it")
	}
	if want := "10.0.0.5:9090"; got != want {
		t.Fatalf("getActualDestination = %q, want %q", got, want)
	}
	if !hooks.deleteCalled {
		t.Fatal("a consumed destination entry was not released")
	}
}

// With an unknown target and no recorded destination, the fallback stands.
func TestGetActualDestinationFallsBackWhenLookupFails(t *testing.T) {
	hooks := &stubHooks{err: errors.New("no destination recorded")}
	pm := &IngressProxyManager{logger: zap.NewNop(), hooks: hooks}

	got := pm.getActualDestination(context.Background(), remoteFrom(t, "127.0.0.1:51000"), "127.0.0.1:0", zap.NewNop())

	if want := "127.0.0.1:0"; got != want {
		t.Fatalf("getActualDestination = %q, want the fallback %q", got, want)
	}
}

// The unprivileged backend MOVES the application's listener and hands us its
// port, so the target is already known. The source-port lookup must be skipped:
// that map holds the application's OUTGOING connections, and an inbound client
// assigned a port matching a stale entry would otherwise be forwarded to that
// external host and would consume a mapping the real outgoing connection still
// needed.
func TestGetActualDestinationSkipsLookupWhenTargetKnown(t *testing.T) {
	// A destination that would be catastrophic to forward an inbound request to.
	hooks := &stubHooks{addr: &agent.NetworkAddress{Version: 4, IPv4Addr: 0x5DB8D822, Port: 443}}
	pm := &IngressProxyManager{logger: zap.NewNop(), hooks: hooks}

	got := pm.getActualDestination(context.Background(), remoteFrom(t, "127.0.0.1:51000"), "127.0.0.1:41234", zap.NewNop())

	if hooks.getCalled {
		t.Fatal("the source-port lookup ran even though the forwarding target was known; a stale egress entry could misroute an inbound request")
	}
	if hooks.deleteCalled {
		t.Fatal("an egress destination entry was consumed while serving an inbound connection")
	}
	if want := "127.0.0.1:41234"; got != want {
		t.Fatalf("getActualDestination = %q, want the known target %q", got, want)
	}
}

// A malformed fallback must not be treated as a known target.
func TestGetActualDestinationMalformedFallbackStillLooksUp(t *testing.T) {
	hooks := &stubHooks{err: errors.New("no destination recorded")}
	pm := &IngressProxyManager{logger: zap.NewNop(), hooks: hooks}

	got := pm.getActualDestination(context.Background(), remoteFrom(t, "127.0.0.1:51000"), "not-an-address", zap.NewNop())

	if !hooks.getCalled {
		t.Fatal("a malformed fallback was treated as a known target")
	}
	if want := "not-an-address"; got != want {
		t.Fatalf("getActualDestination = %q, want %q", got, want)
	}
}

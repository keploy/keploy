//go:build linux

package proxy

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/synhold"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// ingressMoved is an ingress forwarder's map (SetAppListenPort): the run's
// socket for port forwarder listens at moved, and every other port is where
// it was. known false: where it went is not known yet.
func ingressMoved(forwarder, moved uint32, known bool) func(uint16) (uint16, bool) {
	return func(orig uint16) (uint16, bool) {
		if orig != uint16(forwarder) {
			return orig, true
		}
		if !known {
			return 0, false
		}
		return uint16(moved), true
	}
}

// keploy mock's record moves the binds of the run's processes so that its
// forwarder takes their ports and keeps a caller from outside the run as a
// test case. A dependency the run started is one of them: the app's call to
// it comes through the proxy, which records it as the dependency's mock and
// dials the dependency for it. That dial is keploy's own: it connects to the
// dependency's socket at its moved port, never to the forwarder, which would
// keep it as a test case too. So does a dial passed through, and the one a
// held handshake makes. keploy record's own forwarder is left as it was, and
// so is a port whose moved socket is not known yet.
func TestKeploysOwnDialToAPortTheIngressHoldsReachesTheMovedSocket(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mockMode    bool
		passthrough bool
		known       bool
		// forwarded: the dial reaches the forwarder, not the moved socket.
		forwarded bool
	}{
		{name: "mock record", mockMode: true, known: true},
		{name: "mock record passed through", mockMode: true, passthrough: true, known: true},
		{name: "keploy record", mockMode: false, known: true, forwarded: true},
		{name: "moved socket not known", mockMode: true, known: false, forwarded: true},
	} {
		for _, proven := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/proven=%v", tc.name, proven), func(t *testing.T) {
				recorded.forget()
				t.Cleanup(recorded.forget)
				if proven {
					t.Setenv("KEPLOY_PROVEN", "1")
				}
				moved, accepted, _ := echoUpstream(t)
				_, movedText, _ := net.SplitHostPort(moved)
				movedPort, _ := strconv.Atoi(movedText)
				forwarder, taken := countingServer(t, "fwd!")
				dest := fmt.Sprintf("127.0.0.1:%d", forwarder)

				cfg := config.New()
				cfg.Agent.Mode = models.MODE_RECORD
				cfg.Agent.MockMode = tc.mockMode
				cfg.Agent.GlobalPassthrough = tc.passthrough
				// The run's tree is another process's: the dependency and
				// the forwarder, this test process's, are outside it.
				cfg.Agent.ClientNSPID = uint32(hold(t).Process.Pid)
				p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, dest)}, cfg)
				p.nsswitchData = []byte("preseeded-by-test") // see testProxy
				p.SetAppListenPort(ingressMoved(forwarder, uint32(movedPort), tc.known))
				if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })

				app, src := acceptedConn(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- p.handleConnection(ctx, src) }()
				if _, err := app.Write([]byte("ping\n")); err != nil {
					t.Fatal(err)
				}
				want := "ping"
				if tc.forwarded {
					want = "fwd!"
				}
				if got := awaitEvent(t, readN(app, 4), "the call's answer"); got != want {
					t.Fatalf("the call was answered %q, want %q (the moved socket echoes, the forwarder answers fwd!)", got, want)
				}
				if tc.forwarded {
					if n := taken.Load(); n != 1 {
						t.Fatalf("the forwarder took %d connections, want the dial's one", n)
					}
				} else {
					_ = awaitEvent(t, accepted, "the moved socket taking the dial")
					if n := taken.Load(); n != 0 {
						t.Fatalf("keploy's own dial reached the forwarder (%d connections): it keeps it as a test case", n)
					}
				}
				_ = app.Close()
				cancel()
				_ = awaitEvent(t, done, "handleConnection returning")
			})
		}
	}
}

// A held handshake's dial (the connection the app's connect waits on) is
// keploy's own too: it connects to the moved socket, and the connection's
// first dial takes it there. The moved socket serves only the first
// connection it takes, so the echo is the held handshake's connection's.
func TestAHeldHandshakesDialToAPortTheIngressHoldsReachesTheMovedSocket(t *testing.T) {
	moved, _, _ := echoUpstream(t)
	_, movedText, _ := net.SplitHostPort(moved)
	movedPort, _ := strconv.Atoi(movedText)
	forwarder, taken := countingServer(t, "fwd!")
	dest := fmt.Sprintf("127.0.0.1:%d", forwarder)

	p := &Proxy{logger: zap.NewNop(), mockMode: true}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD})
	p.SetAppListenPort(ingressMoved(forwarder, uint32(movedPort), true))
	d := p.decideHandshake(fakeHandshakeDest{dest: ipv4Dest(t, dest)})(context.Background(), appEnd, proxyEnd)
	if d.Outcome != synhold.Accept || d.Keep == nil {
		t.Fatalf("decision %+v, want accept keeping the connection", d)
	}
	d.Keep()
	pre := p.predials.take(appEnd)
	if pre == nil {
		t.Fatal("no pre-dialled connection kept for the application's end")
	}
	if pre.Addr() != dest {
		t.Fatalf("the pre-dialled connection is %s's, want the destination's %s: the connection's first dial takes it by that", pre.Addr(), dest)
	}
	c, err := util.DialDestination(util.WithReachAt(context.Background(), p.reachAt), nil, "tcp", util.DialTarget{Addr: dest, Predialed: pre})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if got := awaitEvent(t, readN(c, 4), "the held handshake's connection's answer"); got != "ping" {
		t.Fatalf("the held handshake's connection was answered %q, want the moved socket's echo (the forwarder answers fwd!)", got)
	}
	if n := taken.Load(); n != 0 {
		t.Fatalf("the held handshake's dial reached the forwarder (%d connections)", n)
	}
}

// Only a destination on this machine is the run's: a port another host
// listens on is dialed there whatever this machine's forwarder holds. An
// address with no port number in a port's range is dialed as it is.
func TestReachAtMovesOnlyThisMachinesDestinations(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), mockMode: true}
	held := ingressMoved(18093, 45771, true)
	p.SetAppListenPort(func(orig uint16) (uint16, bool) {
		if orig == 65535 { // a port number's range is not a dial's: 99999 is not 65535
			return 45772, true
		}
		return held(orig)
	})
	for addr, want := range map[string]string{
		"127.0.0.1:18093":  "127.0.0.1:45771",
		"[::1]:18093":      "[::1]:45771",
		"0.0.0.0:18093":    "0.0.0.0:45771",
		"192.0.2.10:18093": "192.0.2.10:18093", // TEST-NET-1: never this machine's
		"127.0.0.1:18094":  "127.0.0.1:18094",
		"not-an-address":   "not-an-address",
		"127.0.0.1:99999":  "127.0.0.1:99999",
		"127.0.0.1:http":   "127.0.0.1:http",
	} {
		if got := p.reachAt(addr); got != want {
			t.Errorf("reachAt(%s) = %s, want %s", addr, got, want)
		}
	}
	// A proxy given no port map (SetAppListenPort) knows of no forwarder:
	// it dials every address as it is.
	unmapped := &Proxy{logger: zap.NewNop(), mockMode: true}
	if got := unmapped.reachAt("127.0.0.1:18093"); got != "127.0.0.1:18093" {
		t.Errorf("with no port map, reachAt(127.0.0.1:18093) = %s, want it as it is", got)
	}
}

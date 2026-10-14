package proxy

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/agent"
)

// connDestInfo names destinations by the application's end of a connection,
// as the Linux hooks do, and notes what it was asked.
type connDestInfo struct {
	fakeDestInfo
	asked, dropped []netip.AddrPort
	err            error
}

func (c *connDestInfo) GetForConn(_ context.Context, app netip.AddrPort) (*agent.NetworkAddress, error) {
	c.asked = append(c.asked, app)
	if c.err != nil {
		return nil, c.err
	}
	d := *c.dest
	return &d, nil
}

func (c *connDestInfo) DeleteForConn(_ context.Context, app netip.AddrPort) error {
	c.dropped = append(c.dropped, app)
	return nil
}

// Where the hooks key a redirect by the application's end of the connection,
// the proxy looks it up, and drops it, by exactly that: never by the source
// port alone, which an IPv4 and an IPv6 connection can share.
func TestTheProxyLooksARedirectUpByTheApplicationsEnd(t *testing.T) {
	app, proxySide := acceptedConn(t)
	d := &connDestInfo{fakeDestInfo: fakeDestInfo{dest: &agent.NetworkAddress{Version: 4, Port: 9}}}
	p := &Proxy{logger: zap.NewNop(), DestInfo: d}
	got, err := p.destFor(context.Background(), proxySide, 1)
	if err != nil || got.Port != 9 {
		t.Fatalf("destFor = %+v, %v", got, err)
	}
	if err := p.dropDest(context.Background(), proxySide, 1); err != nil {
		t.Fatal(err)
	}
	want := app.LocalAddr().(interface{ AddrPort() netip.AddrPort }).AddrPort()
	if len(d.asked) != 1 || d.asked[0] != want || len(d.dropped) != 1 || d.dropped[0] != want {
		t.Fatalf("asked %v, dropped %v; want the application's end %v", d.asked, d.dropped, want)
	}
}

// A connection the proxy cannot route is said, loud: a WARN with the running
// count and the reason, at the first and at every doubling, never only a
// DEBUG; the total as the proxy stops when no WARN said it; and a connection
// that arrives while the proxy stops is a DEBUG, not counted.
func TestAnUntrackedConnectionIsAWarnWithItsCount(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	_, proxySide := acceptedConn(t)
	p := &Proxy{logger: zap.New(core)}
	lookupErr := errors.New("lookup: key does not exist")
	for i := 0; i < 3; i++ {
		p.untracked(context.Background(), proxySide, lookupErr)
	}
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 2 {
		t.Fatalf("%d WARNs for 3 untracked connections, want 2 (the 1st and 2nd): %v", len(warns), logs.All())
	}
	for i, want := range []uint64{1, 2} {
		f := warns[i].ContextMap()
		if f["untracked_connections"] != want || f["reason"] == nil || f["error"] != lookupErr.Error() {
			t.Fatalf("WARN %d: %v, want the count %d, a reason and the error", i, f, want)
		}
	}
	stopping, cancel := context.WithCancel(context.Background())
	cancel()
	p.untracked(stopping, proxySide, lookupErr) // would be the 4th, a doubling
	if n := len(logs.FilterLevelExact(zapcore.WarnLevel).All()); n != 2 {
		t.Fatalf("%d WARNs after a connection while the proxy stops, want still 2", n)
	}
	if n := p.untrackedConns.Load(); n != 3 {
		t.Fatalf("counted %d untracked connections, want 3: one while the proxy stops is not counted", n)
	}
	// The 3rd was only a DEBUG: the total is said as the proxy stops.
	p.untrackedTotal()
	warns = logs.FilterLevelExact(zapcore.WarnLevel).All()
	if len(warns) != 3 || warns[2].ContextMap()["untracked_connections"] != uint64(3) {
		t.Fatalf("WARNs %v, want a 3rd with the total, 3", warns)
	}
	// A total a WARN already said is not said again.
	p.untracked(context.Background(), proxySide, lookupErr) // the 4th: a WARN
	before := len(logs.FilterLevelExact(zapcore.WarnLevel).All())
	p.untrackedTotal()
	if after := len(logs.FilterLevelExact(zapcore.WarnLevel).All()); after != before {
		t.Fatalf("the total 4, already said, was said again")
	}
}

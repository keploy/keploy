package http

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap/zaptest"
)

// recordClocked records one exchange through recordV2: the request's chunk on
// reqClock, the response's on respClock, as the producer that took each
// stamp named it.
func recordClocked(t *testing.T, reqClock, respClock fakeconn.Clock) *models.Mock {
	t.Helper()
	var captured atomic.Uint64
	stream := func(dir fakeconn.Direction, b []byte, at time.Time, clock fakeconn.Clock) *fakeconn.FakeConn {
		ch := make(chan fakeconn.Chunk, 1)
		ch <- fakeconn.Chunk{Dir: dir, ReadClock: clock, ConnSeq: fakeconn.ConnSeqOf(captured.Add(1)),
			Bytes: append([]byte(nil), b...), ReadAt: at, WrittenAt: at.Add(time.Millisecond), SeqNo: 1}
		close(ch)
		c := fakeconn.New(ch, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	at := time.Unix(1_700_000_000, 0)
	mocks := make(chan *models.Mock, 2)
	sess := &supervisor.Session{
		ClientStream: stream(fakeconn.FromClient, canonicalRequest, at, reqClock),
		DestStream:   stream(fakeconn.FromDest, canonicalResponse, at.Add(5*time.Millisecond), respClock),
		Mocks:        mocks, Logger: zaptest.NewLogger(t), ClientConnID: "clocked-1", DestConnID: "clocked-dest-1",
		Ctx: context.Background(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, sess); err != nil {
		t.Fatalf("recordV2: %v", err)
	}
	select {
	case m := <-mocks:
		return m
	default:
		t.Fatal("no mock recorded")
		return nil
	}
}

// testKtimeClock names the ktime clock (fakeconn.ClockKtime) in this
// package's tests, as a proxyless capture's anchor names it when it is taken:
// the name is the process's, set once, so every test that needs it reads this
// one (nameKtimeClock).
const testKtimeClock = "ktime:boot-test.anchor-test"

// nameKtimeClock names fakeconn.ClockKtime for this test process and returns
// its id. Setting the same id again does nothing.
func nameKtimeClock() string {
	fakeconn.SetClockID(fakeconn.ClockKtime, testKtimeClock)
	return testKtimeClock
}

// Under the proven switch a recorded HTTP call names the clock its request's
// stamp is on (models.Origin.Clock), as its producer named it: the request's
// first chunk's, never the response's. That is the proxy's clock through the
// relay, and the ktime clock in a proxyless capture, which feeds this
// recorder chunks stamped on it. A stamp no producer vouched for
// (fakeconn.ClockUnset) names none, and without the switch nothing is
// written. A set that holds two record runs' recordings (a partial record)
// orders the call by it against its own run's recordings alone.
func TestRecordV2NamesTheClockOfTheRequestsStamp(t *testing.T) {
	proxy := fakeconn.NameProxyClock()
	ktime := nameKtimeClock()
	clockOf := func(m *models.Mock) string {
		if m.Origin == nil {
			return ""
		}
		return m.Origin.Clock
	}
	for _, c := range []struct {
		name                string
		on                  bool
		reqClock, respClock fakeconn.Clock
		want                string
	}{
		{"under the switch, the request on the proxy's clock", true, fakeconn.ClockProxy, fakeconn.ClockUnset, proxy},
		{"under the switch, the request on the ktime clock", true, fakeconn.ClockKtime, fakeconn.ClockKtime, ktime},
		{"under the switch, the request's stamp vouched for by no producer", true, fakeconn.ClockUnset, fakeconn.ClockProxy, ""},
		{"without the switch", false, fakeconn.ClockProxy, fakeconn.ClockProxy, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "0", true: "1"}[c.on])
			m := recordClocked(t, c.reqClock, c.respClock)
			if got := clockOf(m); got != c.want {
				t.Fatalf("the recorded call names clock %q, want %q", got, c.want)
			}
			if c.want == "" && m.Origin != nil {
				t.Fatalf("the recorded call has an origin %+v; a call whose stamp names no clock gets none from its recorder", *m.Origin)
			}
		})
	}
}

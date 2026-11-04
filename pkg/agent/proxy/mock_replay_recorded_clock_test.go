package proxy

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

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

// recordedHTTPCall is one HTTP call as keploy's HTTP recorder records it: its
// request read at exactT0+at, its chunks on clock (fakeconn.ClockProxy through
// the relay, fakeconn.ClockKtime in a proxyless capture), which the caller
// names first (fakeconn.NameProxyClock, nameKtimeClock). The call is named
// name and lives for one test.
func recordedHTTPCall(t *testing.T, name string, at time.Duration, clock fakeconn.Clock) *models.Mock {
	t.Helper()
	if fakeconn.ClockID(clock) == "" {
		t.Fatalf("precondition: the %s clock is not named", clock)
	}
	var captured atomic.Uint64
	stream := func(dir fakeconn.Direction, b string, ts time.Time) *fakeconn.FakeConn {
		ch := make(chan fakeconn.Chunk, 1)
		ch <- fakeconn.Chunk{Dir: dir, ReadClock: clock, ConnSeq: fakeconn.ConnSeqOf(captured.Add(1)),
			Bytes: []byte(b), ReadAt: ts, WrittenAt: ts, SeqNo: 1}
		close(ch)
		c := fakeconn.New(ch, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8200})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	ts := exactT0.Add(at)
	mocks := make(chan *models.Mock, 1)
	sess := &supervisor.Session{
		ClientStream: stream(fakeconn.FromClient, "GET /v1/secret/app HTTP/1.1\r\nHost: vault:8200\r\n\r\n", ts),
		DestStream:   stream(fakeconn.FromDest, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", ts.Add(time.Millisecond)),
		Mocks:        mocks, Logger: zap.NewNop(), ClientConnID: name, Ctx: context.Background(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpint.New(zap.NewNop()).RecordOutgoing(ctx, &integrations.RecordSession{V2: sess}); err != nil {
		t.Fatalf("the HTTP recorder: %v", err)
	}
	select {
	case mk := <-mocks:
		mk.Name = name
		mk.TestModeInfo.Lifetime = models.LifetimePerTest
		return mk
	default:
		t.Fatal("the HTTP recorder recorded no call")
		return nil
	}
}

// A partial record's test recorded anew may make HTTP calls alone (a read of
// a secret store), and a newer run may record an HTTP call that no test
// lists. keploy's HTTP recorder names the clock its call's stamp is on (the
// relay's: the proxy's of that run; a proxyless capture's: the ktime clock of
// that run), so under keploy mock replay such a call bounds the teardown of
// its own run's clock alone: the older run's call after its last test (a Go
// mongo-driver's endSessions at client.Disconnect) is still its teardown, and
// is answered. When the call named no clock, it bounded every clock in both
// shapes (no other recording of its test told its run, or no test listed it),
// and that endSessions missed outside every test.
func TestUnderMockReplayANewerRunsRecordedHTTPCallBoundsOnlyItsRunsClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	sBoot := exactMongo("s-boot", 100*time.Millisecond, models.LifetimePerTest)
	s1 := exactMongo("s1", time.Second, models.LifetimePerTest)
	sGap := exactMongo("s-gap", 1500*time.Millisecond, models.LifetimePerTest)
	s2 := exactMongo("s2", 2*time.Second, models.LifetimePerTest)
	sEnd := exactMongo("s-end", 3*time.Second, models.LifetimePerTest)
	slice := []string{"s-boot", "s-gap", "s-end"}
	// Each run on its own clock, both of one kind: the older run's (s), as its
	// recordings name it, and the newer run's, 42 s later, this process's
	// clock of the kind its producer stamps: the proxy's through the relay,
	// the ktime clock in a proxyless capture.
	for _, newer := range []struct {
		name  string
		s     string
		clock fakeconn.Clock
		id    string
	}{
		{"the newer run on the proxy's clock", "proxy:5555", fakeconn.ClockProxy, fakeconn.NameProxyClock()},
		{"the newer run on the ktime clock", "ktime:boot-old.anchor-old", fakeconn.ClockKtime, nameKtimeClock()},
	} {
		t.Run(newer.name, func(t *testing.T) {
			s, l := newer.s, newer.id
			onClock(s, sBoot, s1, sGap, s2, sEnd)
			l1 := exactMongo("l1", 45*time.Second, models.LifetimePerTest)
			onClock(l, l1)
			lHTTP := recordedHTTPCall(t, "l-http", 46*time.Second, newer.clock)
			if lHTTP.Origin == nil || lHTTP.Origin.Clock != l || !lHTTP.Spec.ReqTimestampMock.Equal(exactT0.Add(46*time.Second)) {
				t.Errorf("the recorded HTTP call names origin %+v at %v; want it on the newer run's clock %q, at its request's stamp", lHTTP.Origin, lHTTP.Spec.ReqTimestampMock, l)
			}
			for _, shape := range []struct {
				name  string
				tests map[string][]string
				t3    []*models.Mock
				all   []*models.Mock
			}{
				{"the newer run's test makes HTTP calls alone", map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l-http"}},
					[]*models.Mock{lHTTP}, []*models.Mock{sBoot, s1, sGap, s2, sEnd, lHTTP}},
				{"no test lists the newer run's HTTP call", map[string][]string{"T1": {"s1"}, "T2": {"s2"}, "T3": {"l1"}},
					[]*models.Mock{l1}, []*models.Mock{sBoot, s1, sGap, s2, sEnd, l1, lHTTP}},
			} {
				owners := map[string]string{}
				for _, names := range shape.tests {
					for _, n := range names {
						owners[n] = "k"
					}
				}
				for _, inTrees := range []bool{false, true} {
					where := map[bool]string{false: "on disk", true: "in the trees"}[inTrees]
					t.Run(shape.name+", the tests' recordings "+where, func(t *testing.T) {
						staged := []*models.Mock{sBoot, sGap, sEnd}
						if inTrees {
							staged = shape.all
						}
						var times func(func(string, time.Time, bool, string) bool) error
						if !inTrees {
							times = onDisk(t, shape.all...).RangeTimes
						}
						mm := stagedManagerOnDisk(t, staged, times)
						if err := (&Proxy{mockManager: mm}).SetPoolNames(slice, owners, shape.tests); err != nil {
							t.Fatalf("SetPoolNames: %v", err)
						}
						after := mm.teardownAfter("k", s)
						if after == nil || !after(sEnd.Spec.ReqTimestampMock) {
							t.Fatal("s-end, sent after every test the older run recorded, is not the teardown of its clock: the newer run's HTTP call bounded it")
						}
						for _, mk := range []*models.Mock{sBoot, sGap} {
							if after(mk.Spec.ReqTimestampMock) {
								t.Errorf("FALSE GREEN? %s, sent before a test of its own clock, reads as the teardown", mk.Name)
							}
						}
						// The HTTP call still bounds its own run's clock.
						if afterL := mm.teardownAfter("k", l); afterL == nil || afterL(lHTTP.Spec.ReqTimestampMock) {
							t.Fatal("FALSE GREEN? the newer run's HTTP call does not bound its own run's clock")
						}
						// The replay: T1 and T2 (the older run's), then T3
						// (the newer's), then the client's disconnect.
						mm.ServeExactPool("k", slice)
						perTestReads(t, mm, "before the first test")
						for _, tc := range []struct {
							name string
							own  []*models.Mock
						}{{"T1", []*models.Mock{s1}}, {"T2", []*models.Mock{s2}}, {"T3", shape.t3}} {
							mm.ServeExactTest(tc.name, tc.own)
							for _, mk := range tc.own {
								if !mm.DeleteFilteredMock(*mk) {
									t.Fatalf("precondition: %s's own recording %s did not answer it", tc.name, mk.Name)
								}
							}
							mm.ServeExactPool("k", slice)
						}
						perTestReads(t, mm, "after the last test", "s-end")
						if !mm.DeleteFilteredMock(*sEnd) {
							t.Fatal("the call after the last test (endSessions) was not answered by the older run's recording of it")
						}
					})
				}
			}
		})
	}
}

package recorder

import (
	"context"
	"fmt"
	"testing"
	"time"

	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// behindAtStop captures a handshake and whole queries 1..n, then ends the
// streams as the relay does at its teardown, with the recording already
// stopped: the recorder starts behind all of its capture. Mocks go to h.mocks,
// through the sync-mock manager when viaSyncMock (as the proxy routes them).
func behindAtStop(t *testing.T, h *v2Harness, n int, viaSyncMock bool) (context.Context, *syncMock.SyncMockManager) {
	t.Helper()
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	greetingBuf := cannedHandshakeV10(t)
	greeting, err := connphase.DecodeHandshakeV10(context.Background(), zap.NewNop(), greetingBuf[4:])
	if err != nil {
		t.Fatal(err)
	}
	h.pushDest(greetingBuf, base)
	h.pushClient(cannedHandshakeResponse41(t, 1, false), base.Add(time.Millisecond))
	h.pushDest(cannedOK(t, 2, greeting.CapabilityFlags), base.Add(2*time.Millisecond))
	for q := 1; q <= n; q++ {
		at := base.Add(time.Duration(10*q) * time.Millisecond)
		h.pushClient(cannedCOMQuery(t, 0, fmt.Sprintf("SELECT %d", q)), at)
		h.pushDest(cannedOK(t, 1, greeting.CapabilityFlags), at.Add(time.Millisecond))
	}
	h.closeStreams()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.sess.Ctx = ctx
	h.sess.RecordingDone = ctx.Done()
	mgr := syncMock.New(nil)
	h.sess.Mgr = mgr
	if viaSyncMock {
		mgr.SetOutputChannel(h.mocks)
		h.sess.RouteMocksViaSyncMock = true
	}
	return ctx, mgr
}

// takeMocks reads h's mocks until it has want of them, or none comes for 10s
// (a lost mock is then a failure, not a hang).
func takeMocks(h *v2Harness, want int) []*models.Mock {
	var got []*models.Mock
	for len(got) < want {
		select {
		case m := <-h.mocks:
			got = append(got, m)
		case <-time.After(10 * time.Second):
			return got
		}
	}
	return got
}

// A recorder behind its capture at the recording's stop records every
// exchange its capture holds whole: the stop ends its context before it has
// read them, and it reads them all the same, off the streams the relay ends
// at its teardown once it has handed on what it captured. It returned at its
// command loop's check of the context instead (and its reads, which return
// the context's error before every read, never read a packet), and the
// session refused the handshake's mock once the context had ended: all four
// exchanges were lost with nothing recording, leaving out or reporting them.
// Under both switches, and both ways a mock is handed on (the session's
// channel, and the sync-mock manager the proxy routes mocks through).
func TestRecordV2_BehindAtTheStopRecordsEveryExchangeCapturedWhole(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, viaSyncMock := range []bool{false, true} {
			t.Run(fmt.Sprintf("proven=%v, sync-mock=%v", proven, viaSyncMock), func(t *testing.T) {
				if proven {
					t.Setenv("KEPLOY_PROVEN", "1")
				} else {
					t.Setenv("KEPLOY_PROVEN", "")
				}
				const queries = 3
				h := newV2Harness(t)
				ctx, mgr := behindAtStop(t, h, queries, viaSyncMock)
				if err := recordV2NoPanic(t, ctx, zaptest.NewLogger(t), h.sess); err != nil {
					t.Fatalf("RecordV2 = %v, want nil: the streams' end ends the recording", err)
				}
				got := takeMocks(h, 1+queries)
				if len(got) != 1+queries {
					t.Fatalf("recorded %d mock(s), want the handshake's and the %d queries captured whole before the stop", len(got), queries)
				}
				if got[0].Spec.Metadata["type"] != "config" {
					t.Fatalf("first mock is %q, want the handshake's (config)", got[0].Spec.Metadata["type"])
				}
				for i, m := range got[1:] {
					qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
					if want := fmt.Sprintf("SELECT %d", i+1); !ok || qp.Query != want {
						t.Fatalf("mock %d is of %+v, want the query %q", i+1, m.Spec.MySQLRequests[0].Message, want)
					}
					if len(m.Spec.MySQLResponses) == 0 {
						t.Fatalf("mock %d has no response", i+1)
					}
				}
				if n := mgr.MocksLeftOut(); n != 0 {
					t.Fatalf("%d mock(s) left out, want none: every exchange was captured whole", n)
				}
			})
		}
	}
}

// A mock the session refuses as its context ends (between the recorder's
// check of it and EmitMock's own) is left out with its reason, not dropped.
// The context here ends inside EmitMock, in the hook it runs before its own
// check: the handshake's mock is refused and left out, and the queries after
// it, read once the context has ended, are recorded.
//
// Not parallel: the WARNs are limited process-wide, and this test reads them.
func TestRecordV2_AMockRefusedAsTheStopComesIsLeftOut(t *testing.T) {
	resetWarnLimiters()
	h := newV2Harness(t)
	_, mgr := behindAtStop(t, h, 2, true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.sess.Ctx = ctx
	h.sess.OnMockRecorded = func(*models.Mock) { cancel() }
	core, logs := observer.New(zapcore.DebugLevel)
	h.sess.Logger = zap.New(core)
	if err := recordV2NoPanic(t, ctx, zaptest.NewLogger(t), h.sess); err != nil {
		t.Fatalf("RecordV2 = %v, want nil", err)
	}
	got := takeMocks(h, 2)
	if len(got) != 2 || got[0].Spec.Metadata["type"] == "config" {
		t.Fatalf("recorded %d mock(s), want the 2 queries read after the stop", len(got))
	}
	var reasons []string
	for _, e := range leftOutWarnings(logs) {
		reasons = append(reasons, fmt.Sprint(e.ContextMap()["reason"]))
	}
	if n := mgr.MocksLeftOut(); n != 1 || len(reasons) != 1 || reasons[0] != leftOutRefusedAtStop {
		t.Fatalf("%d left out, reasons %q; want the handshake's mock left out once for %q", n, reasons, leftOutRefusedAtStop)
	}
}

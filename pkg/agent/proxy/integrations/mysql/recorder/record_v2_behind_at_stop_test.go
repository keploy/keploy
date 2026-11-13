package recorder

import (
	"context"
	"fmt"
	"testing"
	"time"

	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
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

// The recording's stop can come inside EmitMock, after the recorder saw the
// context last and before EmitMock's own look at it, which comes after the
// post-record hooks. Here the stop comes as the handshake's mock is handed
// over (its record hook). Through the sync-mock manager, as the agent
// records, the mock is recorded all the same: it was captured before the
// stop, and the supervisor's stop waits for the recorder to read what was
// captured. Nothing is left out, and the queries read after it are recorded.
//
// Before, EmitMock refused the handshake's mock, and it was left out with
// the test cases over it.
//
// Not parallel: the WARNs are limited process-wide, and this test reads them.
func TestRecordV2_AMockHandedOverAsTheStopComesIsRecorded(t *testing.T) {
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
	got := takeMocks(h, 3)
	if len(got) != 3 || got[0].Spec.Metadata["type"] != "config" {
		t.Fatalf("recorded %d mock(s), want the handshake's and the 2 queries' (every exchange was captured before the stop)", len(got))
	}
	var reasons []string
	for _, e := range leftOutWarnings(logs) {
		reasons = append(reasons, fmt.Sprint(e.ContextMap()["reason"]))
	}
	if n := mgr.MocksLeftOut(); n != 0 || len(reasons) != 0 {
		t.Fatalf("%d left out, reasons %q; want none", n, reasons)
	}
}

// Straight to the session's channel, a mock EmitMock refuses as the stop
// comes (the context ends between emitV2's look at it and EmitMock's own,
// here in the hook EmitMock runs before it, with no room on the channel) is
// left out with its reason, counted and spanned, not dropped with nothing
// said.
//
// Not parallel: the WARNs are limited process-wide, and this test reads them.
func TestEmitV2_AMockRefusedAsTheStopComesIsLeftOut(t *testing.T) {
	resetWarnLimiters()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := syncMock.New(nil)
	spans := &syncMock.Spans{}
	core, logs := observer.New(zapcore.DebugLevel)
	sess := &supervisor.Session{
		Ctx:            ctx,
		Mgr:            mgr,
		Mocks:          make(chan *models.Mock),
		OnMockRecorded: func(*models.Mock) { cancel() },
		Orphans:        spans,
		Logger:         zap.New(core),
	}
	emitV2(sess, emitAtStopMock("refused"))
	var reasons []string
	for _, e := range leftOutWarnings(logs) {
		reasons = append(reasons, fmt.Sprint(e.ContextMap()["reason"]))
	}
	if n, s := mgr.MocksLeftOut(), spanCount(spans); n != 1 || s != 1 || len(reasons) != 1 || reasons[0] != leftOutRefusedAtStop {
		t.Fatalf("%d left out (%q), %d span(s); want the refused mock left out once for %q, with its span", n, reasons, s, leftOutRefusedAtStop)
	}
}

// Straight to the session's channel, every mock handed over after the stop
// is recorded while the channel has room: EmitMock would refuse each one
// whose send it did not pick (a select between the send and the context's
// end, both ready), so emitV2 uses EmitMockOnShutdown once the context has
// ended.
func TestEmitV2_MocksHandedOverAfterTheStopAreRecorded(t *testing.T) {
	const n = 24
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mgr := syncMock.New(nil)
	out := make(chan *models.Mock, n)
	sess := &supervisor.Session{Ctx: ctx, Mgr: mgr, Mocks: out, Orphans: &syncMock.Spans{}, Logger: zaptest.NewLogger(t)}
	for i := 0; i < n; i++ {
		emitV2(sess, emitAtStopMock(fmt.Sprint("after the stop ", i)))
	}
	if got, left := len(out), mgr.MocksLeftOut(); got != n || left != 0 {
		t.Fatalf("recorded %d of the %d mocks handed over after the stop, %d left out; want every one recorded", got, n, left)
	}
}

// emitAtStopMock is a mock with the times of an exchange, so a mock left out
// gets its span.
func emitAtStopMock(name string) *models.Mock {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	return &models.Mock{Kind: models.MySQL, Name: name, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
}

// spanCount is how many spans s holds.
func spanCount(s *syncMock.Spans) int {
	n, _ := s.Counts()
	return n
}

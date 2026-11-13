package generic

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// behindRun is what runBehindAtTheStop saw: the mocks recorded, the reason of
// each mock left out, and the spans left out.
type behindRun struct {
	recorded []*models.Mock
	leftOut  []string
	spans    int
}

// runBehindAtTheStop captures n exchanges whole (numbered as the relay numbers
// them) and records them with the generic parser, its mocks handed to the
// sync-mock manager when viaSyncMock is set (as in production) or to the
// session's channel. stopFirst stops the recording before the parser starts,
// as for a parser behind its capture at the stop; otherwise the stop comes in
// the first mock's record hook, after the parser checked the context and
// before EmitMock checks it. viaDispatch runs the parser from RecordOutgoing,
// its readers taking the chunks from both streams (ended as the relay's
// teardown ends them) and its merge pairing them in capture order; otherwise
// its loop takes them in wire order.
func runBehindAtTheStop(t *testing.T, n int, viaSyncMock, stopFirst, viaDispatch bool) behindRun {
	t.Helper()
	clientCh, destCh := make(chan fakeconn.Chunk, n), make(chan fakeconn.Chunk, n)
	events := make(chan chunkEvent, 2*n)
	// Every chunk is on its stream from the start, and the relay's capture
	// order says so: the readers' order is the scheduler's, and the parser
	// pairs the chunks in the order captured.
	order := newCaptureOrder(nil)
	for i := 0; i < n; i++ {
		at := time.Unix(int64(5000+10*i), 0)
		req := fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(fmt.Sprintf("req%d", i)), ReadAt: at, ConnSeq: uint32(2*i + 1)} // #nosec G115 -- test-only
		resp := fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte(fmt.Sprintf("resp%d", i)), ReadAt: at.Add(time.Second),
			WrittenAt: at.Add(time.Second), ConnSeq: uint32(2*i + 2)} // #nosec G115 -- test-only
		clientCh <- req
		destCh <- resp
		order.put[0] += int64(len(req.Bytes))
		order.put[1] += int64(len(resp.Bytes))
		for _, c := range []fakeconn.Chunk{req, resp} {
			events <- chunkEvent{dir: c.Dir, bytes: c.Bytes, connSeq: c.ConnSeq, readAt: c.ReadAt, writtenAt: c.WrittenAt}
		}
	}
	close(clientCh)
	close(destCh)
	close(events)
	sess, mocks := newV2Session(t, clientCh, destCh)
	sess.Order = order.StreamedBefore
	core, logs := observer.New(zapcore.DebugLevel)
	sess.Logger = zap.New(core)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess.Ctx = ctx
	sess.RecordingDone = ctx.Done()
	mgr := syncMock.New(nil)
	sess.Mgr = mgr
	spans := &syncMock.Spans{}
	sess.Orphans = spans
	if viaSyncMock {
		mgr.SetOutputChannel(mocks)
		sess.RouteMocksViaSyncMock = true
	}
	if stopFirst {
		cancel()
	} else {
		sess.OnMockRecorded = func(*models.Mock) { cancel() }
	}
	var err error
	if viaDispatch {
		err = dispatch(t, sess)
	} else {
		err = pairChunkEvents(events, sess, sess.Logger)
	}
	if err != nil {
		t.Fatalf("the parser returned %v", err)
	}
	var r behindRun
	for _, e := range logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			reason, _ := e.ContextMap()["reason"].(string)
			r.leftOut = append(r.leftOut, reason)
		}
	}
	deadline := time.After(10 * time.Second)
collect:
	for len(r.recorded)+len(r.leftOut) < n {
		select {
		case m := <-mocks:
			r.recorded = append(r.recorded, m)
		case <-deadline:
			break collect
		}
	}
	select {
	case m := <-mocks:
		r.recorded = append(r.recorded, m)
	case <-time.After(50 * time.Millisecond):
	}
	if left := mgr.MocksLeftOut(); int(left) != len(r.leftOut) {
		t.Fatalf("the manager counts %d mock(s) left out, the log says %d (%q)", left, len(r.leftOut), r.leftOut)
	}
	r.spans, _ = spans.Counts()
	return r
}

// reqsOf is the request each mock recorded holds, in order.
func reqsOf(mocks []*models.Mock) []string {
	var got []string
	for _, m := range mocks {
		for _, req := range m.Spec.GenericRequests {
			for _, msg := range req.Message {
				got = append(got, msg.Data)
			}
		}
	}
	return got
}

// A generic parser behind its capture at the recording's stop records every
// exchange captured whole before it, however the mocks are delivered. Before,
// it read them all but handed each to EmitMock, which refuses every mock once
// the session's context has ended: on the sync-mock path (production's) all
// were dropped with a Debug line only, and on the channel each was dropped or
// not by a select's coin.
func TestV2_BehindAtTheStopRecordsEveryExchangeCapturedWhole(t *testing.T) {
	const n = 3
	for _, viaDispatch := range []bool{false, true} {
		for _, viaSyncMock := range []bool{true, false} {
			t.Run(fmt.Sprintf("viaDispatch=%v/viaSyncMock=%v", viaDispatch, viaSyncMock), func(t *testing.T) {
				r := runBehindAtTheStop(t, n, viaSyncMock, true, viaDispatch)
				if got := reqsOf(r.recorded); len(r.recorded) != n || len(r.leftOut) != 0 || len(got) != n {
					t.Fatalf("recorded %d of the %d exchanges captured whole before the stop (%q), %d left out (%q); want every one recorded",
						len(r.recorded), n, got, len(r.leftOut), r.leftOut)
				}
			})
		}
	}
}

// The recording's stop can come inside EmitMock, after the parser saw the
// context last and before EmitMock's own look at it, which comes after the
// post-record hooks: here in the first mock's record hook. Through the
// sync-mock manager, as the agent records, that mock is recorded all the
// same, and so is every exchange after it, with nothing left out: each was
// captured whole before the stop, and the supervisor's stop waits for the
// parser to read what was captured.
//
// Before, EmitMock refused the first mock, and it was left out with the test
// cases over it.
func TestV2_AMockHandedOverAsTheStopComesIsRecorded(t *testing.T) {
	const n = 3
	r := runBehindAtTheStop(t, n, true, false, false)
	if got := reqsOf(r.recorded); len(r.recorded) != n || len(got) != n || len(r.leftOut) != 0 || r.spans != 0 {
		t.Fatalf("recorded %d of the %d exchanges captured whole (%q), %d left out (%q), %d span(s); want every one recorded",
			len(r.recorded), n, got, len(r.leftOut), r.leftOut, r.spans)
	}
}

// Straight to the session's channel, a mock handed over as the stop comes is
// recorded or refused by a select's coin (the send and the context's end are
// both ready, the channel having room): a refused one is left out with its
// reason, counted and spanned, not dropped with nothing said, and the
// exchanges after it, handed over once the context has ended, are recorded.
func TestV2_AMockHandedOverAsTheStopComesOnTheChannelIsRecordedOrLeftOut(t *testing.T) {
	const n = 3
	r := runBehindAtTheStop(t, n, false, false, false)
	if len(r.recorded)+len(r.leftOut) != n {
		t.Fatalf("recorded %d (%q) and left out %d (%q) of the %d exchanges captured whole: some vanished with nothing said",
			len(r.recorded), reqsOf(r.recorded), len(r.leftOut), r.leftOut, n)
	}
	if len(r.leftOut) > 1 {
		t.Fatalf("left out %d (%q); want at most the one refused as the stop came", len(r.leftOut), r.leftOut)
	}
	for _, reason := range r.leftOut {
		if reason != leftOutRefusedAtStop {
			t.Fatalf("left out for %q, want %q", reason, leftOutRefusedAtStop)
		}
	}
	if r.spans != len(r.leftOut) {
		t.Fatalf("%d span(s) left out for %d mock(s) left out; want each one's span", r.spans, len(r.leftOut))
	}
}

// emitAtStopMock is a mock with the times of an exchange, so a mock left out
// gets its span.
func emitAtStopMock(name string) *models.Mock {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	return &models.Mock{Kind: models.GENERIC, Name: name, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}
}

// Straight to the session's channel, a mock EmitMock refuses as the stop
// comes (the context ends between emitV2's look at it and EmitMock's own,
// here in the hook EmitMock runs before it, with no room on the channel) is
// left out with its reason, counted and spanned, not dropped with nothing
// said, and emitV2 says it was left out.
func TestEmitV2_AMockRefusedAsTheStopComesIsLeftOut(t *testing.T) {
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
	leftOut := emitV2(sess, sess.Logger, emitAtStopMock("refused"))
	var reasons []string
	for _, e := range logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			reasons = append(reasons, fmt.Sprint(e.ContextMap()["reason"]))
		}
	}
	s, _ := spans.Counts()
	if !leftOut || mgr.MocksLeftOut() != 1 || s != 1 || len(reasons) != 1 || reasons[0] != leftOutRefusedAtStop {
		t.Fatalf("emitV2 = %v, %d left out (%q), %d span(s); want the refused mock left out once for %q, with its span, and said so",
			leftOut, mgr.MocksLeftOut(), reasons, s, leftOutRefusedAtStop)
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
	core, _ := observer.New(zapcore.DebugLevel)
	sess := &supervisor.Session{Ctx: ctx, Mgr: mgr, Mocks: out, Orphans: &syncMock.Spans{}, Logger: zap.New(core)}
	var leftOut int
	for i := 0; i < n; i++ {
		if emitV2(sess, sess.Logger, emitAtStopMock(fmt.Sprint("after the stop ", i))) {
			leftOut++
		}
	}
	if got := len(out); got != n || leftOut != 0 || mgr.MocksLeftOut() != 0 {
		t.Fatalf("recorded %d of the %d mocks handed over after the stop, %d (%d) left out; want every one recorded", got, n, leftOut, mgr.MocksLeftOut())
	}
}

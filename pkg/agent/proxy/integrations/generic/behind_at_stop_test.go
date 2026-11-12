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

// A mock the session refuses as the stop comes (the context ends after the
// parser checked it and before EmitMock checks it: here in the record hook
// EmitMock runs first) is left out with its reason, counted and spanned, not
// dropped with nothing said; the exchanges after it, handed over once the
// context has ended, are recorded. On the channel the refusal is a select's
// coin (the mock may be taken), so there each exchange is recorded or left
// out, and at most the one is left out.
func TestV2_AMockRefusedAsTheStopComesIsLeftOut(t *testing.T) {
	const n = 3
	for _, viaSyncMock := range []bool{true, false} {
		t.Run(fmt.Sprint("viaSyncMock=", viaSyncMock), func(t *testing.T) {
			r := runBehindAtTheStop(t, n, viaSyncMock, false, false)
			if len(r.recorded)+len(r.leftOut) != n {
				t.Fatalf("recorded %d (%q) and left out %d (%q) of the %d exchanges captured whole: some vanished with nothing said",
					len(r.recorded), reqsOf(r.recorded), len(r.leftOut), r.leftOut, n)
			}
			if viaSyncMock && len(r.leftOut) != 1 {
				t.Fatalf("left out %d (%q); want the one refused as the stop came", len(r.leftOut), r.leftOut)
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
		})
	}
}

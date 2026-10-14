package generic

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// pairHoled runs evs through the exchange state machine on a session whose
// capture the relay has already flagged as missing bytes, keeping the spans
// it could not record in spans, and returns the mocks it emitted, how often it
// cleared its pending work, and the manager its mocks go to, which counts the
// mocks left out.
func pairHoled(t *testing.T, spans *syncMock.Spans, evs []chunkEvent) ([]*models.Mock, int, *syncMock.SyncMockManager) {
	t.Helper()
	events := make(chan chunkEvent, len(evs))
	for _, ev := range evs {
		events <- ev
	}
	close(events)
	mocks := make(chan *models.Mock, 4*len(evs)+8)
	logger := zaptest.NewLogger(t)
	mgr := syncMock.New(nil)
	cleared := 0
	sess := &supervisor.Session{
		Mocks:            mocks,
		Logger:           logger,
		Ctx:              context.Background(),
		ClientConnID:     "test-client-conn",
		Mgr:              mgr,
		Orphans:          spans,
		OnPendingCleared: func() { cleared++ },
	}
	sess.MarkMockIncomplete("memory_pressure")
	if err := pairChunkEvents(events, sess, logger); err != nil {
		t.Fatalf("pairChunkEvents returned error: %v", err)
	}
	close(mocks)
	return drainMocks(mocks), cleared, mgr
}

// An exchange the parser leaves out for the incomplete flag is reported as
// one the connection could not record, over its own times, so the test cases
// recorded over it are left out instead of saved without its mock. The parser
// checks the flag before it emits, so this is not EmitMock's doing: before, it
// reset the flag itself and the exchange was lost with nothing reporting it.
func TestV2_AnExchangeLeftOutForTheIncompleteFlagIsReported(t *testing.T) {
	t.Parallel()

	var (
		reqRead     = time.Unix(1000, 0)
		respWritten = time.Unix(1001, 0)
	)
	spans := &syncMock.Spans{}
	got, cleared, mgr := pairHoled(t, spans, []chunkEvent{
		clientChunk("q", reqRead),
		destChunk("r", respWritten),
	})

	if len(got) != 0 {
		t.Fatalf("expected the holed exchange to be left out, got %d mock(s)", len(got))
	}
	if c, o := spans.Counts(); c != 1 || o != 0 {
		t.Fatalf("the spans hold (closed=%d, open=%d), want the one exchange left out", c, o)
	}
	if over, _ := spans.Overlaps(reqRead.Add(time.Millisecond), reqRead.Add(2*time.Millisecond)); !over {
		t.Fatal("a test case over the exchange left out is not left out")
	}
	if over, _ := spans.Overlaps(respWritten.Add(time.Second), respWritten.Add(2*time.Second)); over {
		t.Fatal("a test case after the exchange is left out: the span is wider than the exchange")
	}
	if cleared != 1 {
		t.Fatalf("pending work cleared %d time(s), want once: the parser consumed its input", cleared)
	}
	if n := mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("the manager counts %d mocks left out, want the one exchange", n)
	}
}

// A holed exchange a split cuts in two loses both halves (the half it carried
// forward is part of a capture known to be missing bytes), and both are
// reported and counted: the carried half over its requests' reads, from its
// first to its last, since its answer is yet to come. Before, the carried half
// was dropped with nothing counting it, and the recording's summary said one
// mock was left out where two were.
func TestV2_TheHalfASplitCarriesOutOfAHoledExchangeIsReported(t *testing.T) {
	t.Parallel()

	var (
		helloRead  = time.Unix(1000, 0)
		helloReply = time.Unix(1001, 0)
		pingRead   = time.Unix(1002, 0)
		ping2Read  = time.Unix(1004, 0)
		pong       = time.Unix(1005, 0)
	)
	spans := &syncMock.Spans{}
	got, _, mgr := pairHoled(t, spans, []chunkEvent{
		clientChunk("HELLO", helloRead),
		clientChunk("PING", pingRead),
		clientChunk("PING2", ping2Read),
		destChunk("hello-reply", helloReply),
		destChunk("pong", pong),
	})

	if len(got) != 0 {
		t.Fatalf("expected the holed exchange to be dropped, got %d mock(s)", len(got))
	}
	if over, _ := spans.Overlaps(helloRead, helloRead); !over {
		t.Fatal("the half the split emitted was left out without being reported")
	}
	if over, _ := spans.Overlaps(pingRead, pingRead); !over {
		t.Fatal("the half the split carried forward was dropped without being reported")
	}
	if over, _ := spans.Overlaps(ping2Read, ping2Read); !over {
		t.Fatal("the carried half's last request is not covered: a test case that made it is saved without its mock")
	}
	if over, _ := spans.Overlaps(helloReply.Add(time.Millisecond), pingRead.Add(-time.Millisecond)); over {
		t.Fatal("the spans cover the gap between the two halves: they are wider than the exchanges left out")
	}
	if over, _ := spans.Overlaps(ping2Read.Add(time.Millisecond), pong.Add(2*time.Second)); over {
		t.Fatal("the spans cover time after the carried half's last request: they are wider than its reads")
	}
	if n := mgr.MocksLeftOut(); n != 2 {
		t.Fatalf("the manager counts %d mocks left out, want both halves", n)
	}
}

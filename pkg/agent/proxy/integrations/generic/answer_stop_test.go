package generic

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// stopChunk is a chunk event the relay read and wrote at at.
func stopChunk(dir fakeconn.Direction, b string, seq uint32, at time.Time) chunkEvent {
	return chunkEvent{dir: dir, bytes: []byte(b), connSeq: seq, readAt: at, writtenAt: at}
}

// awaitReturn waits for the parser to return once its events have ended.
func awaitReturn(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(waitBound):
		t.Fatal("the parser did not return once its events ended")
	}
}

// The stop decides a held answer once the parser has taken the chunks the
// merge has ready: the answer's next chunk, in the merge's queue as the
// parser first looks at the stop (it was busy with the answer's first chunk),
// is recorded with it, never cut from it. Both are ready at that look, so
// the parser's select may take either first: the row runs it many times.
func TestV2_TheStopTakesTheAnswerChunksReadyBeforeItDecides(t *testing.T) {
	for _, proven := range []string{"0", "1"} {
		t.Run("proven="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			const runs = 60
			cut := 0
			for i := 0; i < runs; i++ {
				t0 := time.Unix(6000, 0)
				mgr := syncMock.New(zap.NewNop())
				mgr.SetWatermark(neverBehind{})
				mocks := make(chan *models.Mock, 4)
				entered, release := make(chan struct{}), make(chan struct{})
				clears := 0
				sess := &supervisor.Session{Mocks: mocks, Logger: zap.NewNop(), Mgr: mgr, Orphans: &syncMock.Spans{},
					Ctx: syncMock.NewContext(context.Background(), mgr), ClientConnID: "c"}
				// The parser is busy with the answer's first chunk (its
				// hold's clear of pending work) while the next one is
				// queued and the stop begins.
				sess.OnPendingCleared = func() {
					if clears++; clears == 1 {
						close(entered)
						<-release
					}
				}
				events := make(chan chunkEvent, 4)
				done := make(chan error, 1)
				go func() { done <- pairChunkEvents(events, sess, zap.NewNop()) }()
				events <- stopChunk(fakeconn.FromClient, "GET big", 1, t0)
				events <- stopChunk(fakeconn.FromDest, "head", 2, t0.Add(time.Second))
				select {
				case <-entered:
				case <-time.After(waitBound):
					close(release)
					close(events)
					t.Fatal("the parser did not hold the exchange at its answer's first chunk")
				}
				events <- stopChunk(fakeconn.FromDest, "tail", 3, t0.Add(2*time.Second))
				mgr.BacklogBefore(t0.Add(10 * time.Second)) // the stop begins
				close(release)
				var m *models.Mock
				select {
				case m = <-mocks:
				case <-time.After(waitBound):
					close(events)
					t.Fatal("the held exchange was not handed on as the stop began")
				}
				if len(m.Spec.GenericResponses) != 2 {
					cut++
				}
				close(events)
				awaitReturn(t, done)
			}
			if cut != 0 {
				t.Fatalf("the stop handed the exchange on cut %d of %d times, with the answer's next chunk ready as it decided", cut, runs)
			}
		})
	}
}

// An answer teed ahead of its request is held once its request is taken,
// and the stop decides it at once when its reader has taken nothing more:
// how far into the client's stream the parser has read says nothing of the
// dest's. Taken for the dest's, a request shorter than its answer had the
// stop wait for an answer chunk that never comes, to the streams' end. Under
// both switches.
func TestV2_TheStopDecidesAnAnswerTeedAheadOfItsRequestAtOnce(t *testing.T) {
	for _, proven := range []string{"0", "1"} {
		t.Run("proven="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			t0 := time.Unix(8000, 0)
			const answer = "an answer longer than its request"
			destCh := make(chan fakeconn.Chunk, 1)
			dest := fakeconn.New(destCh, nil, nil)
			destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte(answer)}
			if _, err := dest.ReadChunk(); err != nil { // the dest's reader has taken the answer
				t.Fatal(err)
			}
			mgr := syncMock.New(zap.NewNop())
			mgr.SetWatermark(neverBehind{})
			mocks := make(chan *models.Mock, 4)
			sess := &supervisor.Session{Mocks: mocks, Logger: zap.NewNop(), Mgr: mgr, Orphans: &syncMock.Spans{},
				Ctx: syncMock.NewContext(context.Background(), mgr), ClientConnID: "c", DestStream: dest}
			events := make(chan chunkEvent, 4)
			done := make(chan error, 1)
			go func() { done <- pairChunkEvents(events, sess, zap.NewNop()) }()
			ahead := stopChunk(fakeconn.FromDest, answer, 2, t0.Add(time.Second))
			ahead.through = int64(len(answer))
			req := stopChunk(fakeconn.FromClient, "GET", 1, t0)
			req.through = int64(len("GET"))
			events <- ahead
			events <- req
			// The exchange is held: its answer may go on.
			select {
			case m := <-mocks:
				close(events)
				t.Fatalf("the exchange was handed on before the stop: %v", m.Spec.GenericResponses)
			case <-time.After(100 * time.Millisecond):
			}
			mgr.BacklogBefore(t0.Add(10 * time.Second)) // the stop begins
			var m *models.Mock
			select {
			case m = <-mocks:
			case <-time.After(waitBound):
				close(events)
				awaitReturn(t, done)
				t.Fatal("the stop did not decide the answer teed ahead of its request: it waited for a chunk its reader never took")
			}
			if len(m.Spec.GenericRequests) != 1 || len(m.Spec.GenericResponses) != 1 ||
				m.Spec.GenericResponses[0].Message[0].Data != answer {
				t.Fatalf("handed on %v -> %v, want GET -> the whole answer", m.Spec.GenericRequests, m.Spec.GenericResponses)
			}
			if st := mgr.BacklogBefore(t0.Add(10 * time.Second)); st.Pending {
				t.Fatalf("the stop still finds the exchange pending once it was handed on: %q", st.WaitingOn)
			}
			close(events)
			awaitReturn(t, done)
		})
	}
}

// An answer chunk the dest's reader has taken off its stream before the stop
// decides is part of the answer, even while the merge still holds it (it
// waits to place it after a client chunk captured before it): the stop waits
// for it, and hands the exchange on whole once the merge hands it on. Decided
// at the first look with no event ready, the exchange went on cut, and the
// test case over it would be left out. On both deliveries and under both
// switches.
func TestV2_TheStopWaitsForAnAnswerChunkItsReaderTookBeforeItDecides(t *testing.T) {
	t0 := time.Unix(7000, 0)
	// The request is longer than the answer: how far into the client's
	// stream the loop has read says nothing of the dest's.
	req := fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("request-longer-than-its-answer"), ReadAt: t0, ConnSeq: 1}
	head := fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("head"), ReadAt: t0.Add(time.Second), WrittenAt: t0.Add(time.Second), ConnSeq: 2}
	// A client chunk captured between the two, which the relay has not yet
	// said is on its stream or never will be: the merge holds the tail.
	unsaid := fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("x"), ReadAt: t0.Add(1500 * time.Millisecond), ConnSeq: 3}
	tail := fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("tail"), ReadAt: t0.Add(2 * time.Second), WrittenAt: t0.Add(2 * time.Second), ConnSeq: 4}
	plan := []fakeconn.Chunk{req, head, unsaid, tail}
	for _, c := range switchesAndDeliveries {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, c.proven)
			r := startRecording(t, plan, c.viaSyncMock)
			r.put(req)
			r.put(head)
			r.push(tail) // the dest's reader has taken it
			r.awaitMergeWaits()
			r.stop()
			if r.recorded("request-longer-than-its-answer|head", 200*time.Millisecond) {
				r.end()
				t.Fatal("the stop handed the exchange on cut, without the answer chunk its reader had taken")
			}
			r.order.accept(unsaid, 0) // never on its stream: the merge hands the tail on
			if !r.recorded("request-longer-than-its-answer|head,tail", waitBound) {
				r.end()
				t.Fatalf("the exchange was not handed on whole once the merge handed the tail on: %q", pairsOf(r.got))
			}
			got := r.end()
			if !got.returned {
				t.Fatal("the parser did not return once both streams ended")
			}
			if p := pairsOf(got.recorded); strings.Join(p, " ") != "request-longer-than-its-answer|head,tail" || got.leftOut != 0 {
				t.Fatalf("recorded %q and left out %d; want the exchange whole", p, got.leftOut)
			}
		})
	}
}

// The stop decides a held answer with the chunks the parser has; the rest of
// it, read by the relay before the stop's instant but taken by the parser
// after the decision, has its span as soon as the parser has taken it. So the
// stop, which then finds nothing pending, cannot let a test case over it be
// checked before its span exists.
func TestV2_TheRestOfAnAnswerTheStopCutIsSpannedAsTheParserTakesIt(t *testing.T) {
	for _, proven := range []string{"0", "1"} {
		t.Run("proven="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			t0 := time.Unix(6000, 0)
			stop := t0.Add(10 * time.Second)
			mgr := syncMock.New(zap.NewNop())
			mgr.SetWatermark(neverBehind{})
			spans := &syncMock.Spans{}
			mocks := make(chan *models.Mock, 4)
			sess := &supervisor.Session{Mocks: mocks, Logger: zap.NewNop(), Mgr: mgr, Orphans: spans,
				Ctx: syncMock.NewContext(context.Background(), mgr), ClientConnID: "c"}
			// Unbuffered: each send returns once the parser has taken it,
			// and the next once it is done with it.
			events := make(chan chunkEvent)
			done := make(chan error, 1)
			go func() { done <- pairChunkEvents(events, sess, zap.NewNop()) }()
			ended := false
			t.Cleanup(func() {
				if !ended {
					close(events)
					select {
					case <-done:
					case <-time.After(waitBound):
					}
				}
			})
			events <- stopChunk(fakeconn.FromClient, "GET big", 1, t0)
			events <- stopChunk(fakeconn.FromDest, "head", 2, t0.Add(time.Second))
			events <- chunkEvent{dir: fakeconn.FromDest} // a look past the head: it is held
			mgr.BacklogBefore(stop)                      // the stop begins
			select {
			case m := <-mocks:
				if len(m.Spec.GenericResponses) != 1 {
					t.Fatalf("handed on at the stop with %d answer chunks, want the 1 read", len(m.Spec.GenericResponses))
				}
			case <-time.After(waitBound):
				t.Fatal("the held exchange was not handed on as the stop began")
			}
			tailAt := t0.Add(2 * time.Second) // read before the stop's instant
			events <- stopChunk(fakeconn.FromDest, "tail", 3, tailAt)
			events <- chunkEvent{dir: fakeconn.FromDest} // the parser is done with the tail
			st := mgr.BacklogBefore(stop)
			over, _ := spans.Overlaps(tailAt, tailAt)
			if !st.Pending && !over {
				t.Fatal("the stop finds nothing pending while the rest of the answer it cut short has no span: a test case over it is checked and kept before the span exists")
			}
			if !over {
				t.Fatalf("the parser took the rest of the answer the stop cut short and recorded no span over it (the stop waits on %q)", st.WaitingOn)
			}
			// An exchange after the stop is recorded as any is: its
			// request starts it, and its answer is decided at the next
			// look that finds no more of it.
			events <- stopChunk(fakeconn.FromClient, "GET small", 5, t0.Add(11*time.Second))
			events <- stopChunk(fakeconn.FromDest, "small", 6, t0.Add(12*time.Second))
			select {
			case m := <-mocks:
				assertExchange(t, m, []string{"GET small"}, []string{"small"})
			case <-time.After(waitBound):
				t.Fatal("the exchange after the stop was not handed on")
			}
			ended = true
			close(events)
			awaitReturn(t, done)
			if n := mgr.MocksLeftOut(); n != 1 {
				t.Fatalf("%d left out, want the rest of the answer counted once", n)
			}
		})
	}
}

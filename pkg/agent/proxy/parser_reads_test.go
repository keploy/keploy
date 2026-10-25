package proxy

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// readsIdleGrace is the idle grace the tests give parserReads.
const readsIdleGrace = time.Second

// connRecorder stands in for the connection's syncMock.UnrecordedConn and for
// where its closed spans go: it keeps the instants it is noted at, the closed
// spans left out, and its first stop.
type connRecorder struct {
	stopping bool
	stopped  bool
	notes    []time.Time
	spans    []string
	stops    int
}

func (c *connRecorder) Note(at time.Time) { c.notes = append(c.notes, at) }

func (c *connRecorder) Stop(time.Time) bool {
	if c.stopped {
		return false
	}
	c.stopped = true
	return true
}

func (c *connRecorder) StopFromNextBytes() bool {
	c.stops++
	if c.stopped {
		return false
	}
	c.stopped = true
	return true
}

func (c *connRecorder) leftOut(start, end time.Time) { c.spans = append(c.spans, span(start, end)) }

// span names the closed span [start, end] by its ends' offsets from readsEpoch.
func span(start, end time.Time) string {
	return fmt.Sprintf("[%v, %v]", start.Sub(readsEpoch), end.Sub(readsEpoch))
}

// readsStreams stands in for the parser's client stream (how far it has
// read, and whether it can say so exactly) and for the relay's count of what
// it has teed to each stream.
type readsStreams struct {
	read    atomic.Int64
	inexact atomic.Bool
	teed    [2]atomic.Int64
}

func (s *readsStreams) ProcessedThrough() (int64, bool) {
	if s.inexact.Load() {
		return 0, false
	}
	return s.read.Load(), true
}

func (s *readsStreams) teedOf(dir fakeconn.Direction) int64 { return s.teed[dir].Load() }

// readsUnderTest is a parserReads over stand-ins, and the relay's side of it:
// carry tees a chunk to the parser (or not) and tells of it.
type readsUnderTest struct {
	*parserReads
	conn    *connRecorder
	streams *readsStreams
}

func newReadsUnderTest() *readsUnderTest {
	c := &connRecorder{}
	s := &readsStreams{}
	p := newParserReads(c, c.leftOut, nil, func() bool { return c.stopping }, readsIdleGrace, nil)
	p.follow(s, s.teedOf)
	return &readsUnderTest{parserReads: p, conn: c, streams: s}
}

// tee tees n bytes of dir's stream, and returns where they end.
func (r *readsUnderTest) tee(dir fakeconn.Direction, n int64) int64 {
	return r.streams.teed[dir].Add(n)
}

// carry tees n bytes of dir's stream, read at ms, and tells of them.
func (r *readsUnderTest) carry(dir fakeconn.Direction, ms int, n int64) {
	r.carried(dir, msAfter(ms), true, r.tee(dir, n))
}

var readsEpoch = time.Unix(1_700_000_000, 0)

func msAfter(ms int) time.Time { return readsEpoch.Add(time.Duration(ms) * time.Millisecond) }

func notedSince(c *connRecorder, before int) []time.Time { return c.notes[before:] }

// A request the parser read before it returned is recorded, whether the relay
// told of it before the return or after: the order in which the relay and the
// parser got to it decides nothing. Told of after the return, it was taken for
// the connection's next bytes, and the span it opened left out the test cases
// in flight with the parser's last request, whose mocks it had recorded.
func TestParserReads_ARequestTheParserReadIsRecordedWheneverTheRelayTellsOfIt(t *testing.T) {
	for _, toldAfter := range []bool{false, true} {
		r := newReadsUnderTest()
		through := r.tee(fakeconn.FromClient, 5)
		if !toldAfter {
			r.carried(fakeconn.FromClient, msAfter(0), true, through)
		}
		r.streams.read.Store(5)
		before := len(r.conn.notes)
		r.parserReturned()
		if toldAfter {
			r.carried(fakeconn.FromClient, msAfter(0), true, through)
		}
		if got := notedSince(r.conn, before); len(got) != 0 || len(r.conn.spans) != 0 {
			t.Errorf("told of after the return: %v; noted at %v and left out %v: a request the parser read leaves out test cases whose mocks were recorded", toldAfter, got, r.conn.spans)
		}
	}
}

// A request teed to the parser that it never read is left out, from when it
// was read, whether the relay told of it before the return or after. Told of
// before the return, it was noted while the connection was still recorded,
// and no span ever covered it: the test cases over it were saved without
// their mocks.
func TestParserReads_ARequestTheParserNeverReadIsLeftOutWheneverTheRelayTellsOfIt(t *testing.T) {
	want := []string{span(msAfter(10), msAfter(10).Add(readsIdleGrace))}
	for _, toldAfter := range []bool{false, true} {
		r := newReadsUnderTest()
		r.carry(fakeconn.FromClient, 0, 5)
		unread := r.tee(fakeconn.FromClient, 4)
		if !toldAfter {
			r.carried(fakeconn.FromClient, msAfter(10), true, unread)
		}
		r.streams.read.Store(5)
		r.parserReturned()
		if toldAfter {
			r.carried(fakeconn.FromClient, msAfter(10), true, unread)
		}
		if fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
			t.Errorf("told of after the return: %v; left out %v, want %v: from the unread request to the idle grace past it", toldAfter, r.conn.spans, want)
		}
	}
}

// A request a parser that reads ahead of itself took off its client stream,
// and whose loop never got to it, is left out as one it never read: what
// counts is what the parser processed, not what its reader took.
func TestParserReads_ARequestReadAheadAndNeverProcessedIsLeftOut(t *testing.T) {
	c := &connRecorder{}
	s := &readsStreams{}
	ch := make(chan fakeconn.Chunk, 2)
	client := fakeconn.New(ch, nil, nil)
	p := newParserReads(c, c.leftOut, nil, func() bool { return c.stopping }, readsIdleGrace, nil)
	p.follow(client, s.teedOf)
	for i, req := range []string{"first", "next"} {
		at := msAfter(10 * i)
		ch <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(req), ReadAt: at}
		p.carried(fakeconn.FromClient, at, true, s.teed[fakeconn.FromClient].Add(int64(len(req))))
	}
	// Its reader takes both requests; its loop is done with the first.
	var ahead []fakeconn.Chunk
	for range 2 {
		got, err := client.ReadAhead()
		if err != nil {
			t.Fatalf("ReadAhead: %v", err)
		}
		ahead = append(ahead, got)
	}
	client.MarkProcessed(ahead[0])
	p.parserReturned()
	want := []string{span(msAfter(10), msAfter(10).Add(readsIdleGrace))}
	if fmt.Sprint(c.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v: from the request read ahead and never processed to the idle grace past it", c.spans, want)
	}
}

// Where the client stream cannot say exactly how far the parser is done with
// it (fakeconn.FakeConn.ProcessedThrough), the last exact answer holds: a
// request past it is left out, not taken for read on a guess, and one before
// it, told of after the return, is not.
func TestParserReads_WhereTheStreamCannotSayHowFarTheParserIsTheLastExactAnswerHolds(t *testing.T) {
	r := newReadsUnderTest()
	first := r.tee(fakeconn.FromClient, 5)
	r.streams.read.Store(5)
	r.carry(fakeconn.FromClient, 10, 4)
	r.streams.read.Store(9)
	r.streams.inexact.Store(true)
	r.parserReturned()
	r.carried(fakeconn.FromClient, msAfter(0), true, first)
	want := []string{span(msAfter(10), msAfter(10).Add(readsIdleGrace))}
	if fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v: the stream said exactly only that the first request was read", r.conn.spans, want)
	}
}

// What the parser did not read is left out for as long as the connection
// carried it: a gap longer than the idle grace between two unread requests
// ends one span and starts the next. A span from the first unread request to
// the return covered the gaps too, however long the connection had idled.
func TestParserReads_TheRequestsTheParserDidNotReadAreLeftOutOnlyWhileTheConnectionCarriedThem(t *testing.T) {
	r := newReadsUnderTest()
	r.carry(fakeconn.FromClient, 0, 5) // read
	r.carry(fakeconn.FromClient, 100, 4)
	r.carry(fakeconn.FromClient, 600, 4) // within the grace of the last
	r.carry(fakeconn.FromClient, 9000, 4)
	r.streams.read.Store(5)
	r.parserReturned()
	want := []string{
		span(msAfter(100), msAfter(600).Add(readsIdleGrace)),
		span(msAfter(9000), msAfter(9000).Add(readsIdleGrace)),
	}
	if fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v: one span per run of unread requests, none over the idle gap between them", r.conn.spans, want)
	}
}

// A request of which the parser read a part is not recorded: what it did not
// read leaves the connection out from that request.
func TestParserReads_ARequestTheParserReadInPartIsNotRecorded(t *testing.T) {
	r := newReadsUnderTest()
	r.carry(fakeconn.FromClient, 0, 10)
	r.streams.read.Store(4)
	r.parserReturned()
	if want := span(msAfter(0), msAfter(0).Add(readsIdleGrace)); len(r.conn.spans) != 1 || r.conn.spans[0] != want {
		t.Errorf("left out %v, want %s: the parser read 4 of the request's 10 bytes", r.conn.spans, want)
	}
}

// Server bytes the parser returned without reading leave nothing out, told of
// before the return or after: they are the server's own, or the rest of an
// answer the parser left out and reported itself, as the floor rules for the
// server bytes it drops between requests (Session.dropped). A server's
// unprompted goodbye (MySQL's ERR at wait_timeout) before the app closes the
// connection left out the test cases in flight then.
func TestParserReads_ServerBytesTheParserDidNotReadLeaveNothingOut(t *testing.T) {
	for _, toldAfter := range []bool{false, true} {
		r := newReadsUnderTest()
		r.carry(fakeconn.FromClient, 0, 5)
		r.streams.read.Store(5)
		goodbye := r.tee(fakeconn.FromDest, 7)
		if !toldAfter {
			r.carried(fakeconn.FromDest, msAfter(10), true, goodbye)
		}
		before := len(r.conn.notes)
		r.parserReturned()
		if toldAfter {
			r.carried(fakeconn.FromDest, msAfter(10), true, goodbye)
		}
		if got := notedSince(r.conn, before); len(got) != 0 || len(r.conn.spans) != 0 {
			t.Errorf("told of after the return: %v; noted at %v and left out %v for server bytes the parser did not read, want nothing", toldAfter, got, r.conn.spans)
		}
	}
}

// What the connection carries after the parser returned is not recorded:
// every chunk teed after the return, or refused by a tee, in either
// direction, notes the connection's traffic from when it was read, as before.
func TestParserReads_EveryChunkAfterTheReturnIsNoted(t *testing.T) {
	r := newReadsUnderTest()
	r.carry(fakeconn.FromClient, 0, 5)
	r.streams.read.Store(5)
	before := len(r.conn.notes)
	r.parserReturned()
	r.carry(fakeconn.FromClient, 30, 4)
	r.carry(fakeconn.FromDest, 35, 4)
	r.carried(fakeconn.FromClient, msAfter(40), false, r.streams.teed[fakeconn.FromClient].Load())
	got := notedSince(r.conn, before)
	if len(got) != 3 || !got[0].Equal(msAfter(30)) || !got[1].Equal(msAfter(35)) || !got[2].Equal(msAfter(40)) {
		t.Errorf("noted at %v after the return, want %v, %v and %v: none of them was recorded", got, msAfter(30), msAfter(35), msAfter(40))
	}
}

// A connection stopped before its parser returned (a capture hole, the
// parser's stop on an exchange, its retirement) is left out from that stop by
// the stop's own span: every chunk is noted, even one its parser read. The
// requests it never read that were captured before the stop are in no span
// of the stop's, and the return leaves them out; those captured after it are
// the stop's.
func TestParserReads_AStopBeforeTheReturnLeavesTheRequestsBeforeItToTheReturn(t *testing.T) {
	r := newReadsUnderTest()
	r.carry(fakeconn.FromClient, 0, 5)  // read
	r.carry(fakeconn.FromClient, 10, 4) // unread, before the stop
	r.stop(msAfter(50))
	r.carry(fakeconn.FromClient, 60, 4) // unread, after it
	r.streams.read.Store(5)
	before := len(r.conn.notes)
	r.parserReturned()
	r.carried(fakeconn.FromClient, msAfter(0), true, 5)
	if want := []string{span(msAfter(10), msAfter(10).Add(readsIdleGrace))}; fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v: the unread request captured before the stop, and not the one after it", r.conn.spans, want)
	}
	if got := notedSince(r.conn, before); len(got) != 1 || !got[0].Equal(msAfter(0)) {
		t.Errorf("noted at %v after the return, want %v: after an earlier stop every chunk is noted", got, msAfter(0))
	}
}

// A parser that returns as the recording stops leaves out the requests it
// never read that were captured while the recording ran: the test cases over
// them were recorded. What the connection carried from the recording's stop
// on is no loss: the connection is torn down with the recording.
func TestParserReads_TheRequestsBeforeTheRecordingsStopAreLeftOutAndNoneAfter(t *testing.T) {
	r := newReadsUnderTest()
	r.carry(fakeconn.FromClient, 0, 5) // read
	r.streams.read.Store(5)
	r.carry(fakeconn.FromClient, 10, 4) // unread, while the recording ran
	r.recordingStopped(msAfter(50))
	r.carry(fakeconn.FromClient, 60, 4)   // unread, as it stopped
	late := r.tee(fakeconn.FromClient, 4) // unread, as it stopped, told of after the return
	r.conn.stopping = true
	r.parserReturned()
	r.carried(fakeconn.FromClient, msAfter(70), true, late)
	if want := []string{span(msAfter(10), msAfter(10).Add(readsIdleGrace))}; fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v", r.conn.spans, want)
	}
}

// Before the parser returns, every chunk is noted as the connection's traffic
// (the stops before the return go by it), and the requests the parser has
// read are let go of as more come: what is kept is bounded by what is in the
// parser's stream, not by what the connection carried.
func TestParserReads_BeforeTheReturnEveryChunkIsNotedAndTheReadOnesLetGo(t *testing.T) {
	r := newReadsUnderTest()
	for i := 1; i <= 1000; i++ {
		r.carry(fakeconn.FromClient, i, 1)
		r.carry(fakeconn.FromDest, i, 1)
		r.streams.read.Store(int64(i))
	}
	if len(r.conn.notes) != 2000 {
		t.Errorf("%d chunks noted before the return, want all 2000", len(r.conn.notes))
	}
	if kept := len(r.unread) - r.head; kept > 1 || len(r.unread) > 2 {
		t.Errorf("%d requests kept in a backing array of %d, want at most the one not yet seen read", kept, len(r.unread))
	}
	r.parserReturned()
	r.parserReturned()
	if r.conn.stops != 1 || len(r.conn.spans) != 0 {
		t.Errorf("the connection was stopped %d times and left out %v, want one stop and nothing left out: the parser read every request", r.conn.stops, r.conn.spans)
	}
}

// A request the parser never read, captured before a stop that came before the
// return, is left out by the return whether the relay told of it before the
// return or after: the stop's own span follows the connection from the stop
// on and does not reach back to it.
func TestParserReads_ARequestBeforeAnEarlierStopIsLeftOutWheneverTheRelayTellsOfIt(t *testing.T) {
	want := []string{span(msAfter(10), msAfter(10).Add(readsIdleGrace))}
	for _, toldAfter := range []bool{false, true} {
		r := newReadsUnderTest()
		r.carry(fakeconn.FromClient, 0, 5) // read
		unread := r.tee(fakeconn.FromClient, 4)
		if !toldAfter {
			r.carried(fakeconn.FromClient, msAfter(10), true, unread)
		}
		r.stop(msAfter(50))
		r.streams.read.Store(5)
		r.parserReturned()
		if toldAfter {
			r.carried(fakeconn.FromClient, msAfter(10), true, unread)
		}
		if fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
			t.Errorf("told of after the return: %v; left out %v, want %v", toldAfter, r.conn.spans, want)
		}
	}
}

// A recording that is stopping when the parser returns, before its stop was
// stamped, is taken as stopped then: what the parser had not read is left out.
func TestParserReads_ARecordingStoppingButNotYetStampedStopsAtTheReturn(t *testing.T) {
	r := newReadsUnderTest()
	r.carry(fakeconn.FromClient, 0, 5)
	r.carry(fakeconn.FromClient, 10, 4)
	r.streams.read.Store(5)
	r.conn.stopping = true
	r.parserReturned()
	if want := []string{span(msAfter(10), msAfter(10).Add(readsIdleGrace))}; fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v", r.conn.spans, want)
	}
	if r.recordingStopAt.IsZero() {
		t.Error("the recording's stop was not stamped at the return")
	}
}

// With both a stop and the recording's stop before the return, the earlier
// bounds what the return leaves out.
func TestParserReads_TheEarlierOfTheTwoStopsBoundsTheReturn(t *testing.T) {
	r := newReadsUnderTest()
	r.carry(fakeconn.FromClient, 0, 5) // read
	r.streams.read.Store(5)
	r.carry(fakeconn.FromClient, 10, 4) // unread, before both
	r.recordingStopped(msAfter(30))
	r.carry(fakeconn.FromClient, 40, 4) // unread, after the recording's stop
	r.stop(msAfter(50))
	r.conn.stopping = true
	r.parserReturned()
	if want := []string{span(msAfter(10), msAfter(10).Add(readsIdleGrace))}; fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v: only the request before the recording's stop", r.conn.spans, want)
	}
}

// A parser behind its stream has the chunks it has read let go of from the
// front, the rest kept in order, so what it never read is left out from the
// first of it however far behind it ran.
func TestParserReads_AParserBehindItsStreamKeepsWhatItHasNotReadInOrder(t *testing.T) {
	r := newReadsUnderTest()
	for i := 1; i <= 10; i++ {
		r.carry(fakeconn.FromClient, i*10, 1)
	}
	r.streams.read.Store(6)
	r.carry(fakeconn.FromClient, 110, 1) // lets go of the six read, keeps four
	if kept := len(r.unread) - r.head; kept != 5 {
		t.Fatalf("%d chunks kept, want the 5 the parser has not read", kept)
	}
	r.parserReturned()
	if want := []string{span(msAfter(70), msAfter(110).Add(readsIdleGrace))}; fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v: from the first chunk the parser did not read", r.conn.spans, want)
	}
}

// A run whose stamps step back (the wall clock moved) is covered whole.
func TestParserReads_ARunCoversItsChunksWhicheverWayTheirStampsLie(t *testing.T) {
	r := newReadsUnderTest()
	r.carry(fakeconn.FromClient, 1000, 4)
	r.carry(fakeconn.FromClient, 500, 4)
	r.parserReturned()
	if want := []string{span(msAfter(500), msAfter(1000).Add(readsIdleGrace))}; fmt.Sprint(r.conn.spans) != fmt.Sprint(want) {
		t.Errorf("left out %v, want %v", r.conn.spans, want)
	}
}

package proxy

import (
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
)

// parserReads decides what a connection left unrecorded once its parser's Run
// has returned (on its own, or retired after a stop), by what the parser read,
// not by when the relay got to tell of a chunk.
//
// A parser that returns on its own recorded the exchanges it read. What it
// did not record are the requests it never read: the client bytes past where
// its client stream stood when it returned (fakeconn.FakeConn.ReadThrough).
// The test cases over them are left out. The server bytes past where its
// server stream stood are not a loss here: they are the server's own, or the
// rest of an answer the parser left out and reported itself, by the rule the
// floor applies to the server bytes it drops between exchanges
// (supervisor.Session.dropped, MySQL's ERR at wait_timeout among them).
//
// "Read" is taken for "recorded", and losses fall outside what this sees,
// each its own fix, among them: a mock the session refuses because the
// parser's context is done is dropped without being reported (Session.EmitMock
// after the recording stopped or the parser was retired, or from a helper
// goroutine of the parser after its Run returned, whose context Run cancels
// on the way out); and a parser that returns nil in the middle of an exchange
// on a cancelled context without reporting it (the MySQL recorder) leaves
// that exchange's request read and its answer unread. The first wants
// EmitMock to report what it refuses (ReportLeftOut); the second, the parser
// to report the exchange it stops on.
//
// The spans the return records are closed spans, recorded when it comes. Proxy
// mode checks each test case as it streams it, so a span recorded late, from
// a parser that returned far behind its queue, covers only the test cases not
// streamed yet (Session.RecordOrphanWindow says the same of every span
// recorded after the fact).
//
// Everything the connection carries after the return is left out, as before
// (StopFromNextBytes), except the chunks that came before it: those are
// judged as at the return.
//
// The order in which the relay and the parser get to a chunk decides nothing.
// The relay tells of a chunk (carried, from relay.Config.OnCarried) once it
// has teed it, and the parser can have read the chunk, and returned, by then.
// So a chunk told of after the return is not, for that, one the parser
// missed. Nor is one told of before the return one it read: the parser can
// return with requests in its stream it never took. Taken by that order, the
// first left out the test cases in flight with the parser's last request,
// whose mocks it had recorded, and the second saved the test cases over the
// requests it never took without their mocks.
//
// So until the parser returns, it keeps the client chunks the parser may yet
// read, by where each ends in the stream and when it was read, and lets go of
// those the parser has read. When it returns, the ones past where the stream
// stood are left out in closed spans over the runs of them: from a run's
// first chunk to the idle grace past its last, a gap longer than the grace
// ending one run and starting the next. A test case over an unread request
// has that request's capture time in its window, so it overlaps the span; the
// grace past the last chunk keeps one exchange's chunks in one span. An idle
// stretch between the runs and the return is not covered. One mutex orders
// every chunk it is told of, and every stop, against the return.
//
// Two instants bound what the return leaves out. A connection stopped before
// its parser returned (a capture hole, the parser's own stop on an exchange,
// its death or retirement: stop) is left out from that stop by the stop's own
// span, and its requests the parser never read that were captured before the
// stop are left out here. A recording that is stopping
// (Session.RecordingStopping) tears the connection down with it, and what it
// carries from the recording's stop on is no loss; the requests captured
// before it, while the recording ran, still are.
type parserReads struct {
	mu        sync.Mutex
	conn      unrecordedConn
	leftOut   func(start, end time.Time)
	stopping  func() bool
	idleGrace time.Duration
	logger    *zap.Logger

	// client is the parser's client stream, and teed how much of each stream
	// the relay has teed to the parser (relay.Relay.Teed). Set by follow
	// before the relay runs.
	client readStream
	teed   func(fakeconn.Direction) int64

	// stopAt is the connection's first stop (stop), and recordingStopAt when
	// its recording stopped (recordingStopped). Zero until then; times are on
	// the wall clock alone (wall).
	stopAt, recordingStopAt time.Time

	// returned is set by parserReturned. fromNext says the return was the
	// connection's first stop (StopFromNextBytes). read is where the client
	// stream stood as the parser returned. teedAt is how much of each
	// stream had been teed to it at the return. cutoff is when what the
	// return leaves out ends: the stop that came first, or the recording's;
	// zero when neither did.
	returned bool
	fromNext bool
	read     int64
	teedAt   [2]int64
	cutoff   time.Time

	// unread[head:] holds, until the parser returns, the client chunks it had
	// not read when last looked at, in stream order.
	unread []carriedChunk
	head   int

	// live, once set (listLive), lists the connection live at each client
	// chunk carried from then on that the parser had not read: its parser was
	// retired before it recorded anything of it (mockedNote.retired). Read
	// without mu: carried runs on the relay's forwarder for every chunk.
	live atomic.Pointer[liveListing]
}

// liveListing is how parserReads lists its connection live (listLive): list
// lists it, and from is where the parser's client stream stood as it was
// set. A client chunk teed to the parser and ending there or before it was
// read by the parser before it was retired; listLive's own call of list
// lists it, so carried does not list it again.
type liveListing struct {
	list func()
	from int64
}

// unrecordedConn is the connection's syncMock.UnrecordedConn.
type unrecordedConn interface {
	Note(at time.Time)
	Stop(at time.Time) bool
	StopFromNextBytes() bool
}

// readStream is how far a parser is through one of its streams
// (fakeconn.FakeConn).
type readStream interface {
	ReadThrough() int64
}

// carriedChunk is a client chunk the parser may not have read: where it ends
// in the stream, and when the relay read it.
type carriedChunk struct {
	through int64
	at      time.Time
}

// wall is t on the wall clock alone, so that times stamped from the clock
// (time.Now) and times rebuilt from a stamp (time.Unix) compare one way.
func wall(t time.Time) time.Time { return t.Round(0) }

// newParserReads follows conn. leftOut records a closed span of traffic the
// parser did not record (Session.RecordOrphanWindow); stopping says whether
// the recording is stopping (Session.RecordingStopping); idleGrace is how
// long a run of unread chunks goes on past its last one
// (syncMock.UnrecordedIdleGrace, as conn's followed spans).
func newParserReads(conn unrecordedConn, leftOut func(start, end time.Time), stopping func() bool, idleGrace time.Duration, logger *zap.Logger) *parserReads {
	return &parserReads{conn: conn, leftOut: leftOut, stopping: stopping, idleGrace: idleGrace, logger: logger}
}

// follow sets the parser's client stream and the relay's count of what it has
// teed. Call it before the relay runs.
func (p *parserReads) follow(client readStream, teed func(fakeconn.Direction) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client, p.teed = client, teed
}

// stop is the session's OnStop: the connection stopped being recorded at
// `at`, and its span follows from there (conn.Stop). Ordered against the
// return, so the return knows whether a stop came first, and when.
func (p *parserReads) stop(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn.Stop(at) {
		p.stopAt = wall(at)
	}
}

// recordingStopped says the recording stopped at `at`.
func (p *parserReads) recordingStopped(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.recordingStopAt.IsZero() {
		p.recordingStopAt = wall(at)
	}
}

// carried is relay.Config.OnCarried: the connection carried a chunk, read at
// `at`, teed to the parser or not, ending at `through` in its stream when it
// was.
func (p *parserReads) carried(dir fakeconn.Direction, at time.Time, teed bool, through int64) {
	if dir == fakeconn.FromClient {
		if l := p.live.Load(); l != nil && (!teed || through > l.from) {
			l.list()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.returned {
		if dir == fakeconn.FromClient && teed && p.client != nil {
			p.keep(through, at)
		}
		p.conn.Note(at)
		return
	}
	if teed && dir <= fakeconn.FromDest && through <= p.teedAt[dir] {
		// Teed before the return, told of after it: judged as at the return,
		// whatever stop came first. After an earlier stop it is the
		// connection's traffic too.
		if dir == fakeconn.FromClient && through > p.read && p.beforeCutoff(at) {
			p.leaveOut([]time.Time{at})
		}
		if !p.fromNext {
			p.conn.Note(at)
		}
		return
	}
	// Teed after the return, or refused: not recorded.
	p.conn.Note(at)
}

// listLive lists the connection live with live (mockedNote.retired's func),
// once now, and at each client chunk carried from then on that the parser
// had not read (carried): the relay tells of a chunk after it teed it, so
// the chunk the parser was retired on can be told of after this, and it is
// listed by the call here, not again. A chunk the parser had read only part
// of is listed again, which the agent lists once for its test. A nil live
// lists nothing.
func (p *parserReads) listLive(live func()) {
	if live == nil {
		return
	}
	var from int64
	p.mu.Lock()
	if p.client != nil {
		from = p.client.ReadThrough()
	}
	p.mu.Unlock()
	// Stored first: a chunk the relay carries between the two is listed by
	// carried, not lost.
	p.live.Store(&liveListing{list: live, from: from})
	live()
}

// keep adds a client chunk the parser may yet read, and lets go of those it
// has read. The chunks let go of are skipped (head) and dropped from the
// backing array only once they are half of it, so a parser far behind a deep
// queue costs no copy per chunk.
func (p *parserReads) keep(through int64, at time.Time) {
	read := p.client.ReadThrough()
	for p.head < len(p.unread) && p.unread[p.head].through <= read {
		p.head++
	}
	if p.head == len(p.unread) {
		// Nothing kept. An array a burst grew is let go of, as the tee does,
		// so it is not held for the connection's life; a small one is reused,
		// so a parser that keeps up costs no allocation per chunk.
		if cap(p.unread) > 64 {
			p.unread = nil
		} else {
			p.unread = p.unread[:0]
		}
		p.head = 0
	} else if p.head > len(p.unread)/2 {
		p.unread, p.head = p.unread[:copy(p.unread, p.unread[p.head:])], 0
	}
	p.unread = append(p.unread, carriedChunk{through: through, at: at})
}

// beforeCutoff reports whether a chunk read at `at` came before the stop or
// the recording's stop that bounds what the return leaves out.
func (p *parserReads) beforeCutoff(at time.Time) bool {
	return p.cutoff.IsZero() || wall(at).Before(p.cutoff)
}

// parserReturned says the parser's Run has returned: the parser returned on
// its own, or it was retired, after a stop, and may still be reading. Call it
// before the tees are paused: a chunk refused by a paused tee is told of as
// not teed, which counts as after the return.
func (p *parserReads) parserReturned() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.returned {
		return
	}
	p.returned = true
	if p.client != nil {
		p.read = p.client.ReadThrough()
	}
	if p.teed != nil {
		p.teedAt = [2]int64{fakeconn.FromClient: p.teed(fakeconn.FromClient), fakeconn.FromDest: p.teed(fakeconn.FromDest)}
	}
	unread := p.unread[p.head:]
	p.unread, p.head = nil, 0
	p.fromNext = p.conn.StopFromNextBytes()
	if !p.fromNext {
		p.cutoff = p.stopAt
	}
	if p.stopping() && p.recordingStopAt.IsZero() {
		// Stopping, and not told yet when it stopped: by now.
		p.recordingStopAt = wall(time.Now())
	}
	if !p.recordingStopAt.IsZero() && (p.cutoff.IsZero() || p.recordingStopAt.Before(p.cutoff)) {
		p.cutoff = p.recordingStopAt
	}
	var at []time.Time
	for _, c := range unread {
		if c.through > p.read && p.beforeCutoff(c.at) {
			at = append(at, c.at)
		}
	}
	p.leaveOut(at)
}

// leaveOut records the closed spans over the runs of the unread client chunks
// read at `at`, in stream order (so in the order the relay read them).
func (p *parserReads) leaveOut(at []time.Time) {
	spans := 0
	for k := 0; k < len(at); {
		start, last := wall(at[k]), wall(at[k])
		for k++; k < len(at) && !wall(at[k]).After(last.Add(p.idleGrace)); k++ {
			// The relay's stamps follow the wall clock, which can step back:
			// a run's span covers every chunk in it whichever way they lie.
			if t := wall(at[k]); t.After(last) {
				last = t
			} else if t.Before(start) {
				start = t
			}
		}
		p.leftOut(start, last.Add(p.idleGrace))
		spans++
	}
	if spans > 0 && p.logger != nil {
		p.logger.Debug("left out the requests a connection's parser returned without recording",
			zap.Int("chunks", len(at)), zap.Int("spans", spans), zap.Time("from", at[0]))
	}
}

package supervisor

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// exchanges is NextRequest's state. Only the parser's goroutine touches it.
type exchanges struct {
	// kind is the kind of mock the parser records, for what the floor drops.
	kind models.Kind
	// report is dropped, made once, at the first request, rather than a
	// method value that every request's floor would allocate.
	report func(fakeconn.Dropped)
}

// leftOutUnanswered is why NextRequest's floor reports the server bytes it
// drops at the start of a connection the capture joined mid-way. The text
// before the colon is the cause, which keys the WARN's limit (leftOutCause).
const leftOutUnanswered = leftOutUnansweredCause + ": they were captured before the first request of a connection " +
	"whose capture began after it was established, so the request they answer was sent before the capture began, " +
	"and was not captured (the recording started, a TLS hook attached, or the agent restarted, while it was in flight)"

// NextRequest is how a request/response parser starts each exchange: it waits
// for the client's next request and returns that request's first chunk, not
// taken (fakeconn.FakeConn.Peek): the parser reads its request from the client
// stream as it would without it. It sets the server stream's floor to that
// chunk (fakeconn.FakeConn.DropBefore), for every request, so the rule holds in
// one place for every parser that reads its exchanges this way:
//
//	server bytes captured before the first chunk of a request are not its
//	answer.
//
// The parser never reads them, however late they reach it (the two directions
// are teed independently, so a server chunk captured before the request can
// arrive after it). Order is the chunks' capture sequence
// (fakeconn.Chunk.ConnSeq), never their times: a request whose first chunk is
// not numbered is refused with fakeconn.ErrUnnumbered. What the floor drops is
// reported by dropped.
//
// The floor is the last one set before the server's stream is next read, so a
// parser that reads on through client chunks that start no request (the MySQL
// recorder, through the client's packets inside an exchange it left out) calls
// this for each, and the one in force is its next request's. That recorder
// cuts the rest of a response it could not frame off by capture time
// (fakeconn.FakeConn.SkipThrough), which takes the stream before the floor:
// the floor then drops only what that cut left. The first floor this sets is
// the first request's (fakeconn.FakeConn.DropBefore): what was captured
// before it is the server stream's start, which is reported (dropped) however it
// leaves the stream, the floor's read or that cut, and nothing captured after
// it is in the start's run, whatever floor is in force then. A parser that
// returns without reading the server's stream past its start has
// EndExchanges read it on.
//
// answerUnread says the parser goes on to this request with an earlier answer
// still to read: the server's bytes after what it has read of that answer are
// still that answer's, though they were captured before this request, so the
// floor stays where it is. MySQL's held PREPARE is the one parser that passes
// true, while it holds one: the EOF after its definitions, when the server
// sends one, comes before the client's next command, and which it is, EOF or
// the next command's answer, is told only by the next packet the server sends,
// after a command with an answer. It returns the client stream's errors as
// they are: io.EOF when the stream ends.
func (s *Session) NextRequest(kind models.Kind, answerUnread bool) (fakeconn.Chunk, error) {
	c, err := s.ClientStream.Peek()
	if err != nil {
		return fakeconn.Chunk{}, err
	}
	if c.ConnSeq == 0 {
		return fakeconn.Chunk{}, fakeconn.ErrUnnumbered
	}
	if s.exchanges.report == nil {
		s.exchanges.report = s.dropped
	}
	s.exchanges.kind = kind
	if !answerUnread {
		s.DestStream.DropBefore(c.ConnSeq, s.exchanges.report)
	}
	return c, nil
}

// dropped is told of each run of server bytes the floor drops (NextRequest),
// once. One rule decides what it is. The run of the server stream's start
// (AtStart: the bytes captured before the connection's first request), on a
// connection the capture joined mid-way (JoinedMidConnection), answers a
// request the capture does not have: one in flight when the capture began. It
// is reported once, through ReportLeftOut, as kind's, over the run's capture
// times (with none, it is counted with no span), however late the parser
// reads it, the recording's stop included: it was captured, and it is not
// recorded, as is every exchange ReportLeftOut reports. Any other run is
// dropped with a Debug line, with nothing left out for it here. It answers no
// request at all: on a connection captured from its first byte, nothing was
// sent before the capture began, so bytes the server sent before the first
// request (an HTTP server's 408 on a connection that idled out as the client
// sent it) are the server's own, as are bytes captured after a request the
// parser read and before the request after it (MySQL's ERR at wait_timeout or
// shutdown, an idle-close 408). Or it is what is left of an answer its parser
// stopped reading and left out, which the parser reported itself as it did
// (the MySQL recorder, for a response it cannot frame: it skips the rest of
// that response by capture time before the floor applies, so the floor drops
// only what of it the capture numbered before the next command and stamped
// after it). The start's run holds nothing captured after the first request,
// whichever way it leaves the stream (fakeconn.Dropped.AtStart): when that
// recorder finds the first command's response unframable before it reads a
// byte of the server's stream, its skip takes the answer in flight and that
// response together, and the run ends at the response's first chunk, so the
// answer in flight is reported over its own bytes, as when the floor drops it,
// and the response is the exchange the recorder leaves out itself.
func (s *Session) dropped(d fakeconn.Dropped) {
	if !s.JoinedMidConnection || !d.AtStart {
		if s.Logger != nil {
			s.Logger.Debug("dropped server bytes captured before the request after them: the server sent them on its "+
				"own, or they are the rest of an answer the parser left out",
				zap.String("connID", s.ClientConnID), zap.String("kind", string(s.exchanges.kind)),
				zap.Uint32("firstConnSeq", d.First), zap.Time("from", d.From), zap.Time("to", d.To))
		}
		return
	}
	s.ReportLeftOut(&models.Mock{
		Kind: s.exchanges.kind,
		Spec: models.MockSpec{ReqTimestampMock: d.From, ResTimestampMock: d.To},
	}, leftOutUnanswered)
}

// EndExchanges is how a parser that starts its exchanges with NextRequest
// ends: it calls it as it returns, however it returns (a defer). The run of
// the server stream's start is reported (dropped) as the floor drops it, which
// is when the parser reads the server's stream past it. A parser that returns
// without having read that far would leave it unreported: on a connection the
// capture joined mid-way, the answer in flight when the capture began, and the
// test case that sent its request saved without its mock. Its client sent only
// requests with no answer and then closed (a pool that closes a connection
// with MySQL's COM_QUIT), or closed in the middle of a request. On such a
// connection, EndExchanges reads the server's stream on for the parser until
// its start is behind it (fakeconn.FakeConn.SettleStart): it waits, as the
// parser's read would, for the first server chunk captured after the
// connection's first request, or the stream's end, which the producer brings
// as the connection ends and as Ctx does (JoinedMidConnection). It returns at
// once when the parser has read the stream past its start, or sent no request:
// with none, nothing orders the server's bytes against a request, so nothing
// on the stream can be told to be an answer in flight. On a connection
// captured from its first byte the start's run reports nothing, and
// EndExchanges reads nothing.
func (s *Session) EndExchanges() {
	if s.JoinedMidConnection {
		s.DestStream.SettleStart()
	}
}

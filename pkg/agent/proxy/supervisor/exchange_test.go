package supervisor

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// spanLog is an OrphanSpans that keeps every span it is given.
type spanLog struct {
	mu    sync.Mutex
	spans [][2]time.Time
}

func (l *spanLog) Record(start, end time.Time) {
	l.mu.Lock()
	l.spans = append(l.spans, [2]time.Time{start, end})
	l.mu.Unlock()
}

func (l *spanLog) Open(start time.Time) func() { return func() { l.Record(start, time.Now()) } }

func (l *spanLog) all() [][2]time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][2]time.Time(nil), l.spans...)
}

// exchangeConn is a connection's two streams, fed chunks numbered as the test
// says they were captured.
type exchangeConn struct {
	client, dest chan fakeconn.Chunk
	sess         *Session
	spans        *spanLog
	mgr          *syncMock.SyncMockManager
	logs         *observer.ObservedLogs
	// kind is the kind of the requests request reads: a connection's parser
	// records one (HTTP unless a test says otherwise).
	kind models.Kind
}

func newExchangeConn(t *testing.T, ctx context.Context) *exchangeConn {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	c := &exchangeConn{client: make(chan fakeconn.Chunk, 16), dest: make(chan fakeconn.Chunk, 16), spans: &spanLog{},
		mgr: syncMock.New(nil), logs: logs, kind: models.HTTP}
	c.sess = &Session{
		ClientStream: fakeconn.New(c.client, nil, nil),
		DestStream:   fakeconn.New(c.dest, nil, nil),
		Ctx:          ctx,
		Mgr:          c.mgr,
		Orphans:      c.spans,
		Logger:       zap.New(core),
		ClientConnID: "conn-1",
	}
	return c
}

// at is the capture time of the chunk numbered seq.
func at(seq uint32) time.Time {
	return time.Unix(1_700_000_000, 0).Add(time.Duration(seq) * time.Second)
}

func (c *exchangeConn) req(seq uint32, b string) {
	c.client <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: seq, Bytes: []byte(b), ReadAt: at(seq)}
}

func (c *exchangeConn) resp(seq uint32, b string) {
	c.dest <- fakeconn.Chunk{Dir: fakeconn.FromDest, ConnSeq: seq, Bytes: []byte(b), ReadAt: at(seq)}
}

// request starts an exchange as a parser does: NextRequest, then the request's
// chunk.
func (c *exchangeConn) request(t *testing.T) string {
	t.Helper()
	first, err := c.sess.NextRequest(c.kind, false)
	if err != nil {
		t.Fatalf("NextRequest: %v", err)
	}
	q, err := c.sess.ClientStream.ReadChunk()
	if err != nil || string(q.Bytes) != string(first.Bytes) {
		t.Fatalf("the request read is %q, %v; NextRequest returned %q", q.Bytes, err, first.Bytes)
	}
	return string(q.Bytes)
}

// exchange reads one exchange as a parser does: the request, then the
// response's first chunk.
func (c *exchangeConn) exchange(t *testing.T) (req, resp string) {
	t.Helper()
	q := c.request(t)
	r, err := c.sess.DestStream.ReadChunk()
	if err != nil {
		t.Fatalf("reading the response to %q: %v", q, err)
	}
	return q, string(r.Bytes)
}

// unansweredLines counts the left-out lines with NextRequest's cause: at WARN
// or, past the cause's limit, which is process-wide, at Debug.
func (c *exchangeConn) unansweredLines() int {
	said := 0
	for _, e := range c.logs.All() {
		if r, ok := e.ContextMap()["reason"].(string); ok && strings.HasPrefix(r, leftOutUnansweredCause+":") &&
			(e.Message == LeftOutWarnMsg || e.Message == LeftOutDebugMsg) {
			said++
		}
	}
	return said
}

// droppedLines counts the Debug lines for server bytes dropped with nothing
// left out for them: bytes that answer no request.
func (c *exchangeConn) droppedLines() int {
	return c.logs.FilterMessageSnippet("dropped server bytes captured before the request after them").Len()
}

// NextRequest sets the floor for every request: the server bytes captured
// before a request's first chunk are never its answer. On a connection the
// capture joined mid-way, where the server stream starts with them, before the
// connection's first request, they answer a request the capture does not have:
// that run is reported once, as one span over its capture times, and counted
// once. Bytes captured between two exchanges (a 408 a server sends as it
// closes an idle keep-alive connection, MySQL's ERR at wait_timeout) answer no
// request at all: they are dropped with a Debug line, and nothing is left out
// for them, so no test case in flight is suppressed. Before, they were counted
// as a lost mock too, with a WARN that blamed the capture's start.
func TestNextRequestSetsTheFloorForEveryRequest(t *testing.T) {
	t.Parallel()
	c := newExchangeConn(t, context.Background())
	c.sess.JoinedMidConnection = true
	c.resp(1, "R0 (answers a request the capture does not have)")
	c.req(2, "Q1")
	c.resp(3, "R1")
	c.resp(4, "408 before the next request")
	c.req(5, "Q2")
	c.resp(6, "R2")

	for i, want := range [][2]string{{"Q1", "R1"}, {"Q2", "R2"}} {
		q, r := c.exchange(t)
		if q != want[0] || r != want[1] {
			t.Fatalf("exchange %d paired %q with %q, want %q with %q", i, q, r, want[0], want[1])
		}
	}
	spans := c.spans.all()
	if len(spans) != 1 || !spans[0][0].Equal(at(1)) || !spans[0][1].Equal(at(1)) {
		t.Fatalf("spans left out: %v; want R0's alone, [%v %v]", spans, at(1), at(1))
	}
	if n := c.mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("%d mocks left out counted, want R0's alone", n)
	}
	if n := c.unansweredLines(); n != 1 {
		t.Fatalf("%d left-out lines with cause %q, want R0's alone", n, leftOutUnansweredCause)
	}
	if n := c.droppedLines(); n != 1 {
		t.Fatalf("%d Debug lines for bytes that answer no request, want the 408's", n)
	}
}

// On a connection the capture joined mid-way, the server stream can start with
// bytes captured after the connection's first request, when that request has
// no answer (MySQL's COM_STMT_CLOSE): they were sent after a request the
// parser has, so they answer no request. After bytes captured before the first
// request, they are not in their run, though the floor in force when the
// parser first reads the server's stream (the next request's) drops both: the
// answer to a request the capture does not have is reported once, over its
// own bytes, and they are dropped with a Debug line.
func TestOnlyARunCapturedBeforeTheFirstRequestIsReported(t *testing.T) {
	t.Parallel()
	t.Run("after a first request without an answer", func(t *testing.T) {
		t.Parallel()
		c := newExchangeConn(t, context.Background())
		c.sess.JoinedMidConnection = true
		c.req(1, "STMT_CLOSE")
		c.resp(2, "ERR at wait_timeout")
		c.req(3, "PING")
		c.resp(4, "OK")
		if q := c.request(t); q != "STMT_CLOSE" {
			t.Fatalf("read %q", q)
		}
		if q, r := c.exchange(t); q != "PING" || r != "OK" {
			t.Fatalf("paired %q with %q", q, r)
		}
		if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 || c.unansweredLines() != 0 || c.droppedLines() != 1 {
			t.Fatalf("left out %v (%d counted, %d lines, %d dropped lines); want only a Debug line for the ERR",
				s, c.mgr.MocksLeftOut(), c.unansweredLines(), c.droppedLines())
		}
	})
	t.Run("after an answer captured before the first request", func(t *testing.T) {
		t.Parallel()
		c := newExchangeConn(t, context.Background())
		c.sess.JoinedMidConnection = true
		c.resp(1, "R0")
		c.req(2, "STMT_CLOSE")
		c.resp(3, "ERR at wait_timeout")
		c.req(4, "PING")
		c.resp(5, "OK")
		if q := c.request(t); q != "STMT_CLOSE" {
			t.Fatalf("read %q", q)
		}
		if q, r := c.exchange(t); q != "PING" || r != "OK" {
			t.Fatalf("paired %q with %q", q, r)
		}
		if s := c.spans.all(); len(s) != 1 || !s[0][0].Equal(at(1)) || !s[0][1].Equal(at(1)) || c.mgr.MocksLeftOut() != 1 ||
			c.unansweredLines() != 1 || c.droppedLines() != 1 {
			t.Fatalf("left out %v (%d counted, %d lines, %d dropped lines); want R0's alone, [%v %v], and a Debug line for the ERR",
				s, c.mgr.MocksLeftOut(), c.unansweredLines(), c.droppedLines(), at(1), at(1))
		}
	})
}

// Capture sequences are compared as serial numbers, which order two chunks
// right only while they are fewer than 2^31 apart. A chunk is compared with
// the first request only until the server stream's start is behind it, at the
// first chunk captured after that request, so a run 2^31 chunks after it on a
// long-lived connection the capture joined mid-way, which the comparison would
// put before it, is not reported as the answer to a request the capture does
// not have.
func TestARunLongAfterTheFirstRequestIsNeverComparedWithIt(t *testing.T) {
	t.Parallel()
	c := newExchangeConn(t, context.Background())
	c.sess.JoinedMidConnection = true
	c.req(10, "Q1")
	c.resp(11, "R1")
	late := uint32(10 + 1<<31 + 1)
	if !(fakeconn.Chunk{ConnSeq: late}).CapturedBefore(10) {
		t.Fatal("the serial comparison does not put the late run before the first request: the test proves nothing")
	}
	c.resp(late, "ERR at wait_timeout")
	c.req(late+1, "PING")
	c.resp(late+2, "OK")
	for i, want := range [][2]string{{"Q1", "R1"}, {"PING", "OK"}} {
		if q, r := c.exchange(t); q != want[0] || r != want[1] {
			t.Fatalf("exchange %d paired %q with %q", i, q, r)
		}
	}
	if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 || c.droppedLines() != 1 {
		t.Fatalf("left out %v (%d counted, %d dropped lines); want only a Debug line", s, c.mgr.MocksLeftOut(), c.droppedLines())
	}
}

// The answer in flight that the floor drops was captured, and it is not
// recorded, so it is reported once however late the parser reads it: after
// the recording stopped, and after the parser alone was retired, as every
// other exchange left out is (ReportLeftOut). The recording's stop
// (Session.RecordingStopping) decides only what follows it, and the parser's
// lifetime (Ctx) nothing.
func TestNextRequestReportsTheAnswerInFlightHoweverLateItIsRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// recordingStops stops the recording, which ends the parser's
		// lifetime with it; otherwise the parser alone is retired.
		recordingStops bool
	}{{"the recording stops", true}, {"the parser is retired", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recording, stop := context.WithCancel(context.Background())
			defer stop()
			parser, retire := context.WithCancel(recording)
			defer retire()
			c := newExchangeConn(t, parser)
			c.sess.RecordingDone = recording.Done()
			c.sess.JoinedMidConnection = true
			c.resp(1, "R0")
			c.req(2, "Q1")
			if _, err := c.sess.NextRequest(models.HTTP, false); err != nil {
				t.Fatal(err)
			}
			if tc.recordingStops {
				stop()
			} else {
				retire()
			}
			c.resp(3, "R1")
			if r, err := c.sess.DestStream.ReadChunk(); err != nil || string(r.Bytes) != "R1" {
				t.Fatalf("read %q, %v; want R1", r.Bytes, err)
			}
			s := c.spans.all()
			if len(s) != 1 || !s[0][0].Equal(at(1)) || !s[0][1].Equal(at(1)) || c.mgr.MocksLeftOut() != 1 ||
				c.unansweredLines() != 1 {
				t.Fatalf("reported %v (%d counted, %d lines), want R0's once, [%v %v]",
					s, c.mgr.MocksLeftOut(), c.unansweredLines(), at(1), at(1))
			}
		})
	}
}

// A connection captured from its first byte (the relay's, and any the
// producer saw open) has had nothing sent on it before the capture began, so
// server bytes captured before its first request answer no request: the
// server sent them on its own (an HTTP server's 408 on a connection that idled
// out as the client sent its first request). They are not the first request's
// answer, and they are dropped with a Debug line: nothing is left out, counted
// or said at WARN for them, so the test case that sent the request is not
// suppressed. Before, they were reported as the answer to a request the
// capture did not have.
func TestACaptureFromTheConnectionsFirstByteReportsNothingItDrops(t *testing.T) {
	t.Parallel()
	c := newExchangeConn(t, context.Background())
	c.resp(1, "408 as the connection idles out")
	c.req(2, "Q1")
	c.resp(3, "R1")
	if q, r := c.exchange(t); q != "Q1" || r != "R1" {
		t.Fatalf("paired %q with %q", q, r)
	}
	if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 || c.unansweredLines() != 0 || c.droppedLines() != 1 {
		t.Fatalf("left out %v (%d counted, %d lines, %d dropped lines); want only a Debug line for the 408",
			s, c.mgr.MocksLeftOut(), c.unansweredLines(), c.droppedLines())
	}
}

// A request whose first chunk its producer did not number cannot be ordered
// against the server's bytes: NextRequest refuses it.
func TestNextRequestRefusesAnUnnumberedRequest(t *testing.T) {
	t.Parallel()
	c := newExchangeConn(t, context.Background())
	c.client <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("GET / HTTP/1.1\r\n\r\n"), ReadAt: time.Now()}
	if _, err := c.sess.NextRequest(models.HTTP, false); !errors.Is(err, fakeconn.ErrUnnumbered) {
		t.Fatalf("NextRequest = %v, want fakeconn.ErrUnnumbered", err)
	}
}

// NextRequest ends with the client's stream.
func TestNextRequestReturnsTheEndOfTheClientsStream(t *testing.T) {
	t.Parallel()
	c := newExchangeConn(t, context.Background())
	close(c.client)
	if _, err := c.sess.NextRequest(models.HTTP, false); !errors.Is(err, io.EOF) {
		t.Fatalf("NextRequest = %v, want io.EOF", err)
	}
}

// A parser that goes on to a request with an earlier answer still to read
// (answerUnread) still reads the rest of it, though it was captured before
// that request, and however late it reaches the parser; the next request it
// starts without one sets the floor again.
func TestNextRequestKeepsTheFloorWhileAnAnswerIsUnread(t *testing.T) {
	t.Parallel()
	c := newExchangeConn(t, context.Background())
	c.kind = models.MySQL
	c.req(1, "PREPARE")
	c.resp(2, "PREPARE OK + definitions")
	c.req(4, "EXECUTE")
	if q, r := c.exchange(t); q != "PREPARE" || r != "PREPARE OK + definitions" {
		t.Fatalf("paired %q with %q", q, r)
	}
	if first, err := c.sess.NextRequest(models.MySQL, true); err != nil || string(first.Bytes) != "EXECUTE" {
		t.Fatalf("NextRequest = %q, %v", first.Bytes, err)
	}
	if _, err := c.sess.ClientStream.ReadChunk(); err != nil {
		t.Fatal(err)
	}
	// The PREPARE's EOF was captured before the EXECUTE, and is teed only now.
	c.resp(3, "EOF of the PREPARE")
	c.resp(5, "EXECUTE result")
	for _, want := range []string{"EOF of the PREPARE", "EXECUTE result"} {
		if r, err := c.sess.DestStream.ReadChunk(); err != nil || string(r.Bytes) != want {
			t.Fatalf("read %q, %v; want %q", r.Bytes, err, want)
		}
	}
	c.resp(6, "a stale reply")
	c.req(7, "PING")
	c.resp(8, "OK")
	if q, r := c.exchange(t); q != "PING" || r != "OK" {
		t.Fatalf("after the answer was read, paired %q with %q", q, r)
	}
	if s := c.spans.all(); len(s) != 0 || c.droppedLines() != 1 {
		t.Fatalf("spans %v, %d dropped lines; want the stale reply dropped with a Debug line alone", s, c.droppedLines())
	}
}

// endExchanges calls EndExchanges as a parser's return does, and fails the
// test if it does not return.
func (c *exchangeConn) endExchanges(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.sess.EndExchanges()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("EndExchanges did not return")
	}
}

// A parser that returns without reading the server's stream past its start (a
// pool that closes the connection with MySQL's COM_QUIT, a client that closes
// in the middle of a request) has EndExchanges read it on: on a connection the
// capture joined mid-way, the answer in flight when the capture began is
// reported once, as when the parser reads past it, and EndExchanges waits for
// it as a read does, until the first server chunk captured after the first
// request, or the stream's end. With no request, nothing orders the server's
// bytes against one, and on a connection captured from its first byte the
// start's run reports nothing: there, and once the parser has read past the
// start, it returns at once, and reads nothing.
func TestEndExchangesReportsTheAnswerInFlightOfAParserThatDoesNotReadIt(t *testing.T) {
	t.Parallel()
	reportedOnce := func(t *testing.T, c *exchangeConn) {
		t.Helper()
		if s := c.spans.all(); len(s) != 1 || !s[0][0].Equal(at(1)) || !s[0][1].Equal(at(1)) || c.mgr.MocksLeftOut() != 1 ||
			c.unansweredLines() != 1 {
			t.Fatalf("reported %v (%d counted, %d lines), want R0's once, [%v %v]", s, c.mgr.MocksLeftOut(), c.unansweredLines(), at(1), at(1))
		}
	}
	t.Run("the client quits, then the server's stream ends", func(t *testing.T) {
		t.Parallel()
		c := newExchangeConn(t, context.Background())
		c.sess.JoinedMidConnection = true
		c.resp(1, "R0")
		c.req(2, "QUIT")
		if q := c.request(t); q != "QUIT" {
			t.Fatalf("read %q", q)
		}
		close(c.client)
		if _, err := c.sess.NextRequest(models.MySQL, false); !errors.Is(err, io.EOF) {
			t.Fatalf("NextRequest = %v, want io.EOF", err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.sess.EndExchanges()
		}()
		// It drops R0 and waits on the server's stream, which ends only now.
		for deadline := time.Now().Add(5 * time.Second); ; {
			if w, _ := c.sess.DestStream.Waiting(); w {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("EndExchanges never waited on the server's stream")
			}
			time.Sleep(time.Millisecond)
		}
		close(c.dest)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("EndExchanges did not return at the stream's end")
		}
		reportedOnce(t, c)
	})
	t.Run("the server sends after the first request", func(t *testing.T) {
		t.Parallel()
		c := newExchangeConn(t, context.Background())
		c.sess.JoinedMidConnection = true
		c.resp(1, "R0")
		c.req(2, "the head of a request")
		if q := c.request(t); q != "the head of a request" {
			t.Fatalf("read %q", q)
		}
		c.resp(3, "400")
		c.endExchanges(t)
		reportedOnce(t, c)
	})
	t.Run("the parser read past the start", func(t *testing.T) {
		t.Parallel()
		c := newExchangeConn(t, context.Background())
		c.sess.JoinedMidConnection = true
		c.resp(1, "R0")
		c.req(2, "Q1")
		c.resp(3, "R1")
		if q, r := c.exchange(t); q != "Q1" || r != "R1" {
			t.Fatalf("paired %q with %q", q, r)
		}
		c.endExchanges(t)
		reportedOnce(t, c)
	})
	t.Run("no request", func(t *testing.T) {
		t.Parallel()
		c := newExchangeConn(t, context.Background())
		c.sess.JoinedMidConnection = true
		c.resp(1, "R0")
		close(c.client)
		if _, err := c.sess.NextRequest(models.MySQL, false); !errors.Is(err, io.EOF) {
			t.Fatalf("NextRequest = %v, want io.EOF", err)
		}
		c.endExchanges(t)
		if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 || c.sess.DestStream.Consumed() != 0 {
			t.Fatalf("left out %v (%d counted), read %d bytes; want nothing", s, c.mgr.MocksLeftOut(), c.sess.DestStream.Consumed())
		}
	})
	t.Run("a connection captured from its first byte", func(t *testing.T) {
		t.Parallel()
		c := newExchangeConn(t, context.Background())
		c.resp(1, "408 as the connection idles out")
		c.req(2, "QUIT")
		if q := c.request(t); q != "QUIT" {
			t.Fatalf("read %q", q)
		}
		c.endExchanges(t)
		if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 || c.droppedLines() != 0 || c.sess.DestStream.Consumed() != 0 {
			t.Fatalf("left out %v (%d counted, %d dropped lines), read %d bytes; want nothing", s, c.mgr.MocksLeftOut(), c.droppedLines(), c.sess.DestStream.Consumed())
		}
	})
}

// A Session is made for each connection. NextRequest's state (exchanges)
// keeps a Session in the 640 B size class, not the 704 B one.
func TestASessionStaysInThe640ByteSizeClass(t *testing.T) {
	if n := unsafe.Sizeof(Session{}); n > 640 {
		t.Fatalf("a Session is %d B, past the 640 B size class: each connection's takes 704 B", n)
	}
}

// unfinished reports the exchanges the session holds open as a parser that
// stopped without ending them, and returns how many mocks were left out in
// all, with the spans recorded.
func (c *exchangeConn) unfinished(t *testing.T) (int64, [][2]time.Time) {
	t.Helper()
	c.sess.StoppedAt()
	c.sess.ReportUnfinishedExchange("it panicked")
	return c.mgr.MocksLeftOut(), c.spans.all()
}

// A parser that reports its own stop (ReportStoppedOn) ends whatever exchange
// it had begun, whichever request time it gives: the MySQL recorder reports
// the command it stopped in from the end of the last exchange it read
// (leaveOutInFlight), older than the command NextRequest began. The supervisor
// and the dispatcher then have nothing left to report for it; reported again,
// it was two mocks left out and two WARNs for one exchange.
func TestAParsersOwnStopIsNotReportedAgain(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.req(2, "COM_QUERY")
	c.request(t)
	c.sess.ReportStoppedOn(models.MySQL, at(1), "mysql framing: x")
	if left, _ := c.unfinished(t); left != 1 {
		t.Fatalf("%d mocks left out, want the one the parser reported", left)
	}
}

// One exchange held open (a PREPARE the MySQL recorder settles later) while
// the next begins: ending the held one leaves the next open, as the oldest
// one, and a parser that stops in it leaves it out. Tracked in one slot that
// only the first could take, the next ran untracked, and was in no report.
func TestTheNextExchangeStaysOpenOnceAnOlderOneEnds(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.kind = models.MySQL
	c.req(2, "PREPARE")
	c.request(t)
	c.req(3, "EXECUTE")
	if _, err := c.sess.NextRequest(models.MySQL, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.sess.ClientStream.ReadChunk(); err != nil {
		t.Fatal(err)
	}
	if got := c.sess.exchangeAt.Load(); got != at(2).UnixNano() {
		t.Fatalf("the oldest open exchange is at %v, want the PREPARE's %v: a later request took it", time.Unix(0, got), at(2))
	}
	_ = c.sess.EmitMock(&models.Mock{Kind: models.MySQL, Spec: models.MockSpec{ReqTimestampMock: at(2), ResTimestampMock: at(2)}})
	left, spans := c.unfinished(t)
	if left != 1 || len(spans) != 1 || !spans[0][0].Equal(at(3)) {
		t.Fatalf("%d left out, spans %v; want the EXECUTE's exchange, from %v", left, spans, at(3))
	}
}

// The span of an unfinished exchange ends at the last byte the parser read of
// it, not at the stop: a request read, then minutes of an idle connection
// before the parser stops, is a span over the request, not over the idle
// minutes.
func TestAnUnfinishedExchangesSpanEndsAtWhatTheParserRead(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.req(2, "GET /a")
	c.request(t)
	c.sess.CaptureClock = func() time.Time { return at(300) } // the stop, minutes later
	left, spans := c.unfinished(t)
	if left != 1 || len(spans) != 1 || !spans[0][0].Equal(at(2)) || !spans[0][1].Equal(at(2)) {
		t.Fatalf("%d left out, spans %v; want one over [%v, %v]", left, spans, at(2), at(2))
	}
}

// A mock the session refuses (its context done, as a memory-cap retirement
// cancels it before the abort reports) does not end the exchange: the abort
// reports it. Ended before the refusal, it was in no report.
func TestAMockRefusedDoesNotEndItsExchange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := newExchangeConn(t, ctx)
	c.sess.RouteMocksViaSyncMock = true
	c.req(2, "GET /a")
	c.request(t)
	cancel()
	if err := c.sess.EmitMock(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at(2), ResTimestampMock: at(2)}}); err == nil {
		t.Fatal("fixture: the session accepted a mock on a done context")
	}
	if left, _ := c.unfinished(t); left != 1 {
		t.Fatalf("%d left out, want the exchange whose mock was refused", left)
	}
}

// The kind of the exchange left unfinished is read by the goroutine that
// reports it, while the parser's own goroutine goes on with NextRequest: the
// kind is written once, at the connection's first request, before any
// exchange's start is published, so the report reads it after that write and
// no write races it. The reporter keeps reporting until the parser has made
// all its requests, so every later NextRequest runs against a report.
func TestAnUnfinishedExchangeIsReportedWithoutARace(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	const requests = 500
	c.client = make(chan fakeconn.Chunk, requests)
	c.sess.ClientStream = fakeconn.New(c.client, nil, nil)
	for i := uint32(2); i < 2+requests; i++ {
		c.req(i, "GET /a")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < requests; i++ {
			if _, err := c.sess.NextRequest(models.HTTP, false); err != nil {
				return
			}
			if _, err := c.sess.ClientStream.ReadChunk(); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			// A reporter descheduled for the whole run reports it now.
			c.sess.ReportUnfinishedExchange("over its memory cap")
			if n := c.mgr.MocksLeftOut(); n != 1 {
				t.Fatalf("%d exchanges reported, want the one the parser was in, once", n)
			}
			return
		default:
		}
		c.sess.ReportUnfinishedExchange("over its memory cap")
	}
}

// A connection's parser records one kind of exchange: a request of another
// kind is refused (a parser bug), and the exchange left unfinished is
// reported under the kind it began with.
func TestAConnectionsParserRecordsOneKind(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.req(2, "GET /a")
	c.req(3, "COM_QUERY")
	c.request(t)
	if _, err := c.sess.NextRequest(models.MySQL, false); !errors.Is(err, errKindChanged) {
		t.Fatalf("NextRequest of another kind = %v, want it refused (errKindChanged)", err)
	}
	c.sess.StoppedAt()
	c.sess.ReportUnfinishedExchange("it panicked")
	var kinds []any
	for _, e := range c.logs.All() {
		if e.Message == LeftOutWarnMsg || e.Message == LeftOutDebugMsg { // past the WARN's limit, at Debug
			kinds = append(kinds, e.ContextMap()["kind"])
		}
	}
	if len(kinds) != 1 || kinds[0] != string(models.HTTP) {
		t.Fatalf("the exchange left unfinished is reported as %v, want once, as the HTTP exchange it began as", kinds)
	}
}

// The supervisor or the dispatcher's abort reports the exchange a parser was
// in (ReportUnfinishedExchange); the parser, still running, then reports its
// own stop on it (ReportStoppedOn), as a retired parser that wakes does. The
// exchange is reported once: twice, it was two mocks left out and two WARNs.
// Nor does an exchange begin after it: the parser is gone.
func TestAnExchangeReportedUnfinishedIsNotReportedAgainByItsParser(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.req(2, "GET /a")
	c.req(3, "GET /b")
	c.request(t)
	if left, _ := c.unfinished(t); left != 1 {
		t.Fatalf("%d left out, want the exchange the parser was in", left)
	}
	c.sess.ReportStoppedOn(models.HTTP, at(2), "http decode error: x")
	if n := c.mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("%d left out once the parser reported its own stop too, want the one exchange, once", n)
	}
	c.request(t)
	c.sess.ReportUnfinishedExchange("again")
	if n := c.mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("%d left out: an exchange began after the parser was reported gone", n)
	}
}

// A mock left out with no request time says not which exchange it was (the
// floor's report of a connection's start with no capture time comes as the
// first request's exchange begins): it ends none. It ended the oldest open,
// and a parser that then died in that exchange left it in no report.
func TestALeftOutReportWithNoTimeEndsNoExchange(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.req(2, "GET /a")
	c.request(t)
	c.sess.ReportLeftOut(&models.Mock{Kind: models.HTTP}, "no time")
	left, spans := c.unfinished(t)
	if left != 2 || len(spans) != 1 || !spans[0][0].Equal(at(2)) {
		t.Fatalf("%d left out, spans %v: want the report with no time, and the exchange the parser was in from %v", left, spans, at(2))
	}
}

// A mock whose request time is earlier than one already emitted on the
// session is clamped to just after it (invariant I5): one nanosecond later,
// in the mock's own location, so the recorded time reads as the parser's.
func TestAnOutOfOrderRequestTimeIsClampedJustAfterTheLast(t *testing.T) {
	prev := debugMonotonic.Load()
	debugMonotonic.Store(false)
	t.Cleanup(func() { debugMonotonic.Store(prev) })
	loc := time.FixedZone("IST", 5*3600+1800)
	s := &Session{}
	first := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: at(5).In(loc)}}
	s.enforceReqMonotonic(first)
	late := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: at(3).In(loc), ResTimestampMock: at(9).In(loc)}}
	s.enforceReqMonotonic(late)
	got := late.Spec.ReqTimestampMock
	if want := at(5).Add(time.Nanosecond); !got.Equal(want) {
		t.Fatalf("clamped to %v, want one nanosecond after the last request time, %v", got, want)
	}
	if got.Location() != loc {
		t.Fatalf("clamped request time is in %v, want the mock's own location %v", got.Location(), loc)
	}
	inOrder := &models.Mock{Spec: models.MockSpec{ReqTimestampMock: at(7).In(loc)}}
	s.enforceReqMonotonic(inOrder)
	if !inOrder.Spec.ReqTimestampMock.Equal(at(7)) {
		t.Fatalf("an in-order request time was changed to %v", inOrder.Spec.ReqTimestampMock)
	}
}

// Supervisor.Run reports the exchange a parser that returns an error, or
// panics, was in, as it learns of it, whatever its caller does on a
// retirement: the parser cannot report it, having stopped.
func TestTheSupervisorReportsTheExchangeAParserDiedIn(t *testing.T) {
	for _, panics := range []bool{false, true} {
		c := newExchangeConn(t, context.Background())
		c.req(2, "GET /a")
		sv := New(Config{Logger: zap.NewNop()})
		sv.Run(context.Background(), func(_ context.Context, s *Session) error {
			if _, err := s.NextRequest(models.HTTP, false); err != nil {
				return err
			}
			if _, err := s.ClientStream.ReadChunk(); err != nil {
				return err
			}
			if panics {
				panic("a parser bug")
			}
			return errors.New("a parser error")
		}, c.sess)
		sv.Close()
		if left := c.mgr.MocksLeftOut(); left != 1 {
			t.Fatalf("panics=%v: %d mocks left out, want the exchange the parser died in", panics, left)
		}
	}
}

// The mock of a connection's handshake (EmitHandshakeMock: the MySQL
// recorder's config mock) answers no request NextRequest found, and ends no
// exchange, whatever its request time: one open stays open.
func TestAHandshakeMockEndsNoExchange(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.kind = models.MySQL
	c.req(2, "COM_QUERY")
	c.request(t)
	if err := c.sess.EmitHandshakeMock(&models.Mock{Kind: models.MySQL, Spec: models.MockSpec{ReqTimestampMock: at(3), ResTimestampMock: at(3)}}); err != nil {
		t.Fatal(err)
	}
	left, spans := c.unfinished(t)
	if left != 1 || len(spans) != 1 || !spans[0][0].Equal(at(2)) {
		t.Fatalf("%d left out, spans %v: want the exchange the parser was in, from %v, still open past the handshake's mock", left, spans, at(2))
	}
}

// The floor's report of the server bytes at a connection's start is made
// apart from the exchanges the parser began: those bytes can be stamped at or
// after the first request (the two directions are stamped apart), and ended by
// their times they ended that request's exchange, which a parser that then
// died in it left in no report.
func TestTheFloorsReportEndsNoExchange(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.sess.JoinedMidConnection = true
	c.dest <- fakeconn.Chunk{Dir: fakeconn.FromDest, ConnSeq: 1, Bytes: []byte("R0"), ReadAt: at(3)} // captured first, stamped after Q1
	c.req(2, "Q1")
	c.resp(4, "R1")
	if q, r := c.exchange(t); q != "Q1" || r != "R1" {
		t.Fatalf("paired %q with %q", q, r)
	}
	left, spans := c.unfinished(t)
	if left != 2 || len(spans) != 2 || !spans[1][0].Equal(at(2)) {
		t.Fatalf("%d left out, spans %v: want R0's, and the exchange the parser was in from %v", left, spans, at(2))
	}
}

// A parser that reports its own stop after the supervisor or the abort
// reported the exchange it was in is not counted again, but what its own
// request time covers before the start they reported from is still left out:
// the MySQL recorder reports from the end of the last exchange it recorded,
// before the command NextRequest found.
func TestAParsersOwnStopAfterTheReportLeavesOutWhatThatReportDidNot(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.req(5, "GET /a")
	c.request(t)
	if left, _ := c.unfinished(t); left != 1 {
		t.Fatalf("%d left out, want the exchange the parser was in", left)
	}
	c.sess.ReportStoppedOn(models.HTTP, at(3), "http decode error: x")
	if n := c.mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("%d left out, want the one exchange, counted once", n)
	}
	spans := c.spans.all()
	if len(spans) != 2 || !spans[1][0].Equal(at(3)) || !spans[1][1].Equal(at(5)) {
		t.Fatalf("spans %v: want the part before the reported start, [%v %v], left out too", spans, at(3), at(5))
	}
}

// A parser that reports its own stop with no request time is spanned from the
// exchange NextRequest found it in, not counted with no span.
func TestAParsersOwnStopWithNoTimeIsSpannedFromItsExchange(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.req(4, "GET /a")
	c.request(t)
	c.sess.ReportStoppedOn(models.HTTP, time.Time{}, "http decode error: x")
	spans := c.spans.all()
	if c.mgr.MocksLeftOut() != 1 || len(spans) != 1 || !spans[0][0].Equal(at(4)) {
		t.Fatalf("%d left out, spans %v: want one, from the exchange's request %v", c.mgr.MocksLeftOut(), spans, at(4))
	}
}

// The newest exchange begun is the latest NextRequest found, in stream order:
// with the wall clock stepped back between two requests, ending the first
// does not leave the slot on a stamp from before the step, which no later
// request's mock would end.
func TestTheNewestExchangeIsTheLatestFound(t *testing.T) {
	c := newExchangeConn(t, context.Background())
	c.req(10, "GET /a")
	c.client <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: 11, Bytes: []byte("GET /b"), ReadAt: at(2)} // the clock stepped back
	c.request(t)
	c.request(t)
	if got := c.sess.exchangeNewest.Load(); got != at(2).UnixNano() {
		t.Fatalf("the newest exchange begun is at %v, want the latest found, %v", time.Unix(0, got), at(2))
	}
}

// A mock ends the exchange its request time names as the parser emitted it,
// not as the session clamps it to keep request times in order (I5): with the
// wall clock stepped back, a held PREPARE's mock, emitted after a later
// command's, is clamped past every exchange still open, and ended by that
// time it ended the EXECUTE in flight too, which a parser that then died in it
// left in no report.
func TestTheClampedRequestTimeEndsNoNewerExchange(t *testing.T) {
	prev := debugMonotonic.Load()
	debugMonotonic.Store(false)
	t.Cleanup(func() { debugMonotonic.Store(prev) })
	c := newExchangeConn(t, context.Background())
	c.kind = models.MySQL
	c.req(100, "COM_QUERY")
	c.request(t)
	_ = c.sess.EmitMock(&models.Mock{Kind: models.MySQL, Spec: models.MockSpec{ReqTimestampMock: at(100), ResTimestampMock: at(100)}})
	c.client <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: 101, Bytes: []byte("PREPARE"), ReadAt: at(50)} // the clock stepped back
	c.request(t)
	c.client <- fakeconn.Chunk{Dir: fakeconn.FromClient, ConnSeq: 102, Bytes: []byte("EXECUTE"), ReadAt: at(60)}
	if _, err := c.sess.NextRequest(models.MySQL, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.sess.ClientStream.ReadChunk(); err != nil {
		t.Fatal(err)
	}
	_ = c.sess.EmitMock(&models.Mock{Kind: models.MySQL, Spec: models.MockSpec{ReqTimestampMock: at(50), ResTimestampMock: at(55)}}) // the held PREPARE's
	left, spans := c.unfinished(t)
	if left != 1 || len(spans) != 1 || !spans[0][0].Equal(at(60)) {
		t.Fatalf("%d left out, spans %v: want the EXECUTE's exchange, still open, from %v", left, spans, at(60))
	}
}

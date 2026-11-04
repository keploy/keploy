//go:build linux

package http

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
)

// The upstream's end read once the recording's stop had begun
// (Session.EndFollowedStop), partway through the response, is the stop's, as
// before the answer: no fault of the upstream's or the application's. Its
// response is not whole: its head, or a body its Content-Length or chunked
// framing frames, had not ended. So the exchange is left out, with its span,
// for the stop (supervisor.LeftOutStopCut): not reported as a response that
// does not decode, and the parser goes on, with no error logged for a normal
// stop. A close-delimited body ends with the upstream's close, and is
// recorded whole.

// upstreamStopCut runs one call through the relay whose response the upstream
// sends as far as part, and then ends, once the recording's stop has begun: by
// a close, or a reset. With mark, the session holds a mark on the
// incomplete-mock flag before the upstream's end (a chunk the capture lost).
// at bounds the response's chunks, part its last part.
func upstreamStopCut(t *testing.T, part string, reset, mark bool) (c *relayedConn, at cutTimes, mocks []*models.Mock, err error) {
	t.Helper()
	c = newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	at.sent = time.Now()
	at.last = at.sent
	write(t, c.dest, c.dst, part)
	relayed(t, c.app, part)
	at.done = time.Now()
	if mark {
		c.sess.MarkMockIncomplete("per_conn_cap")
	}
	c.stopBegun.Store(true)
	if reset {
		resetConn(t, c.dest)
	} else {
		_ = c.dest.Close()
	}
	awaitPeerEnd(t, c, fakeconn.FromDest)
	mocks, err = c.finish(t)
	return c, at, mocks, err
}

// cutTimes bound the times of a cut response's chunks, as the relay stamps
// each (WrittenAt) once it has read it from the upstream and before it writes
// it to the application: sent is a time before the upstream sent the
// response's first part, last a time before it sent the last part the parser
// read, and done a time after the application had read that part.
type cutTimes struct{ sent, last, done time.Time }

// leftOutReasons is the reason of each exchange c's session left out, as it
// logged them.
func leftOutReasons(c *relayedConn) []string {
	var got []string
	for _, e := range c.logs.All() {
		if e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg {
			r, _ := e.ContextMap()["reason"].(string)
			got = append(got, r)
		}
	}
	return got
}

// errorsLogged is each entry c's session logged at ERROR or above.
func errorsLogged(c *relayedConn) []string {
	var got []string
	for _, e := range c.logs.All() {
		if e.Level >= zapcore.ErrorLevel {
			got = append(got, e.Message)
		}
	}
	return got
}

// stopCutParts is a response cut short partway through each part its framing
// frames: a Content-Length body, a chunked body between chunks and inside one,
// and the head.
var stopCutParts = []struct{ name, part string }{
	{"Content-Length body", "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n01234"},
	{"chunked body", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nabcde\r\n"},
	{"chunked body in a chunk", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n14\r\nabcde"},
	{"head", "HTTP/1.1 200 OK\r\nContent-Le"},
}

// switchName and markName name a row's proven switch and mark.
var (
	switchName = map[bool]string{false: ", switch off", true: ", switch on"}
	markName   = map[bool]string{false: "", true: ", marked"}
)

// wantLeftOutForTheStop checks that c's call, whose response the recording's
// stop cut short, was left out once for the stop's reason want (under the
// mark per_conn_cap with mark, which it took), with no error returned or
// logged, and with one span, from the request's time, before the response's
// first part was sent, to the time the relay stamped the last chunk of the
// response the parser read, within at's bounds of the last part.
func wantLeftOutForTheStop(t *testing.T, c *relayedConn, at cutTimes, mocks []*models.Mock, err error, want string, mark bool) {
	t.Helper()
	if mark {
		want = "per_conn_cap: " + want
	}
	if err != nil {
		t.Errorf("recordV2 = %v, want nil: the recording's stop cut the response, which is no decode error", err)
	}
	if len(mocks) != 0 {
		t.Errorf("recorded %d mocks, want none: the response is not whole", len(mocks))
	}
	if got := c.mgr.MocksLeftOut(); got != 1 {
		t.Errorf("left out %d mocks, want 1", got)
	}
	if got := leftOutReasons(c); len(got) != 1 || got[0] != want {
		t.Errorf("left out for %q, want once for %q", got, want)
	}
	if c.sess.IsMockIncomplete() {
		t.Error("the incomplete-mock mark is still set, want it taken by the exchange it was left out under")
	}
	if got := errorsLogged(c); len(got) != 0 {
		t.Errorf("logged %q at ERROR, want nothing: the recording stopped as it should", got)
	}
	spans := c.spans.all()
	if len(spans) != 1 || spans[0][0].IsZero() || !spans[0][0].Before(at.sent) ||
		spans[0][1].Before(at.last) || spans[0][1].After(at.done) {
		t.Errorf("spans %v, want one, from the request's time (before %v) to the response's last chunk the parser read "+
			"(stamped from %v to %v): test cases over the call are left out", spans, at.sent, at.last, at.done)
	}
}

func TestRecordV2_StopCutAnswerIsLeftOutForTheStop(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, f := range stopCutParts {
			for _, reset := range []bool{false, true} {
				for _, mark := range []bool{false, true} {
					name := f.name + map[bool]string{false: ", closed", true: ", reset"}[reset] + markName[mark] + switchName[proven]
					t.Run(name, func(t *testing.T) {
						t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
						c, at, mocks, err := upstreamStopCut(t, f.part, reset, mark)
						wantLeftOutForTheStop(t, c, at, mocks, err, supervisor.LeftOutStopCut, mark)
					})
				}
			}
		}
	}
}

// The close that ends a close-delimited body after the stop began is still
// its end: the response is whole, and recorded, with no fault.
func TestRecordV2_StopEndsACloseDelimitedAnswerWhole(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(map[bool]string{false: "switch off", true: "switch on"}[proven], func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
			c, _, mocks, err := upstreamStopCut(t, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n01234", false, false)
			m := oneMock(t, mocks, err)
			if m.Spec.Fault != nil || m.Spec.HTTPResp.Body != "01234" {
				t.Errorf("recorded body %q, fault %v; want the whole body %q, no fault", m.Spec.HTTPResp.Body, m.Spec.Fault, "01234")
			}
			if got := leftOutReasons(c); len(got) != 0 {
				t.Errorf("left out for %q, want nothing", got)
			}
		})
	}
}

// Not the stop's cut: a stream that ended at a capture hole (the capture lost
// what came after), and a client that closed first, whose end is its own
// (FirstPeerEnded), are never left out for the upstream's end at the stop.
func TestRecordV2_StopCutOnlyForTheUpstreamsEnd(t *testing.T) {
	const part = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n01234"
	t.Run("capture hole", func(t *testing.T) {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		write(t, c.dest, c.dst, part)
		relayed(t, c.app, part)
		c.sess.MarkEndedAtHole(fakeconn.FromDest, "test hole")
		c.stopBegun.Store(true)
		_ = c.dest.Close()
		awaitPeerEnd(t, c, fakeconn.FromDest)
		_, _ = c.finish(t)
		if got := stopReasons(c); len(got) != 0 {
			t.Errorf("left out for %q, want another reason: the capture lost the rest of the response", got)
		}
	})
	t.Run("client closed first", func(t *testing.T) {
		c := newRelayedConn(t)
		c.start(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		write(t, c.dest, c.dst, part)
		relayed(t, c.app, part)
		c.stopBegun.Store(true)
		appClosed(t, c)
		_, _ = c.finish(t)
		if got := stopReasons(c); len(got) != 0 {
			t.Errorf("left out for %q, want another reason: the client's end came first", got)
		}
	})
}

// The recording's stop with the connection still open, partway through the
// response: neither side has ended, and the capture of the connection ends
// with the recording, as the proxy wires it. The session's RecordingDone is
// the recording's context's, and the parser's context and the relay's are
// under it. However the parser's read of the response ends, the exchange is
// left out for the stop (LeftOutStopCutOpen), framed or close-delimited, with
// its span, and nothing is logged at ERROR:
//   - relayEnds: the relay ends its streams at its context's end;
//   - abortCloses: the supervisor's abort at the stop closes the streams while
//     the relay's read of the upstream has not yet returned;
//   - parserCtxEnds: the parser's context has ended as it takes the next part
//     of the response, which the relay still relays.
type openStopWay int

const (
	relayEnds openStopWay = iota
	abortCloses
	parserCtxEnds
	// parserRetired is no stop: the parser's context alone ends (the
	// supervisor retires the parser), the recording runs on, and the
	// relay relays the next part.
	parserRetired
)

func (w openStopWay) String() string {
	return [...]string{"the relay's end", "the abort's close", "the parser's context", "the parser retired"}[w]
}

// deafToDeadlines is the destination's connection as the relay reads it, with
// no read deadline set: the relay's nudge at its context's end does not end
// its read of the upstream, which returns with the upstream's bytes or end.
type deafToDeadlines struct{ net.Conn }

func (deafToDeadlines) SetReadDeadline(time.Time) error { return nil }

// NetConn is the conn under it, the connseq.Upstream the relay numbers by.
func (c deafToDeadlines) NetConn() net.Conn { return c.Conn }

// openStopCut runs one call through the relay whose response the upstream
// sends as far as part, and then the recording stops, the way way says, with
// the connection open; more is the next part of the response the upstream
// sends for parserCtxEnds, which does not end it either. With mark, the
// session holds a mark on the incomplete-mock flag before the stop. at bounds
// the response's chunks: its last part is more for parserCtxEnds, part
// otherwise.
func openStopCut(t *testing.T, part, more string, way openStopWay, mark bool) (c *relayedConn, at cutTimes, mocks []*models.Mock, err error) {
	t.Helper()
	var held func(*supervisor.Session)
	if mark {
		held = func(s *supervisor.Session) { s.MarkMockIncomplete("per_conn_cap") }
	}
	return openStopCutHolding(t, part, more, way, held)
}

// openStopCutHolding is openStopCut with what the session holds before the
// stop set by held, if not nil.
func openStopCutHolding(t *testing.T, part, more string, way openStopWay, held func(*supervisor.Session)) (c *relayedConn, at cutTimes, mocks []*models.Mock, err error) {
	t.Helper()
	c = newRelayedConn(t)
	if way == abortCloses {
		c.wrapDst = func(u net.Conn) net.Conn { return deafToDeadlines{u} }
	}
	c.startRelay(t)
	rec, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	parserCtx, cancelParser := context.WithCancel(rec)
	t.Cleanup(cancelParser)
	c.sess.RecordingDone, c.sess.Ctx = rec.Done(), parserCtx
	go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(parserCtx, c.sess) }()
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	at.sent = time.Now()
	at.last = at.sent
	write(t, c.dest, c.dst, part)
	relayed(t, c.app, part)
	at.done = time.Now()
	awaitTaken(t, c.sess.DestStream, len(part))
	if held != nil {
		held(c.sess)
	}
	// The recording's context ends: RecordingDone is closed before the
	// contexts under it end, the parser's here and the relay's next.
	if way == parserRetired {
		cancelParser()
	} else {
		stop()
	}
	switch way {
	case relayEnds:
		c.stopRelay()
	case abortCloses:
		c.stopRelay()
		_ = c.sess.ClientStream.Close()
		_ = c.sess.DestStream.Close()
	case parserCtxEnds, parserRetired:
		at.last = time.Now()
		write(t, c.dest, c.dst, more)
		relayed(t, c.app, more)
		at.done = time.Now()
	}
	select {
	case err = <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return within 10s of the recording's stop")
	}
	if first, ended := c.sess.FirstPeerEnded(); ended {
		t.Fatalf("side %d ended the connection before recordV2 returned, want neither: the stop is not one with the connection open", first)
	}
	c.stopRelay()
	_ = c.app.Close()
	_ = c.dest.Close()
	<-c.relayDone
	for {
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		default:
			return c, at, mocks, err
		}
	}
}

// awaitTaken waits until the reader of stream has taken n bytes and waits
// for more, with nothing buffered for it.
func awaitTaken(t *testing.T, stream *fakeconn.FakeConn, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if waiting, _ := stream.Waiting(); waiting && stream.Consumed() == int64(n) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the parser took %d of %d bytes in 10s", stream.Consumed(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// stopCutMore is the next part of each stopCutParts response, and of a
// close-delimited one, which does not end it either.
var stopCutMore = map[string]string{
	"Content-Length body":     "56789",
	"chunked body":            "3\r\nfgh\r\n",
	"chunked body in a chunk": "fghij",
	"head":                    "ngth: 5\r\n",
	"close-delimited body":    "56789",
}

// stopCutAllParts is stopCutParts and a close-delimited body.
var stopCutAllParts = append(stopCutParts[:len(stopCutParts):len(stopCutParts)],
	struct{ name, part string }{"close-delimited body", "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n01234"})

func TestRecordV2_StopWithTheConnectionOpenLeavesTheCutAnswerOut(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, f := range stopCutAllParts {
			for _, way := range []openStopWay{relayEnds, abortCloses, parserCtxEnds} {
				for _, mark := range []bool{false, true} {
					t.Run(f.name+", "+way.String()+markName[mark]+switchName[proven], func(t *testing.T) {
						t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
						c, at, mocks, err := openStopCut(t, f.part, stopCutMore[f.name], way, mark)
						wantLeftOutForTheStop(t, c, at, mocks, err, supervisor.LeftOutStopCutOpen, mark)
					})
				}
			}
		}
	}
}

// stopReasons is each reason c's session left an exchange out for that is the
// recording's stop's.
func stopReasons(c *relayedConn) []string {
	var got []string
	for _, r := range leftOutReasons(c) {
		if strings.Contains(r, supervisor.LeftOutStopCut) || strings.Contains(r, supervisor.LeftOutStopCutOpen) ||
			strings.Contains(r, supervisor.LeftOutStopCutAfterEnd) {
			got = append(got, r)
		}
	}
	return got
}

// collect returns what c's recordV2 recorded once it has returned, and what
// it returned, with the connection's ends closed after.
func (c *relayedConn) collect(t *testing.T) ([]*models.Mock, error) {
	t.Helper()
	var err error
	select {
	case err = <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return within 10s")
	}
	_ = c.app.Close()
	_ = c.dest.Close()
	var mocks []*models.Mock
	for {
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		default:
			return mocks, err
		}
	}
}

// The upstream's end at the stop, then the stop ends the parser's read before
// it has read all the upstream sent: the supervisor's abort at the stop closes
// the streams before the relay ends them (it waits on the client's half of the
// connection), or the parser's context ends as it takes the next part. The
// exchange is left out for the stop (LeftOutStopCutAfterEnd): the stop cut
// the read, not the upstream, whose end never reached the parser. A
// close-delimited body read up to then is not known whole: its end, the
// upstream's close, never reached the parser.
func TestRecordV2_StopCutAtTheAbortAfterTheUpstreamsEnd(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, f := range stopCutAllParts {
			t.Run(f.name+", the abort's close"+switchName[proven], func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
				c := newRelayedConn(t)
				c.grace = time.Minute
				c.start(t)
				write(t, c.app, c.src, faultReq)
				relayed(t, c.dest, faultReq)
				at := cutTimes{sent: time.Now()}
				at.last = at.sent
				write(t, c.dest, c.dst, f.part)
				relayed(t, c.app, f.part)
				at.done = time.Now()
				awaitTaken(t, c.sess.DestStream, len(f.part))
				c.stopBegun.Store(true)
				_ = c.dest.Close()
				awaitPeerEnd(t, c, fakeconn.FromDest)
				_ = c.sess.ClientStream.Close()
				_ = c.sess.DestStream.Close()
				mocks, err := c.collect(t)
				wantLeftOutForTheStop(t, c, at, mocks, err, supervisor.LeftOutStopCutAfterEnd, false)
			})
			t.Run(f.name+", the parser's context"+switchName[proven], func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
				c := newRelayedConn(t)
				c.startRelay(t)
				ctx := &ctxEndHeld{Context: context.Background(), released: make(chan struct{})}
				t.Cleanup(func() { ctx.release() })
				go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, c.sess) }()
				write(t, c.app, c.src, faultReq)
				relayed(t, c.dest, faultReq)
				at := cutTimes{sent: time.Now()}
				write(t, c.dest, c.dst, f.part)
				relayed(t, c.app, f.part)
				awaitTaken(t, c.sess.DestStream, len(f.part))
				c.stopBegun.Store(true)
				ctx.ended.Store(true)
				// The parser takes the next part and checks its context,
				// which holds it there until the upstream's end, sent after
				// that part, has reached the relay.
				at.last = time.Now()
				write(t, c.dest, c.dst, stopCutMore[f.name])
				relayed(t, c.app, stopCutMore[f.name])
				at.done = time.Now()
				_ = c.dest.Close()
				awaitPeerEnd(t, c, fakeconn.FromDest)
				ctx.release()
				mocks, err := c.collect(t)
				wantLeftOutForTheStop(t, c, at, mocks, err, supervisor.LeftOutStopCutAfterEnd, false)
			})
		}
	}
}

// ctxEndHeld is a parser's context whose end the test holds: once ended, Err
// waits until release and then reports context.Canceled. The parser checks
// its context before each read of the response, so the end holds the parser
// after the chunk it took last.
type ctxEndHeld struct {
	context.Context
	ended    atomic.Bool
	released chan struct{}
	once     sync.Once
}

func (c *ctxEndHeld) Err() error {
	if !c.ended.Load() {
		return nil
	}
	<-c.released
	return context.Canceled
}

func (c *ctxEndHeld) release() { c.once.Do(func() { close(c.released) }) }

// Not the stop's cut: what the parser read when its context ended without the
// recording's (the supervisor retired it) is returned as before, with no
// ERROR; a response that does not decode once the stop has begun is not left
// out for the stop; nor is one the client's end, or the upstream's end before
// the stop began, ended while the recording was stopping: the upstream's is
// its fault, recorded.
func TestRecordV2_OpenStopCutOnlyForTheRecordingsStop(t *testing.T) {
	const part = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n01234"
	t.Run("the parser retired", func(t *testing.T) {
		c, _, mocks, err := openStopCut(t, part, "56789", parserRetired, false)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("recordV2 = %v, want the parser's context's end, as before", err)
		}
		if len(mocks) != 0 || len(stopReasons(c)) != 0 || len(errorsLogged(c)) != 0 {
			t.Errorf("recorded %d mocks, left out for %q, logged %q at ERROR; want none of them", len(mocks), stopReasons(c), errorsLogged(c))
		}
	})
	for _, way := range []openStopWay{relayEnds, abortCloses, parserCtxEnds} {
		t.Run("a capture hole, the recording stopping at "+way.String(), func(t *testing.T) {
			c, _, _, _ := openStopCutHolding(t, part, "56789", way, func(s *supervisor.Session) {
				s.MarkEndedAtHole(fakeconn.FromDest, "test hole")
			})
			if got := stopReasons(c); len(got) != 0 {
				t.Errorf("left out for %q, want no stop's reason: the capture lost the rest of the response", got)
			}
		})
	}
	t.Run("a chunk size that does not decode, once the stop began", func(t *testing.T) {
		c, _, _, _ := openStopCut(t, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nabcde\r\n", "zz\r\nabc", parserCtxEnds, false)
		if got := stopReasons(c); len(got) != 0 {
			t.Errorf("left out for %q, want no stop's reason: the response does not decode", got)
		}
	})
	t.Run("the client closed first, the recording stopping", func(t *testing.T) {
		c := newRelayedConn(t)
		c.startRelay(t)
		stopped := make(chan struct{})
		close(stopped)
		c.sess.RecordingDone = stopped
		c.startParser(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		write(t, c.dest, c.dst, part)
		relayed(t, c.app, part)
		c.stopBegun.Store(true)
		appClosed(t, c)
		_, _ = c.finish(t)
		if got := stopReasons(c); len(got) != 0 {
			t.Errorf("left out for %q, want another reason: the client's end came first", got)
		}
		if got := leftOutReasons(c); len(got) != 1 || got[0] != supervisor.LeftOutPartwayAtStop {
			t.Errorf("left out for %q, want once for %q: the stop closed the application first", got, supervisor.LeftOutPartwayAtStop)
		}
	})
	t.Run("the upstream closed before the stop began", func(t *testing.T) {
		c := newRelayedConn(t)
		c.grace = time.Minute
		c.startRelay(t)
		rec, stop := context.WithCancel(context.Background())
		t.Cleanup(stop)
		c.sess.RecordingDone = rec.Done()
		c.startParser(t)
		write(t, c.app, c.src, faultReq)
		relayed(t, c.dest, faultReq)
		write(t, c.dest, c.dst, part)
		relayed(t, c.app, part)
		_ = c.dest.Close()
		awaitPeerEnd(t, c, fakeconn.FromDest)
		// The relay holds the stream open on the client's half; the
		// recording stops before the parser reads its end.
		stop()
		mocks, err := c.finish(t)
		if got := stopReasons(c); len(got) != 0 {
			t.Errorf("left out for %q, want none: the upstream's end came before the stop", got)
		}
		if m := oneMock(t, mocks, err); m.Spec.Fault == nil || m.Spec.Fault.Kind != models.FaultTruncate {
			t.Errorf("recorded the fault %v, want the upstream's truncation", m.Spec.Fault)
		}
	})
}

// The client closed its socket with only the chunked body's last chunk to
// come, which the upstream had sent before the client's FIN reached the
// proxy, and then the upstream closed, once the recording's stop had begun:
// the client had every byte of content, and the response is whole
// (chunkedEndRefused). Where the relay's client forwarder is not run as the
// client's FIN comes in, as on a busy host, the upstream's end is read and
// marked first, and as the stop's (EndFollowedStop). The
// stream still ended where the client stopped, partway through the framing
// alone, so the exchange is recorded whole, with no fault, and not left out
// for the stop.
func TestRecordV2_TheUpstreamsEndAtTheStopAfterTheClientStoppedKeepsAWholeAnswer(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"
	const data = `{"id":"ch_1","status":"pending"}`
	chunk := "20\r\n" + data + "\r\n"
	for _, proven := range []bool{false, true} {
		for _, f := range []struct{ name, read, after, body string }{
			{name: "the last chunk and the empty line after it", read: head + chunk, after: "0\r\n\r\n", body: data},
			{name: "the empty line alone", read: head + chunk + "0\r\n", after: "\r\n", body: data},
			{name: "the last chunk of an empty body", read: head, after: "0\r\n\r\n"},
		} {
			t.Run(f.name+switchName[proven], func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
				c := newRelayedConn(t)
				held := &readsHeldAfter{n: len(f.read), release: make(chan struct{})}
				c.wrapDst = func(u net.Conn) net.Conn { held.Conn = u; return held }
				clientEnd := &endHeldUntil{ended: make(chan struct{}), until: func() bool {
					return c.sess.PeerEnded(fakeconn.FromDest) != supervisor.PeerOpen
				}}
				c.wrapSrc = func(u net.Conn) net.Conn { clientEnd.Conn = u; return clientEnd }
				c.start(t)
				// Before the relay's own cleanup waits for it to end.
				t.Cleanup(held.let)
				write(t, c.app, c.src, faultReq)
				relayed(t, c.dest, faultReq)
				if _, err := c.dest.Write([]byte(f.read)); err != nil {
					t.Fatal(err)
				}
				relayed(t, c.app, f.read)
				// At the proxy's socket, unread, and the upstream's FIN
				// behind it: the relay's forwarder reads them only once the
				// client's FIN is in.
				write(t, c.dest, c.dst, f.after)
				if err := c.dest.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				c.stopBegun.Store(true)
				if err := c.app.Close(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-clientEnd.ended:
				case <-time.After(10 * time.Second):
					t.Fatal("the relay did not read the client's end within 10s")
				}
				held.let()
				awaitPeerEnd(t, c, fakeconn.FromClient)
				if first, _ := c.sess.FirstPeerEnded(); first != fakeconn.FromDest || !c.sess.EndFollowedStop() {
					t.Fatalf("first end %d, the stop's %v; want the upstream's, at the stop", first, c.sess.EndFollowedStop())
				}
				mocks := c.end(t)
				if !c.sess.EndedWhereClientStopped() {
					t.Fatal("the relay did not say the destination's stream ended where the client stopped")
				}
				if len(mocks) != 1 {
					t.Fatalf("recorded %d mocks, left out for %q; want the one exchange: the client took every byte the response carries", len(mocks), leftOutReasons(c))
				}
				if got := mocks[0].Spec; got.HTTPResp.StatusCode != 200 || got.HTTPResp.Body != f.body || got.Fault != nil {
					t.Fatalf("recorded a %d with the body %q, fault %v; want a 200 with %q, no fault", got.HTTPResp.StatusCode, got.HTTPResp.Body, got.Fault, f.body)
				}
				if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
					t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
				}
				if w := c.warnings(); len(w) != 0 {
					t.Fatalf("logged at %v: %q", w[0].Level, w[0].Message)
				}
			})
		}
	}
}

// endHeldUntil is the proxy's end of the client connection as the relay reads
// it, whose read that finds the client's end (io.EOF) returns it only once
// until reports true, or 10s on: the relay's client forwarder is not run
// meanwhile. ended is closed as that read finds the end.
type endHeldUntil struct {
	net.Conn
	until func() bool
	ended chan struct{}
	once  sync.Once
}

func (c *endHeldUntil) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if errors.Is(err, io.EOF) {
		c.once.Do(func() { close(c.ended) })
		for limit := time.Now().Add(10 * time.Second); !c.until() && time.Now().Before(limit); {
			time.Sleep(time.Millisecond)
		}
	}
	return n, err
}

// NetConn is the conn under it, whose socket the relay looks at.
func (c *endHeldUntil) NetConn() net.Conn { return c.Conn }

// The client's end partway through the response, with the upstream still
// sending: the relay's write after the client's FIN met the closed socket, so
// the response's stream ended where the client stopped taking it
// (Session.EndedWhereClientStopped), as for a slow or streaming answer in
// flight as the recording stops the application. An end that records no fault
// (Session.FirstEndNoFault) says which end it was, as where the upstream sends
// nothing more (partAnswerLeftOut): the recording's stop closing the
// application is the stop's cut (supervisor.LeftOutPartwayAtStop, class
// at_stop), not the client's giving up; the client's process's exit, or an end
// the hooks cannot judge, takes its own reason. The exchange is left out once,
// with its span, under the mark of a chunk the capture lost when there is one,
// and nothing is logged at ERROR.
func TestRecordV2_TheClientStoppedPartwaySaysWhichEndItWas(t *testing.T) {
	body := strings.Repeat("0123456789", 200)
	framings := []struct{ name, head string }{
		{"Content-Length", "HTTP/1.1 200 OK\r\nContent-Length: 2000\r\n\r\n"},
		{"close-delimited", "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"},
	}
	ends := []struct {
		name   string
		why    supervisor.NoFaultEnd
		reason string
	}{
		{"the recording's stop closed the application", supervisor.EndAtStop, supervisor.LeftOutPartwayAtStop},
		{"the client's process exited on its own", supervisor.EndAtExit, supervisor.LeftOutPartwayAtExit},
		{"the hooks cannot judge the client's end", supervisor.EndUnjudged, supervisor.LeftOutPartwayUnjudged},
	}
	for _, proven := range []bool{false, true} {
		for _, e := range ends {
			for _, f := range framings {
				for _, mark := range []bool{false, true} {
					t.Run(e.name+", "+f.name+markName[mark]+switchName[proven], func(t *testing.T) {
						t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
						c := newRelayedConn(t)
						c.start(t)
						write(t, c.app, c.src, faultReq)
						relayed(t, c.dest, faultReq)
						read := f.head + body[:500]
						at := cutTimes{sent: time.Now()}
						at.last = at.sent
						write(t, c.dest, c.dst, read)
						relayed(t, c.app, read)
						at.done = time.Now()
						if mark {
							c.sess.MarkMockIncomplete("per_conn_cap")
						}
						if e.why == supervisor.EndAtStop {
							c.stopBegun.Store(true)
						} else {
							c.clientEnd.Store(uint32(e.why))
						}
						_ = c.app.Close()
						awaitFIN(t, c.dest)
						if _, err := c.dest.Write([]byte(body[500:])); err != nil {
							t.Fatal(err)
						}
						mocks, err := c.finish(t)
						if !c.sess.EndedWhereClientStopped() {
							t.Fatal("the response's stream did not end where the client stopped taking it")
						}
						wantLeftOutForTheStop(t, c, at, mocks, err, e.reason, mark)
					})
				}
			}
		}
	}
}

// The client's end partway through the response, by a FIN or a reset, once the
// recording's stop had begun (the stop closed the application with the call in
// flight), and then the stop ends the parser's read before the response's
// stream ends: the
// supervisor's abort at the stop closes the streams while the relay still
// waits on the upstream's half of the connection (fakeconn.ErrClosed), with
// the recording's end seen by the session or not, or the parser's context
// ends as it takes the next part. The response is not whole, framed or
// close-delimited, so the exchange is left out once as the stop's cut
// (supervisor.LeftOutPartwayAtStop), with its span: not a response that does
// not decode, with an ERROR for a normal stop, nor a close-delimited body
// recorded whole as far as it came.
func TestRecordV2_StopCutAtTheAbortAfterTheClientsEndAtTheStop(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, f := range stopCutAllParts {
			for _, end := range clientEnds {
				for _, done := range []bool{false, true} {
					name := f.name + end.name + ", the abort's close" + map[bool]string{false: "", true: ", the recording done"}[done] + switchName[proven]
					t.Run(name, func(t *testing.T) {
						t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
						c := newRelayedConn(t)
						c.grace = time.Minute
						c.startRelay(t)
						if done {
							stopped := make(chan struct{})
							close(stopped)
							c.sess.RecordingDone = stopped
						}
						c.startParser(t)
						write(t, c.app, c.src, faultReq)
						relayed(t, c.dest, faultReq)
						at := cutTimes{sent: time.Now()}
						at.last = at.sent
						write(t, c.dest, c.dst, f.part)
						relayed(t, c.app, f.part)
						at.done = time.Now()
						awaitTaken(t, c.sess.DestStream, len(f.part))
						c.stopBegun.Store(true)
						end.end(t, c)
						awaitPeerEnd(t, c, fakeconn.FromClient)
						_ = c.sess.ClientStream.Close()
						_ = c.sess.DestStream.Close()
						mocks, err := c.collect(t)
						wantLeftOutForTheStop(t, c, at, mocks, err, supervisor.LeftOutPartwayAtStop, false)
					})
				}
			}
			for _, end := range clientEnds {
				t.Run(f.name+end.name+", the parser's context"+switchName[proven], func(t *testing.T) {
					t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
					c := newRelayedConn(t)
					c.grace = time.Minute
					c.startRelay(t)
					ctx := &ctxEndHeld{Context: context.Background(), released: make(chan struct{})}
					t.Cleanup(func() { ctx.release() })
					go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, c.sess) }()
					write(t, c.app, c.src, faultReq)
					relayed(t, c.dest, faultReq)
					at := cutTimes{sent: time.Now()}
					write(t, c.dest, c.dst, f.part)
					relayed(t, c.app, f.part)
					awaitTaken(t, c.sess.DestStream, len(f.part))
					c.stopBegun.Store(true)
					ctx.ended.Store(true)
					// The parser takes the next part and checks its context,
					// which holds it there until the client's end, at the stop,
					// has reached the relay.
					at.last = time.Now()
					write(t, c.dest, c.dst, stopCutMore[f.name])
					relayed(t, c.app, stopCutMore[f.name])
					at.done = time.Now()
					end.end(t, c)
					awaitPeerEnd(t, c, fakeconn.FromClient)
					ctx.release()
					mocks, err := c.collect(t)
					wantLeftOutForTheStop(t, c, at, mocks, err, supervisor.LeftOutPartwayAtStop, false)
				})
			}
		}
	}
}

// clientEnds are the ways the client ends its side of the connection: its
// close (a FIN, forwarded to the upstream) or its reset.
var clientEnds = []struct {
	name string
	end  func(*testing.T, *relayedConn)
}{
	{"", appClosed},
	{", the client's reset", func(t *testing.T, c *relayedConn) { t.Helper(); resetConn(t, c.app) }},
}

// Not the stop's cut: a response stream that ended at a capture hole is the
// hole's, though the client's end at the stop came first and the stop's abort
// then closed the stream: the capture lost the rest, and the stop did not cut
// what it has.
func TestRecordV2_TheClientsEndAtTheStopIsNotAHolesCut(t *testing.T) {
	const part = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n01234"
	c := newRelayedConn(t)
	c.grace = time.Minute
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, part)
	relayed(t, c.app, part)
	awaitTaken(t, c.sess.DestStream, len(part))
	c.sess.MarkEndedAtHole(fakeconn.FromDest, "test hole")
	c.stopBegun.Store(true)
	appClosed(t, c)
	awaitPeerEnd(t, c, fakeconn.FromClient)
	_ = c.sess.ClientStream.Close()
	_ = c.sess.DestStream.Close()
	_, _ = c.collect(t)
	for _, r := range leftOutReasons(c) {
		if strings.Contains(r, supervisor.LeftOutPartwayAtStop) {
			t.Errorf("left out for %q, want the hole's handling: the capture lost the rest of the response", r)
		}
	}
}

// The client's end partway through the response, before the recording's stop
// began: the application gave up on the call (its timeout), its process
// exited on its own, or the hooks cannot judge its end (macOS, Windows). Then
// the relay waits on the upstream's half of the connection, and either the
// upstream closes it with no stop, or the supervisor's abort at the stop
// closes the streams first (with the recording's end seen by the session or
// not). The abort ends only that wait: the exchange is judged by the client's
// end, the same with the stop as without it. Its process's exit, or an end the
// hooks cannot judge, leaves the exchange out once with that end's reason and
// its span; its giving up records its timeout, after the bytes of the answer
// it had (on the wire, undecoded), or with no answer where the answer's head
// had not arrived whole. Never a response that does not decode, with an ERROR,
// and never a close-delimited body recorded whole as far as it came.
func TestRecordV2_TheClientsEndBeforeTheStopJudgesTheAbortsCut(t *testing.T) {
	ends := []struct {
		name   string
		why    supervisor.NoFaultEnd
		reason string
	}{
		{"the application gave up", supervisor.EndMayBeFault, ""},
		{"the client's process exited on its own", supervisor.EndAtExit, supervisor.LeftOutPartwayAtExit},
		{"the hooks cannot judge the client's end", supervisor.EndUnjudged, supervisor.LeftOutPartwayUnjudged},
	}
	ways := []string{"the upstream's close, no stop", "the abort's close", "the abort's close, the recording done"}
	for _, proven := range []bool{false, true} {
		for _, e := range ends {
			for _, f := range stopCutAllParts {
				for _, way := range ways {
					t.Run(e.name+", "+f.name+", "+way+switchName[proven], func(t *testing.T) {
						t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
						c := newRelayedConn(t)
						c.grace = time.Minute
						c.startRelay(t)
						if way == ways[2] {
							stopped := make(chan struct{})
							close(stopped)
							c.sess.RecordingDone = stopped
						}
						c.startParser(t)
						write(t, c.app, c.src, faultReq)
						relayed(t, c.dest, faultReq)
						at := cutTimes{sent: time.Now()}
						at.last = at.sent
						write(t, c.dest, c.dst, f.part)
						relayed(t, c.app, f.part)
						at.done = time.Now()
						awaitTaken(t, c.sess.DestStream, len(f.part))
						c.clientEnd.Store(uint32(e.why))
						appClosed(t, c)
						awaitPeerEnd(t, c, fakeconn.FromClient)
						if way == ways[0] {
							_ = c.dest.Close()
						} else {
							_ = c.sess.ClientStream.Close()
							_ = c.sess.DestStream.Close()
						}
						mocks, err := c.collect(t)
						if e.reason != "" {
							wantLeftOutForTheStop(t, c, at, mocks, err, e.reason, false)
							return
						}
						wantTheClientsTimeout(t, c, f.part, mocks, err)
					})
				}
			}
		}
	}
}

// wantTheClientsTimeout checks that c's call, whose response was cut by the
// application's giving up after it had part, was recorded once with its
// timeout: after part's head and the body bytes part carries, as on the wire,
// or with no answer where part has no whole head. Nothing is left out, and no
// error is returned or logged.
func wantTheClientsTimeout(t *testing.T, c *relayedConn, part string, mocks []*models.Mock, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("recordV2 = %v, want nil: the application's timeout is a fault to record, not a decode error", err)
	}
	if got := errorsLogged(c); len(got) != 0 {
		t.Errorf("logged %q at ERROR, want nothing", got)
	}
	if got := leftOutReasons(c); len(got) != 0 {
		t.Errorf("left out for %q, want the call recorded with its timeout", got)
	}
	if len(mocks) != 1 {
		t.Fatalf("recorded %d mocks, want 1", len(mocks))
	}
	f := mocks[0].Spec.Fault
	if f == nil || f.Kind != models.FaultTimeout {
		t.Fatalf("recorded the fault %v, want the application's timeout", f)
	}
	i := strings.Index(part, "\r\n\r\n")
	if i < 0 {
		if f.AfterHead || f.BodyBytes != 0 {
			t.Errorf("recorded the fault %v, want it with no answer: the answer's head never arrived whole", f)
		}
		return
	}
	body := part[i+4:]
	if !f.AfterHead || f.BodyBytes != int64(len(body)) {
		t.Errorf("recorded the fault %v, want it after the head and %d body bytes", f, len(body))
	}
	if r := mocks[0].Spec.HTTPResp; r == nil || r.Body != body {
		t.Errorf("recorded the response %+v, want the body as it came on the wire, %q", r, body)
	}
}

// The ways the recording's stop ends a parser's read once a side's end came
// first, before the stop began, while the relay waits on the other side's
// half: the supervisor's abort closes the streams (the recording's context
// not ended, as the session saw it), or, as at the stop, the parser's
// context (the session's) ends and the abort closes the streams after it
// while the parser waits in a read, or the parser's context ends as the
// parser takes the next part of the response.
type firstEndStop int

const (
	theAbortsClose firstEndStop = iota
	theCtxThenTheAbort
	theParsersCtx
)

func (w firstEndStop) String() string {
	return [...]string{"the abort's close", "the context's end, then the abort's close", "the parser's context"}[w]
}

// firstEndThenStop runs c's call: part of the answer, then the first end
// (end, before the recording's stop), then the stop's end of the read (way).
// Where the parser's context ends, it is the session's, as the supervisor
// makes it (Session.Ctx), and mocks go through the sync-mock manager, as the
// proxy routes them, so it waits for want mocks the manager hands on. It
// returns what the parser read of the answer, and when the request was sent
// and the last part of it the parser read was written.
func firstEndThenStop(t *testing.T, c *relayedConn, part, more string, way firstEndStop, want int, end func()) (read string, at cutTimes, mocks []*models.Mock, err error) {
	t.Helper()
	c.grace = time.Minute
	c.startRelay(t)
	var held *ctxEndHeld
	var cancel context.CancelFunc
	switch way {
	case theAbortsClose:
		c.startParser(t)
	case theCtxThenTheAbort, theParsersCtx:
		var ctx context.Context
		if way == theParsersCtx {
			held = &ctxEndHeld{Context: context.Background(), released: make(chan struct{})}
			t.Cleanup(held.release)
			ctx = held
		} else {
			ctx, cancel = context.WithCancel(context.Background())
			t.Cleanup(cancel)
		}
		c.sess.Ctx = ctx
		c.mgr.SetOutputChannel(c.mocks)
		c.sess.RouteMocksViaSyncMock = true
		go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, c.sess) }()
	}
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	at = cutTimes{sent: time.Now()}
	at.last = at.sent
	write(t, c.dest, c.dst, part)
	relayed(t, c.app, part)
	at.done = time.Now()
	awaitTaken(t, c.sess.DestStream, len(part))
	read = part
	if way == theParsersCtx {
		// The parser takes the next part and checks its context, which
		// holds it there until the first end has reached the relay.
		held.ended.Store(true)
		at.last = time.Now()
		write(t, c.dest, c.dst, more)
		relayed(t, c.app, more)
		at.done = time.Now()
		read += more
	}
	end()
	switch way {
	case theAbortsClose:
		_ = c.sess.ClientStream.Close()
		_ = c.sess.DestStream.Close()
	case theCtxThenTheAbort:
		cancel()
		_ = c.sess.ClientStream.Close()
		_ = c.sess.DestStream.Close()
	case theParsersCtx:
		held.release()
	}
	mocks, err = c.collect(t)
	for len(mocks) < want {
		// The sync-mock manager may hand a mock on after the parser
		// returned.
		select {
		case m := <-c.mocks:
			mocks = append(mocks, m)
		case <-time.After(10 * time.Second): // turns a lost mock into a failure
			return read, at, mocks, err
		}
	}
	return read, at, mocks, err
}

// The client's end partway through the response, before the recording's stop
// began, then the stop as it ends a parser's read: the parser's context, the
// session's, ends, and the supervisor's abort closes the streams after it
// while the parser waits in a read; or the parser's context ends as it takes
// the next part. The exchange is judged by the client's end, as when the
// abort's close ends the read with no context's end: left out once with its
// span for its process's exit or an end the hooks cannot judge; recorded
// with the application's timeout (after the bytes of the answer it had, or
// with no answer where the answer's head had not arrived whole) for its
// giving up, though the session's context has ended. Never the context's end
// returned with nothing recorded and nothing left out, and never a response
// that does not decode, with an ERROR.
func TestRecordV2_TheClientsEndBeforeTheStopJudgesTheContextsEnd(t *testing.T) {
	ends := []struct {
		name   string
		why    supervisor.NoFaultEnd
		reason string
	}{
		{"the application gave up", supervisor.EndMayBeFault, ""},
		{"the client's process exited on its own", supervisor.EndAtExit, supervisor.LeftOutPartwayAtExit},
		{"the hooks cannot judge the client's end", supervisor.EndUnjudged, supervisor.LeftOutPartwayUnjudged},
	}
	for _, proven := range []bool{false, true} {
		for _, e := range ends {
			for _, f := range stopCutAllParts {
				for _, way := range []firstEndStop{theCtxThenTheAbort, theParsersCtx} {
					t.Run(e.name+", "+f.name+", "+way.String()+switchName[proven], func(t *testing.T) {
						t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
						c := newRelayedConn(t)
						read, at, mocks, err := firstEndThenStop(t, c, f.part, stopCutMore[f.name], way, map[bool]int{false: 1, true: 0}[e.reason != ""], func() {
							c.clientEnd.Store(uint32(e.why))
							appClosed(t, c)
							awaitPeerEnd(t, c, fakeconn.FromClient)
						})
						if e.reason != "" {
							wantLeftOutForTheStop(t, c, at, mocks, err, e.reason, false)
							return
						}
						wantTheClientsTimeout(t, c, read, mocks, err)
					})
				}
			}
		}
	}
}

// The upstream's close partway through the response, before the recording's
// stop began, then the stop ends the parser's read while the relay waits on
// the client's half: the supervisor's abort closes the streams, with the
// parser's context (the session's) ended before it or not, or the parser's
// context ends as it takes the next part. The exchange is judged by the
// upstream's close, as at the stream's end with no stop: recorded once with
// the upstream's truncation, after the head and the body bytes the parser
// read, as on the wire; a close-delimited body, which the close ends, is
// recorded whole. Never a response that does not decode, with an ERROR, nor
// the context's end returned with nothing recorded, and nothing is left out.
func TestRecordV2_TheUpstreamsEndBeforeTheStopJudgesTheStopsCut(t *testing.T) {
	parts := append(stopCutParts[:3:3], stopCutAllParts[len(stopCutAllParts)-1])
	for _, proven := range []bool{false, true} {
		for _, f := range parts {
			for _, way := range []firstEndStop{theAbortsClose, theCtxThenTheAbort, theParsersCtx} {
				t.Run(f.name+", "+way.String()+switchName[proven], func(t *testing.T) {
					t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
					c := newRelayedConn(t)
					read, _, mocks, err := firstEndThenStop(t, c, f.part, stopCutMore[f.name], way, 1, func() {
						_ = c.dest.Close()
						awaitPeerEnd(t, c, fakeconn.FromDest)
					})
					if err != nil {
						t.Errorf("recordV2 = %v, want nil: the upstream's close is a fault to record, not a decode error", err)
					}
					if got := errorsLogged(c); len(got) != 0 {
						t.Errorf("logged %q at ERROR, want nothing", got)
					}
					if got := leftOutReasons(c); len(got) != 0 {
						t.Errorf("left out for %q, want the call recorded as the upstream gave it", got)
					}
					if len(mocks) != 1 {
						t.Fatalf("recorded %d mocks, want 1", len(mocks))
					}
					body := read[strings.Index(read, "\r\n\r\n")+4:]
					fault, r := mocks[0].Spec.Fault, mocks[0].Spec.HTTPResp
					if f.name == "close-delimited body" {
						if fault != nil || r == nil || r.Body != body {
							t.Errorf("recorded the response %+v with the fault %v, want it whole, %q, with no fault: the upstream's close ends it", r, fault, body)
						}
						return
					}
					if fault == nil || fault.Kind != models.FaultTruncate || fault.BodyBytes != int64(len(body)) {
						t.Fatalf("recorded the fault %v, want the upstream's truncation after %d body bytes", fault, len(body))
					}
					if r == nil || r.Body != body {
						t.Errorf("recorded the response %+v, want the body as it came on the wire, %q", r, body)
					}
				})
			}
		}
	}
}

// Not judged by the first end: the parser's context ended after the client's
// end, before the recording's stop, with the response's stream ended at a
// capture hole: the capture lost the rest, and the read is returned as
// before, with no ERROR and no client's reason.
func TestRecordV2_TheContextsEndAtAHoleIsNotTheClients(t *testing.T) {
	const part = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n01234"
	c := newRelayedConn(t)
	_, _, mocks, err := firstEndThenStop(t, c, part, "56789", theParsersCtx, 0, func() {
		c.sess.MarkEndedAtHole(fakeconn.FromDest, "test hole")
		c.clientEnd.Store(uint32(supervisor.EndAtExit))
		appClosed(t, c)
		awaitPeerEnd(t, c, fakeconn.FromClient)
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("recordV2 = %v, want the parser's context's end, as before", err)
	}
	if len(mocks) != 0 || len(leftOutReasons(c)) != 0 || len(errorsLogged(c)) != 0 {
		t.Errorf("recorded %d mocks, left out for %q, logged %q at ERROR; want none of them", len(mocks), leftOutReasons(c), errorsLogged(c))
	}
}

// Not a cut: a parser behind its capture reads a whole response after the
// client's end, which came once the application had it (its giving up, the
// recording's stop, its process's exit, or an end the hooks cannot judge),
// and before the upstream's close. The read ended with the response's
// framing, not at the stream's end nor at its reader's close, so the client's
// end does not judge it: the exchange is
// recorded whole, with no fault, and nothing is left out.
func TestRecordV2_AWholeAnswerReadAfterTheClientsEndIsRecorded(t *testing.T) {
	const answer = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n01234"
	ends := []struct {
		name string
		why  supervisor.NoFaultEnd
	}{
		{"the application gave up", supervisor.EndMayBeFault},
		{"the recording's stop closed the application", supervisor.EndAtStop},
		{"the client's process exited on its own", supervisor.EndAtExit},
		{"the hooks cannot judge the client's end", supervisor.EndUnjudged},
	}
	for _, proven := range []bool{false, true} {
		for _, e := range ends {
			t.Run(e.name+switchName[proven], func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
				c := newRelayedConn(t)
				c.grace = time.Minute
				c.startRelay(t)
				write(t, c.app, c.src, faultReq)
				relayed(t, c.dest, faultReq)
				write(t, c.dest, c.dst, answer)
				relayed(t, c.app, answer)
				if e.why == supervisor.EndAtStop {
					c.stopBegun.Store(true)
				} else {
					c.clientEnd.Store(uint32(e.why))
				}
				appClosed(t, c)
				awaitPeerEnd(t, c, fakeconn.FromClient)
				c.startParser(t)
				// The upstream's close ends the connection, so the parser's
				// wait for a next request ends.
				_ = c.dest.Close()
				mocks, err := c.collect(t)
				if err != nil {
					t.Errorf("recordV2 = %v, want nil", err)
				}
				if got := leftOutReasons(c); len(got) != 0 {
					t.Errorf("left out for %q, want the whole exchange recorded", got)
				}
				if got := errorsLogged(c); len(got) != 0 {
					t.Errorf("logged %q at ERROR, want nothing", got)
				}
				if len(mocks) != 1 {
					t.Fatalf("recorded %d mocks, want 1", len(mocks))
				}
				if r := mocks[0].Spec.HTTPResp; mocks[0].Spec.Fault != nil || r == nil || r.Body != "01234" {
					t.Errorf("recorded the response %+v with the fault %v, want it whole with no fault", r, mocks[0].Spec.Fault)
				}
			})
		}
	}
}

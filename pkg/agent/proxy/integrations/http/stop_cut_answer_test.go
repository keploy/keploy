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

// Not a cut: the parser read a whole response as its context ended, with no
// side's end. The exchange it read is recorded whole: the session's context is
// the parser's, and EmitMock's check of it would drop the mock with nothing
// reporting it. The connection did not end, so recordV2 returns the context's
// end, as before.
func TestRecordV2_AWholeAnswerReadAsTheContextEndsIsRecorded(t *testing.T) {
	const part, rest = "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n01234", "56789"
	for _, proven := range []bool{false, true} {
		t.Run(strings.TrimPrefix(switchName[proven], ", "), func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
			c := newRelayedConn(t)
			_, _, mocks, err := firstEndThenStop(t, c, part, rest, theParsersCtx, 1, func() {})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("recordV2 = %v, want the parser's context's end: the connection did not end", err)
			}
			if got := leftOutReasons(c); len(got) != 0 {
				t.Errorf("left out for %q, want the whole exchange recorded", got)
			}
			if got := errorsLogged(c); len(got) != 0 {
				t.Errorf("logged %q at ERROR, want nothing", got)
			}
			if len(mocks) != 1 {
				t.Fatalf("recorded %d mocks, want the whole exchange once", len(mocks))
			}
			if f, r := mocks[0].Spec.Fault, mocks[0].Spec.HTTPResp; f != nil || r == nil || r.Body != "0123456789" {
				t.Errorf("recorded the response %+v with the fault %v, want it whole, with no fault", r, f)
			}
		})
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

// The upstream's end partway through the response's head, before the
// recording's stop or with none: the application got part of a head, then the
// end, and no recorded fault replays that (a fault sends a head only whole,
// and one with no answer replays as an end before any byte, which an HTTP
// client retries on a reused connection). So the call is left out, once, with
// its span and LeftOutHeadCut: never recorded as a fault with no answer, nor
// as a response that does not decode, with an ERROR. Read at the stream's
// end, at the stop's abort's close after it, or with the parser's context
// ending after it; on the connection's first request and on a later one,
// whose earlier answer is recorded whole.
func TestRecordV2_TheUpstreamsEndInTheHeadLeavesTheCallOut(t *testing.T) {
	const part, more = "HTTP/1.1 200 OK\r\nContent-Le", "ngth: 5\r\n"
	const whole = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	ends := []struct {
		name  string
		reset bool
	}{{"closed", false}, {"reset", true}}
	end := func(t *testing.T, c *relayedConn, reset bool) {
		if reset {
			resetConn(t, c.dest)
		} else {
			_ = c.dest.Close()
		}
		awaitPeerEnd(t, c, fakeconn.FromDest)
	}
	check := func(t *testing.T, c *relayedConn, at cutTimes, mocks []*models.Mock, err error, n int) {
		t.Helper()
		if err != nil {
			t.Errorf("recordV2 = %v, want nil: a head the upstream cut short is no decode error", err)
		}
		if got := errorsLogged(c); len(got) != 0 {
			t.Errorf("logged %q at ERROR, want nothing", got)
		}
		if got := leftOutReasons(c); len(got) != 1 || got[0] != supervisor.LeftOutHeadCut {
			t.Errorf("left out for %q, want once for %q", got, supervisor.LeftOutHeadCut)
		}
		if got := c.mgr.MocksLeftOut(); got != 1 {
			t.Errorf("left out %d mocks, want 1", got)
		}
		if len(mocks) != n {
			t.Fatalf("recorded %d mocks, want %d: the earlier calls' alone", len(mocks), n)
		}
		for _, m := range mocks {
			if m.Spec.Fault != nil || m.Spec.HTTPResp == nil || m.Spec.HTTPResp.Body != "ok" {
				t.Errorf("recorded %+v with the fault %v, want the earlier call's whole answer", m.Spec.HTTPResp, m.Spec.Fault)
			}
		}
		spans := c.spans.all()
		if len(spans) != 1 || spans[0][0].IsZero() || !spans[0][0].Before(at.sent) ||
			spans[0][1].Before(at.last) || spans[0][1].After(at.done) {
			t.Errorf("spans %v, want one, from the request's time (before %v) to the head's last chunk the parser read "+
				"(stamped from %v to %v)", spans, at.sent, at.last, at.done)
		}
	}
	for _, proven := range []bool{false, true} {
		for _, e := range ends {
			for _, later := range []bool{false, true} {
				name := e.name + map[bool]string{false: "", true: ", a later request"}[later] + ", the stream's end" + switchName[proven]
				t.Run(name, func(t *testing.T) {
					t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
					c := newRelayedConn(t)
					c.start(t)
					n := 0
					if later {
						write(t, c.app, c.src, faultReq)
						relayed(t, c.dest, faultReq)
						write(t, c.dest, c.dst, whole)
						relayed(t, c.app, whole)
						n = 1
					}
					write(t, c.app, c.src, faultReq)
					relayed(t, c.dest, faultReq)
					at := cutTimes{sent: time.Now()}
					at.last = at.sent
					write(t, c.dest, c.dst, part)
					relayed(t, c.app, part)
					at.done = time.Now()
					end(t, c, e.reset)
					mocks, err := c.finish(t)
					check(t, c, at, mocks, err, n)
				})
			}
			for _, way := range []firstEndStop{theAbortsClose, theCtxThenTheAbort, theParsersCtx} {
				t.Run(e.name+", "+way.String()+switchName[proven], func(t *testing.T) {
					t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
					c := newRelayedConn(t)
					_, at, mocks, err := firstEndThenStop(t, c, part, more, way, 0, func() { end(t, c, e.reset) })
					check(t, c, at, mocks, err, 0)
				})
			}
		}
	}
}

// The parser's context, the session's, ends after the parser last asked it
// and before the emit asks it, here as the post-record hook runs: the mock of
// the exchange read whole is recorded, not dropped with nothing reporting it
// (Session.EmitMockAcrossShutdown).
func TestRecordV2_AMockEmittedAsTheContextEndsIsRecorded(t *testing.T) {
	const answer = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n01234"
	for _, proven := range []bool{false, true} {
		t.Run(strings.TrimPrefix(switchName[proven], ", "), func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
			c := newRelayedConn(t)
			c.startRelay(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			c.sess.Ctx = ctx
			c.mgr.SetOutputChannel(c.mocks)
			c.sess.RouteMocksViaSyncMock = true
			c.sess.AddPostRecordHook(func(*models.Mock) { cancel() })
			go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, c.sess) }()
			write(t, c.app, c.src, faultReq)
			relayed(t, c.dest, faultReq)
			write(t, c.dest, c.dst, answer)
			relayed(t, c.app, answer)
			var m *models.Mock
			select {
			case m = <-c.mocks:
			case <-time.After(10 * time.Second): // turns a dropped mock into a failure
				t.Fatal("recorded no mock: the exchange read whole was dropped as the context ended")
			}
			if r := m.Spec.HTTPResp; m.Spec.Fault != nil || r == nil || r.Body != "01234" {
				t.Errorf("recorded the response %+v with the fault %v, want it whole, with no fault", r, m.Spec.Fault)
			}
			_ = c.sess.ClientStream.Close()
			_ = c.sess.DestStream.Close()
			if _, err := c.collect(t); err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("recordV2 = %v, want nil or the context's end", err)
			}
			if got := errorsLogged(c); len(got) != 0 {
				t.Errorf("logged %q at ERROR, want nothing", got)
			}
			if got := leftOutReasons(c); len(got) != 0 {
				t.Errorf("left out for %q, want the whole exchange recorded", got)
			}
		})
	}
}

// ctxEndHook is a parser's context whose end the test sets (ended), and which
// runs hook once, on the at'th time Err reports the end, before it reports it:
// an event the test makes happen between two of recordV2's checks. fired says
// whether it ran.
type ctxEndHook struct {
	context.Context
	ended atomic.Bool
	calls atomic.Int32
	at    int32
	hook  func()
	fired atomic.Bool
}

func (c *ctxEndHook) Err() error {
	if !c.ended.Load() {
		return nil
	}
	if c.calls.Add(1) == c.at {
		c.hook()
		c.fired.Store(true)
	}
	return context.Canceled
}

// peerEndRead waits until the relay has read dir's end of c's connection, and
// says whether it did within 10s. Safe off the test's goroutine.
func peerEndRead(c *relayedConn, dir fakeconn.Direction) bool {
	for deadline := time.Now().Add(10 * time.Second); c.sess.PeerEnded(dir) == supervisor.PeerOpen; {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// The recording's stop ends the parser's context partway through the
// response, with neither side's end come, and a side's end (the stop closing
// the application, or the upstream's close) is read after it, before recordV2
// has judged the read: here on its third check of the context after its end
// (readResponseV2's, the one that would have the read take what the stream
// held, and the one that finds the context ended the read). The read is
// judged as it ended: the stop cut it with the connection still open
// (LeftOutStopCutOpen), left out once with its span. Not dropped with nothing
// reported (the client's end), nor said to be cut after the upstream had
// ended the connection (the upstream's). Where the recording is not stopping
// (Session.RecordingStopping), the context's end with the connection open is
// returned as before, nothing left out, whatever end is read after it.
func TestRecordV2_AnEndReadAfterTheContextsEndAtTheStopDoesNotJudgeTheCut(t *testing.T) {
	const part, more = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n01234", "567"
	rows := []struct {
		name               string
		upstream, stopping bool
	}{
		{"the stop closes the application", false, true},
		{"the upstream closes", true, true},
		{"the upstream closes, the recording not stopping", true, false},
	}
	for _, proven := range []bool{false, true} {
		for _, r := range rows {
			upstream := r.upstream
			t.Run(r.name+switchName[proven], func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
				c := newRelayedConn(t)
				c.grace = time.Minute
				c.startRelay(t)
				if r.stopping {
					stopping := make(chan struct{})
					close(stopping)
					c.sess.RecordingDone = stopping
				}
				c.stopBegun.Store(true)
				endRead := true
				// The side's end comes once the application has the part
				// the parser took last.
				moreRelayed := make(chan struct{})
				var relayedOnce sync.Once
				relayedMore := func() { relayedOnce.Do(func() { close(moreRelayed) }) }
				t.Cleanup(relayedMore)
				ctx := &ctxEndHook{Context: context.Background(), at: 3, hook: func() {
					<-moreRelayed
					if upstream {
						_ = c.dest.Close()
						endRead = peerEndRead(c, fakeconn.FromDest)
						return
					}
					_ = c.app.Close()
					endRead = peerEndRead(c, fakeconn.FromClient)
				}}
				go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, c.sess) }()
				write(t, c.app, c.src, faultReq)
				relayed(t, c.dest, faultReq)
				at := cutTimes{sent: time.Now()}
				write(t, c.dest, c.dst, part)
				relayed(t, c.app, part)
				awaitTaken(t, c.sess.DestStream, len(part))
				ctx.ended.Store(true)
				at.last = time.Now()
				write(t, c.dest, c.dst, more)
				relayed(t, c.app, more)
				at.done = time.Now()
				relayedMore()
				mocks, err := c.collect(t)
				if !ctx.fired.Load() || !endRead {
					t.Fatalf("the side's end ran %v, read by the relay %v: recordV2 asked its context fewer than %d times after its end",
						ctx.fired.Load(), endRead, ctx.at)
				}
				if !r.stopping {
					if !errors.Is(err, context.Canceled) || len(mocks) != 0 || len(leftOutReasons(c)) != 0 || len(errorsLogged(c)) != 0 {
						t.Errorf("recordV2 = %v, recorded %d mocks, left out for %q, logged %q at ERROR; want the context's end, and none of them",
							err, len(mocks), leftOutReasons(c), errorsLogged(c))
					}
					return
				}
				wantLeftOutForTheStop(t, c, at, mocks, err, supervisor.LeftOutStopCutOpen, false)
			})
		}
	}
}

// ctxErrHook is a parser's context that does not end, and runs hook once, on
// the first time Err is asked after the test arms it: an event the test makes
// happen between two of recordV2's checks. fired says whether it ran.
type ctxErrHook struct {
	context.Context
	armed atomic.Bool
	hook  func()
	fired atomic.Bool
}

func (c *ctxErrHook) Err() error {
	if c.armed.Load() && c.fired.CompareAndSwap(false, true) {
		c.hook()
	}
	return nil
}

// The supervisor's abort at the recording's stop closes the stream partway
// through the response, with neither side's end come, and a side's end (the
// stop closing the application, or the upstream's close) is read after the
// close, before recordV2 has judged the read: here on its first check of its
// context after the read returned. The read is judged by the ends as it ended
// (Session.Ends, read once): the stop cut it with the connection still open
// (LeftOutStopCutOpen), left out once with its span. Not judged by the end
// that came after the close, as if the client had closed partway through the
// call (LeftOutPartwayAtStop) or the upstream had ended the connection before
// the stop's close (LeftOutStopCutAfterEnd).
func TestRecordV2_AnEndReadAfterTheAbortsCloseDoesNotJudgeTheCut(t *testing.T) {
	const part = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n01234"
	for _, proven := range []bool{false, true} {
		for _, upstream := range []bool{false, true} {
			name := map[bool]string{false: "the stop closes the application", true: "the upstream closes"}[upstream] + switchName[proven]
			t.Run(name, func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
				c := newRelayedConn(t)
				c.grace = time.Minute
				c.startRelay(t)
				stopping := make(chan struct{})
				close(stopping)
				c.sess.RecordingDone = stopping
				c.stopBegun.Store(true)
				endRead := true
				ctx := &ctxErrHook{Context: context.Background(), hook: func() {
					if upstream {
						_ = c.dest.Close()
						endRead = peerEndRead(c, fakeconn.FromDest)
						return
					}
					_ = c.app.Close()
					endRead = peerEndRead(c, fakeconn.FromClient)
				}}
				go func() { c.done <- (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, c.sess) }()
				write(t, c.app, c.src, faultReq)
				relayed(t, c.dest, faultReq)
				at := cutTimes{sent: time.Now()}
				at.last = at.sent
				write(t, c.dest, c.dst, part)
				relayed(t, c.app, part)
				at.done = time.Now()
				awaitTaken(t, c.sess.DestStream, len(part))
				// The parser is blocked for the rest, its check of its
				// context before the read made: the next is after the read.
				for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
					if waiting, _ := c.sess.DestStream.Waiting(); waiting {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the parser did not block for the rest of the response in 10s")
					}
				}
				ctx.armed.Store(true)
				_ = c.sess.DestStream.Close()
				_ = c.sess.ClientStream.Close()
				mocks, err := c.collect(t)
				if !ctx.fired.Load() || !endRead {
					t.Fatalf("the side's end ran %v, read by the relay %v: recordV2 did not ask its context after the read",
						ctx.fired.Load(), endRead)
				}
				wantLeftOutForTheStop(t, c, at, mocks, err, supervisor.LeftOutStopCutOpen, false)
			})
		}
	}
}

// consumedAll waits until the reader of stream has taken n bytes.
func consumedAll(t *testing.T, stream *fakeconn.FakeConn, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); stream.Consumed() != int64(n); {
		if time.Now().After(deadline) {
			t.Fatalf("the parser took %d of %d bytes in 10s", stream.Consumed(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// inStream waits until the next chunk stream hands out is want, without
// taking it. Only while its reader is not reading.
func inStream(t *testing.T, stream *fakeconn.FakeConn, want string) {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		ch, err := stream.Peek()
		if err != nil {
			got <- err.Error()
			return
		}
		got <- string(ch.Bytes)
	}()
	select {
	case g := <-got:
		if g != want {
			t.Fatalf("the stream holds %q next, want %q", g, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%q was not in the stream within 10s", want)
	}
}

// The parser's context ends after the client's end came first, with a part
// of the answer the parser has not taken still in the stream: the read the
// context's end stops takes it, and the exchange's span ends at it, not at
// the part read before. A recorded mock's response time is that part's, and
// so is the end of a left-out exchange's span.
func TestRecordV2_TheReadAfterTheContextsEndSpansWhatTheStreamHeld(t *testing.T) {
	const more, extra = "56789", "abcde"
	rows := []struct {
		name   string
		length int
		why    supervisor.NoFaultEnd
		reason string
	}{
		{"the answer whole, the application gave up", 15, supervisor.EndMayBeFault, ""},
		{"the answer cut, the application gave up", 20, supervisor.EndMayBeFault, ""},
		{"the answer cut, the client's process exited on its own", 20, supervisor.EndAtExit, supervisor.LeftOutPartwayAtExit},
	}
	for _, proven := range []bool{false, true} {
		for _, r := range rows {
			t.Run(r.name+switchName[proven], func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
				part := "HTTP/1.1 200 OK\r\nContent-Length: " + map[int]string{15: "15", 20: "20"}[r.length] + "\r\n\r\n01234"
				c := newRelayedConn(t)
				var extraAt, extraDone time.Time
				want := map[bool]int{false: 1, true: 0}[r.reason != ""]
				_, at, mocks, err := firstEndThenStop(t, c, part, more, theParsersCtx, want, func() {
					// The parser took the next part and holds at its
					// context's check.
					consumedAll(t, c.sess.DestStream, len(part)+len(more))
					extraAt = time.Now()
					write(t, c.dest, c.dst, extra)
					relayed(t, c.app, extra)
					extraDone = time.Now()
					// The relay's tee hands the part to the stream after it
					// forwards it: a look at the stream, while the parser holds
					// at its context's check, waits until it is there, not on
					// its way, as the read after the context's end closes the
					// stream.
					inStream(t, c.sess.DestStream, extra)
					c.clientEnd.Store(uint32(r.why))
					appClosed(t, c)
					awaitPeerEnd(t, c, fakeconn.FromClient)
				})
				if r.reason != "" {
					wantLeftOutForTheStop(t, c, cutTimes{sent: at.sent, last: extraAt, done: extraDone}, mocks, err, r.reason, false)
					return
				}
				if len(mocks) != 1 {
					t.Fatalf("recorded %d mocks (%v), want 1", len(mocks), err)
				}
				if ts := mocks[0].Spec.ResTimestampMock; ts.Before(extraAt) || ts.After(extraDone) {
					t.Errorf("the response's time is %v, want that of its last part, the one the stream held (stamped from %v to %v)", ts, extraAt, extraDone)
				}
				f, resp := mocks[0].Spec.Fault, mocks[0].Spec.HTTPResp
				if r.length == 15 && (f != nil || resp == nil || resp.Body != "0123456789abcde") {
					t.Errorf("recorded the response %+v with the fault %v, want it whole, with no fault", resp, f)
				}
				if r.length == 20 && (f == nil || f.Kind != models.FaultTimeout || !f.AfterHead) {
					t.Errorf("recorded the fault %v, want the application's timeout after the head", f)
				}
			})
		}
	}
}

// The client sends a second request before the first's answer, and gives up
// partway through that answer: the first call is recorded with its timeout
// after the head, and recordV2, its context alive, goes on to the second
// request, which the application gave up on with no answer, and records it
// with its timeout too. Judging the first exchange by the connection's end
// does not end the recording of the connection while the context lasts.
func TestRecordV2_AJudgedExchangeGoesOnToTheNextRequestWhileTheContextLasts(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	for _, proven := range []bool{false, true} {
		t.Run(strings.TrimPrefix(switchName[proven], ", "), func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
			c := newRelayedConn(t)
			c.start(t)
			write(t, c.app, c.src, faultReq)
			relayed(t, c.dest, faultReq)
			write(t, c.app, c.src, faultReq)
			relayed(t, c.dest, faultReq)
			write(t, c.dest, c.dst, head+"01234")
			relayed(t, c.app, head+"01234")
			appClosed(t, c)
			mocks, err := c.finish(t)
			if err != nil || len(mocks) != 2 {
				t.Fatalf("recorded %d mocks, %v; want 2: the call cut partway, then the one sent after it", len(mocks), err)
			}
			wantFault(t, mocks[0], models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5})
			wantFault(t, mocks[1], models.Fault{Kind: models.FaultTimeout})
		})
	}
}

// The supervisor's abort closes the streams with the parser's context ended,
// as when it retires the parser, partway through a close-delimited body, with
// neither side's end and the recording not stopping: no end judges the read,
// so recordV2 returns the context's end, as it does for any read the
// context's end stops with the connection open.
func TestRecordV2_TheAbortsCloseWithNoEndReturnsTheContextsEnd(t *testing.T) {
	part := stopCutAllParts[len(stopCutAllParts)-1].part
	for _, proven := range []bool{false, true} {
		t.Run(strings.TrimPrefix(switchName[proven], ", "), func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[proven])
			c := newRelayedConn(t)
			_, _, _, err := firstEndThenStop(t, c, part, "", theCtxThenTheAbort, 0, func() {})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("recordV2 = %v, want the parser's context's end: no side's end judges the read", err)
			}
			if got := errorsLogged(c); len(got) != 0 {
				t.Errorf("logged %q at ERROR, want nothing", got)
			}
		})
	}
}

// On the direct channel, with the session's context alive, the emit waits for
// a consumer that comes to the channel after it, as EmitMock's does: here the
// channel is full until the consumer takes what it holds, once the parser has
// gone on to wait for the next request, or after 200ms if it has not. Not a
// shutdown's send, which drops what a full channel cannot take.
func TestRecordV2_AMockWaitsForItsConsumerWhileTheContextLasts(t *testing.T) {
	const answer = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	c := newRelayedConn(t)
	c.mocks = make(chan *models.Mock, 1)
	c.mocks <- &models.Mock{Name: "before"}
	c.start(t)
	write(t, c.app, c.src, faultReq)
	relayed(t, c.dest, faultReq)
	write(t, c.dest, c.dst, answer)
	relayed(t, c.app, answer)
	for until := time.Now().Add(200 * time.Millisecond); time.Now().Before(until); time.Sleep(time.Millisecond) {
		if waiting, _ := c.sess.ClientStream.Waiting(); waiting && c.sess.ClientStream.Consumed() == int64(len(faultReq)) {
			break
		}
	}
	if m := <-c.mocks; m.Name != "before" {
		t.Fatalf("the channel held %q, want what it held before the call", m.Name)
	}
	select {
	case m := <-c.mocks:
		if r := m.Spec.HTTPResp; r == nil || r.Body != "ok" {
			t.Errorf("recorded the response %+v, want the answer whole", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call's mock was not delivered to a consumer that came after the emit")
	}
	if _, err := c.finish(t); err != nil {
		t.Errorf("recordV2 = %v, want nil", err)
	}
}

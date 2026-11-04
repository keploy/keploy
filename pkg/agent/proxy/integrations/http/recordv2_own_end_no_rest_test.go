//go:build linux

package http

import (
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
)

// Through the relay, as the proxy records on Linux: the app closes partway
// through a call (its process exits, the recording's stop closes it, or it
// gives up), and the upstream, with nothing sent after the app's FIN, ends
// its side on its own as the app goes: it closes, or it resets. The relay
// refuses nothing, so it gives no word that the app stopped taking the stream
// (Session.EndedWhereClientStopped). Its look at the app's socket as it reads
// the upstream's end, before it forwards that end, finds the app's FIN in
// (Session.ClientEndedBeforeDest): the app ended its side before the
// upstream's end reached it. The call is the app's to judge, by its own end,
// whichever of the two ends the relay reads first, and the two orders record
// the same: left out with that end's reason where it records no fault,
// recorded as far as the app read it (stopped_reading) where the app gave up,
// and, with no byte of the answer read, a timeout. Before, with the upstream's
// end read first, the upstream's end judged it: a truncate or a reset after
// the head, or the upstream's close with no answer (eof), whatever the app's
// own end was, and the recording's stop cut was reported as a decode error.

// endsWithNothingAfterTheAppsFIN runs one call through c: the app sends a
// request, reads read (its first bytes of the answer; none for ""), and
// closes; once the relay has read its FIN, the upstream ends its side with
// nothing sent after it, by a reset where reset is set, else by its close. It
// returns what recordV2 recorded, after checking the relay gave no word that
// the app stopped taking the stream, found the app's end in before the
// upstream's end reached it, and marked the ends in the order asked.
func endsWithNothingAfterTheAppsFIN(t *testing.T, c *relayedConn, appFirst, atStop bool, read string, reset bool) []*models.Mock {
	t.Helper()
	appRead := orderEnds(c, appFirst)
	c.start(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	if read != "" {
		if _, err := c.dest.Write([]byte(read)); err != nil {
			t.Fatal(err)
		}
		relayed(t, c.app, read)
	}
	c.stopBegun.Store(atStop)
	if err := c.app.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-appRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not read the app's FIN within 10s")
	}
	if reset {
		resetConn(t, c.dest)
	} else if err := c.dest.Close(); err != nil {
		t.Fatal(err)
	}
	mocks := c.end(t)
	if c.sess.EndedWhereClientStopped() {
		t.Fatal("the relay said the app stopped taking the stream, though the upstream sent nothing after the app's FIN")
	}
	if !c.sess.ClientEndedBeforeDest() {
		t.Fatal("the relay did not say the app had ended its side before the upstream's end reached it")
	}
	wantEnd := supervisor.PeerClosed
	if reset {
		wantEnd = supervisor.PeerReset
	}
	if got := c.sess.PeerEnded(fakeconn.FromDest); got != wantEnd {
		t.Fatalf("the upstream's end was marked %d, want %d", got, wantEnd)
	}
	want := fakeconn.FromDest
	if appFirst {
		want = fakeconn.FromClient
	}
	if first, ok := c.sess.FirstPeerEnded(); !ok || first != want {
		t.Fatalf("the relay marked side %d's end first (%v), want side %d's", first, ok, want)
	}
	return mocks
}

// partAnswers are answers the app reads the head and 5 body bytes of: framed
// by a length, by chunks, and by the upstream's close.
var partAnswers = []struct {
	name, read, body string
	sent             sentAs
}{
	{name: "a length", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 20\r\n\r\n01234",
		body: "01234", sent: sentAs{length: 20}},
	{name: "chunks", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n14\r\n01234",
		body: "01234", sent: sentChunked},
	{name: "the upstream's close", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n01234",
		body: "01234", sent: sentByClose},
}

// upstreamEnds are the ways the upstream ends its side on its own.
var upstreamEnds = []struct {
	name  string
	reset bool
}{
	{"the upstream closes", false},
	{"the upstream resets", true},
}

func TestRecordV2_ThroughTheRelayTheAppsOwnEndJudgesAnAnswerTheUpstreamEndsOnItsOwnWhicheverEndIsReadFirst(t *testing.T) {
	for _, shape := range partAnswers {
		for _, up := range upstreamEnds {
			for _, end := range ownEnds {
				for _, proven := range []string{"", "1"} {
					t.Run(shape.name+"/"+up.name+"/"+end.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						var first string
						for _, order := range ownEndOrders {
							c := newRelayedConn(t)
							c.clientEnd.Store(uint32(end.client))
							mocks := endsWithNothingAfterTheAppsFIN(t, c, order.appFirst, end.atStop, shape.read, up.reset)
							if end.leftOut != "" {
								if len(mocks) != 0 {
									t.Fatalf("%s: recorded %d mocks (fault %+v), want none: the app's own end records no fault, and what it read is no whole answer",
										order.name, len(mocks), mocks[0].Spec.Fault)
								}
								if got := c.leftOut(end.leftOut); len(got) != 1 {
									t.Fatalf("%s: said %d times that the exchange was left out under %q, want once", order.name, len(got), end.leftOut)
								}
								continue
							}
							if len(mocks) != 1 {
								t.Fatalf("%s: recorded %d mocks, want the one exchange, as far as the app read it", order.name, len(mocks))
							}
							wantStoppedReading(t, mocks[0], shape.body, shape.sent)
							got := mockShape(t, mocks[0])
							if first == "" {
								first = got
								continue
							}
							if got != first {
								t.Fatalf("%s: recorded\n%s\nwant the mock recorded with the app's end read first:\n%s", order.name, got, first)
							}
						}
					})
				}
			}
		}
	}
}

// The same with no byte of the answer read: the app closed before the
// upstream answered, and the upstream ended its side with no answer. An app
// that gave up has the call recorded as a timeout, either order, never as the
// upstream's close or reset with no answer; one whose process exited, or that
// the stop closed, records nothing.
func TestRecordV2_ThroughTheRelayTheAppsOwnEndJudgesACallTheUpstreamEndsUnansweredWhicheverEndIsReadFirst(t *testing.T) {
	for _, up := range upstreamEnds {
		for _, end := range ownEnds {
			for _, proven := range []string{"", "1"} {
				t.Run(up.name+"/"+end.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
					t.Setenv("KEPLOY_PROVEN", proven)
					for _, order := range ownEndOrders {
						c := newRelayedConn(t)
						c.clientEnd.Store(uint32(end.client))
						mocks := endsWithNothingAfterTheAppsFIN(t, c, order.appFirst, end.atStop, "", up.reset)
						if end.leftOut != "" {
							if len(mocks) != 0 {
								t.Fatalf("%s: recorded %d mocks (fault %+v), want none: the app's own end records no fault",
									order.name, len(mocks), mocks[0].Spec.Fault)
							}
							continue
						}
						if len(mocks) != 1 {
							t.Fatalf("%s: recorded %d mocks, want the call the app gave up on", order.name, len(mocks))
						}
						wantFault(t, mocks[0], models.Fault{Kind: models.FaultTimeout})
					}
				})
			}
		}
	}
}

// holdTheUpstreamsEnd makes the relay hold the upstream's end mark (endHook)
// until it has read the app's FIN, and then mark the two ends in the order
// asked. It returns a channel closed once the relay has read the upstream's
// end: that end has not reached the app yet, as the relay forwards it only
// once it is marked. A hook that waits longer than 10s marks its own, and the
// order the test asserts afterwards fails.
func holdTheUpstreamsEnd(c *relayedConn, appFirst bool) (destRead <-chan struct{}) {
	dr, appRead, appMarked, destMarked := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	await := func(ch <-chan struct{}) {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
		}
	}
	c.endHook = func(dir fakeconn.Direction, mark func()) {
		if dir == fakeconn.FromDest {
			close(dr)
			await(appRead)
			if appFirst {
				await(appMarked)
			}
			mark()
			close(destMarked)
			return
		}
		close(appRead)
		if !appFirst {
			await(destMarked)
		}
		mark()
		close(appMarked)
	}
	return dr
}

// Through the relay: the upstream closes partway through the answer first,
// and the relay has read its close but not yet forwarded it when the app
// closes (it gave up, or its process exited, with the upstream's end still
// not at its socket). The app ended its side before the upstream's end reached
// it, so its own end judges the call, whichever of the two ends the relay
// marks first: the look the relay takes once the upstream's end is marked,
// and before it forwards it, finds the app's FIN in. A look taken before the
// mark would find the app still open, and the upstream's end marked first
// would then judge the call.
func TestRecordV2_ThroughTheRelayAnAppThatClosedBeforeTheUpstreamsEndReachedItJudgesTheCall(t *testing.T) {
	shape := partAnswers[0]
	for _, end := range ownEnds {
		if end.atStop {
			continue // the stop's end records no fault on either side
		}
		for _, proven := range []string{"", "1"} {
			t.Run(end.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				for _, order := range ownEndOrders {
					c := newRelayedConn(t)
					c.clientEnd.Store(uint32(end.client))
					destRead := holdTheUpstreamsEnd(c, order.appFirst)
					c.start(t)
					write(t, c.app, c.src, jdkRequest(7))
					relayed(t, c.dest, jdkRequest(7))
					if _, err := c.dest.Write([]byte(shape.read)); err != nil {
						t.Fatal(err)
					}
					relayed(t, c.app, shape.read)
					if err := c.dest.Close(); err != nil {
						t.Fatal(err)
					}
					select {
					case <-destRead:
					case <-time.After(10 * time.Second):
						t.Fatal("the relay did not read the upstream's close within 10s")
					}
					if err := c.app.Close(); err != nil {
						t.Fatal(err)
					}
					mocks := c.end(t)
					want := fakeconn.FromDest
					if order.appFirst {
						want = fakeconn.FromClient
					}
					if first, ok := c.sess.FirstPeerEnded(); !ok || first != want {
						t.Fatalf("%s: the relay marked side %d's end first (%v), want side %d's", order.name, first, ok, want)
					}
					if end.leftOut != "" {
						if len(mocks) != 0 {
							t.Fatalf("%s: recorded %d mocks (fault %+v), want none: the app's own end records no fault",
								order.name, len(mocks), mocks[0].Spec.Fault)
						}
						if got := c.leftOut(end.leftOut); len(got) != 1 {
							t.Fatalf("%s: said %d times that the exchange was left out under %q, want once", order.name, len(got), end.leftOut)
						}
						continue
					}
					if len(mocks) != 1 {
						t.Fatalf("%s: recorded %d mocks, want the one exchange, as far as the app read it", order.name, len(mocks))
					}
					wantStoppedReading(t, mocks[0], shape.body, shape.sent)
				}
			})
		}
	}
}

// Through the relay: the upstream ends its side partway through the answer,
// the relay forwards that end, and the app reads it (its read of the answer
// ends short, or is reset) and only then closes, giving up or exiting. The
// app had the upstream's end before it ended its own: the upstream's end
// judges the call, as before, a truncate or a reset, and the relay does not
// say the app ended first. A body the upstream's close frames is whole at
// that close: recorded whole, with no fault.
func TestRecordV2_ThroughTheRelayAnAppThatClosedOnceItHadTheUpstreamsEndIsJudgedByTheUpstream(t *testing.T) {
	for _, shape := range partAnswers {
		for _, up := range upstreamEnds {
			for _, end := range ownEnds {
				if end.atStop {
					continue
				}
				for _, proven := range []string{"", "1"} {
					t.Run(shape.name+"/"+up.name+"/"+end.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						c := newRelayedConn(t)
						c.clientEnd.Store(uint32(end.client))
						c.start(t)
						write(t, c.app, c.src, jdkRequest(7))
						relayed(t, c.dest, jdkRequest(7))
						if _, err := c.dest.Write([]byte(shape.read)); err != nil {
							t.Fatal(err)
						}
						relayed(t, c.app, shape.read)
						if up.reset {
							resetConn(t, c.dest)
						} else if err := c.dest.Close(); err != nil {
							t.Fatal(err)
						}
						_ = c.app.SetReadDeadline(time.Now().Add(10 * time.Second))
						n, err := c.app.Read(make([]byte, 1))
						if wantEOF := !up.reset; n != 0 || wantEOF && !errors.Is(err, io.EOF) || !wantEOF && !errors.Is(err, syscall.ECONNRESET) {
							t.Fatalf("the app read %d byte(s), %v, after the upstream's end; want its end (io.EOF for a close, ECONNRESET for a reset)", n, err)
						}
						if err := c.app.Close(); err != nil {
							t.Fatal(err)
						}
						mocks := c.end(t)
						if c.sess.ClientEndedBeforeDest() {
							t.Fatal("the relay said the app had ended its side before the upstream's end reached it, though the app read that end first")
						}
						if len(mocks) != 1 {
							t.Fatalf("recorded %d mocks, want the call the upstream cut", len(mocks))
						}
						f := mocks[0].Spec.Fault
						switch {
						case up.reset:
							if f == nil || f.Kind != models.FaultReset {
								t.Fatalf("recorded fault %+v, want the upstream's reset", f)
							}
						case shape.sent == sentByClose:
							if f != nil || mocks[0].Spec.HTTPResp.Body != shape.body {
								t.Fatalf("recorded fault %+v and body %q, want the whole answer its close framed, %q, with no fault", f, mocks[0].Spec.HTTPResp.Body, shape.body)
							}
						default:
							if f == nil || f.Kind != models.FaultTruncate {
								t.Fatalf("recorded fault %+v, want the upstream's truncate", f)
							}
						}
					})
				}
			}
		}
	}
}
